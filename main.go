package main

import (
	"errors"
	"fmt"
	"go/build"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	cli "github.com/codegangsta/cli"
	rw "github.com/whyrusleeping/gx-go-tool/rewrite"
	gx "github.com/whyrusleeping/gx/gxutil"
)

func main() {
	app := cli.NewApp()
	app.Name = "gx-go-tool"
	app.Author = "whyrusleeping"
	app.Version = "0.2.0"

	var UpdateCommand = cli.Command{
		Name:      "update",
		Usage:     "update a packages imports to a new path",
		ArgsUsage: "[old import] [new import]",
		Action: func(c *cli.Context) {
			if len(c.Args()) < 2 {
				fmt.Println("must specify current and new import names")
				return
			}

			oldimp := c.Args()[0]
			newimp := c.Args()[1]

			curpath, err := os.Getwd()
			if err != nil {
				fmt.Println("error getting working dir: ", err)
				return
			}

			rwf := func(in string) string {
				if in == oldimp {
					return newimp
				}
				return in
			}

			filter := func(in string) bool {
				return strings.HasSuffix(in, ".go")
			}

			err = rw.RewriteImports(curpath, rwf, filter)
			if err != nil {
				fmt.Println(err)
				return
			}
		},
	}

	var ImportCommand = cli.Command{
		Name:  "import",
		Usage: "import a go package and all its depencies into gx",
		Flags: []cli.Flag{
			cli.BoolFlag{
				Name:  "rewrite",
				Usage: "rewrite import paths to use vendored packages",
			},
		},
		Action: func(c *cli.Context) {
			importer, err := NewImporter(c.Bool("rewrite"))
			if err != nil {
				fmt.Println(err)
				return
			}

			if !c.Args().Present() {
				fmt.Println("must specify a package name")
				return
			}

			pkg := c.Args().First()
			fmt.Printf("vendoring package %s\n", pkg)

			_, err = importer.GxPublishGoPackage(pkg)
			if err != nil {
				log.Println(err)
				return
			}
		},
	}

	var PathCommand = cli.Command{
		Name:  "path",
		Usage: "prints the import path of the current package within GOPATH",
		Action: func(c *cli.Context) {
			gopath := os.Getenv("GOPATH")
			if gopath == "" {
				fmt.Println("GOPATH not set, cannot derive import path")
				return
			}

			cwd, err := os.Getwd()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			srcdir := path.Join(gopath, "src")
			srcdir += "/"

			if !strings.HasPrefix(cwd, srcdir) {
				fmt.Println("package not within GOPATH/src")
				os.Exit(1)
			}

			rel := cwd[len(srcdir):]
			fmt.Println(rel)
		},
	}

	app.Commands = []cli.Command{
		UpdateCommand,
		ImportCommand,
		PathCommand,
	}

	app.Run(os.Args)
}

func pathIsNotStdlib(path string) bool {
	first := strings.Split(path, "/")[0]

	if len(strings.Split(first, ".")) > 1 {
		return true
	}
	return false
}

type Importer struct {
	pkgs    map[string]*gx.Dependency
	gopath  string
	pm      *gx.PM
	rewrite bool
}

func NewImporter(rw bool) (*Importer, error) {
	gp, err := getGoPath()
	if err != nil {
		return nil, err
	}

	return &Importer{
		pkgs:    make(map[string]*gx.Dependency),
		gopath:  gp,
		pm:      gx.NewPM(),
		rewrite: rw,
	}, nil
}

func getGoPath() (string, error) {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		return "", errors.New("gopath not set")
	}
	return gopath, nil
}

func (i *Importer) GxPublishGoPackage(imppath string) (*gx.Dependency, error) {
	if d, ok := i.pkgs[imppath]; ok {
		return d, nil
	}

	// make sure its local
	err := GoGet(imppath)
	if err != nil {
		return nil, err
	}

	pkgpath := path.Join(i.gopath, "src", imppath)
	pkgFilePath := path.Join(pkgpath, gx.PkgFileName)
	pkg, err := gx.LoadPackageFile(pkgFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		// init as gx package
		parts := strings.Split(imppath, "/")
		pkgname := parts[len(parts)-1]
		err = gx.InitPkg(pkgpath, pkgname, "go")
		if err != nil {
			return nil, err
		}

		pkg, err = gx.LoadPackageFile(pkgFilePath)
		if err != nil {
			return nil, err
		}
	}

	// recurse!
	gopkg, err := build.Import(imppath, "", 0)
	if err != nil {
		return nil, err
	}

	var depsToVendor []string

	for _, child := range gopkg.Imports {
		if pathIsNotStdlib(child) {
			depsToVendor = append(depsToVendor, child)
		}
	}

	for n, child := range depsToVendor {
		fmt.Printf("- processing dep %s for %s [%d / %d]\n", child, imppath, n+1, len(depsToVendor))
		childdep, err := i.GxPublishGoPackage(child)
		if err != nil {
			return nil, err
		}

		pkg.Dependencies = append(pkg.Dependencies, childdep)
	}

	err = gx.SavePackageFile(pkg, pkgFilePath)
	if err != nil {
		return nil, err
	}

	if i.rewrite {
		fullpkgpath, err := filepath.Abs(pkgpath)
		if err != nil {
			return nil, err
		}

		err = i.rewriteImports(fullpkgpath)
		if err != nil {
			return nil, err
		}
	}

	hash, err := i.pm.PublishPackage(pkgpath, pkg)
	if err != nil {
		return nil, err
	}

	fmt.Printf("published %s as %s\n", imppath, hash)

	dep := &gx.Dependency{
		Hash: hash,
		Name: pkg.Name,
	}
	i.pkgs[imppath] = dep
	return dep, nil
}

func (i *Importer) rewriteImports(pkgpath string) error {

	filter := func(p string) bool {
		return !strings.HasPrefix(p, "vendor") &&
			!strings.HasPrefix(p, ".git") &&
			strings.HasSuffix(p, ".go")
	}

	rwf := func(in string) string {
		dep, ok := i.pkgs[in]
		if !ok {
			return in
		}

		return dep.Hash + "/" + dep.Name
	}

	return rw.RewriteImports(pkgpath, rwf, filter)
}

// TODO: take an option to grab packages from local GOPATH
func GoGet(path string) error {
	out, err := exec.Command("go", "get", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("go get failed: %s - %s", string(out), err)
	}
	return nil
}
