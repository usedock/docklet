// Command dockyard-index writes dockyard.json for the files CI built.
// Usage: dockyard-index -catalog images/catalog.toml -dir dist -version TAG -base-url URL
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/usedock/docklet/internal/dockyard"
)

func main() {
	catalog := flag.String("catalog", "images/catalog.toml", "catalog file")
	dir := flag.String("dir", "dist", "directory holding the built files")
	version := flag.String("version", "", "release tag")
	baseURL := flag.String("base-url", "", "download URL prefix")
	flag.Parse()
	if err := run(*catalog, *dir, *version, *baseURL); err != nil {
		fmt.Fprintln(os.Stderr, "dockyard-index:", err)
		os.Exit(1)
	}
}

func run(catalog, dir, version, baseURL string) error {
	c, err := dockyard.LoadCatalog(catalog)
	if err != nil {
		return err
	}
	ix, err := c.BuildIndex(version, baseURL, dir)
	if err != nil {
		return err
	}
	out, err := ix.Marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "dockyard.json"), out, 0o644)
}
