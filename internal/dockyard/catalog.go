package dockyard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	archRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
)

// Catalog is images/catalog.toml: what CI builds.
type Catalog struct {
	Images []Entry `toml:"image"`
}

// Entry is one image to build. Base is empty for a root image (debootstrap) or
// names another entry; Setup is a script under images/ run in the rootfs.
type Entry struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Base        string   `toml:"base"`
	Setup       string   `toml:"setup"`
	SizeMiB     int      `toml:"size_mib"`
	Arch        []string `toml:"arch"`
}

// ParseCatalog decodes and validates catalog.toml.
func ParseCatalog(data []byte) (*Catalog, error) {
	var c Catalog
	if _, err := toml.Decode(string(data), &c); err != nil {
		return nil, fmt.Errorf("dockyard: parse catalog: %w", err)
	}
	if len(c.Images) == 0 {
		return nil, errors.New("dockyard: catalog has no images")
	}
	byName := map[string]*Entry{}
	for i := range c.Images {
		e := &c.Images[i]
		if !nameRe.MatchString(e.Name) {
			return nil, fmt.Errorf("dockyard: bad image name %q", e.Name)
		}
		if byName[e.Name] != nil {
			return nil, fmt.Errorf("dockyard: duplicate image %q", e.Name)
		}
		if e.SizeMiB <= 0 || len(e.Arch) == 0 {
			return nil, fmt.Errorf("dockyard: image %q needs size_mib and arch", e.Name)
		}
		for _, a := range e.Arch {
			if !archRe.MatchString(a) {
				return nil, fmt.Errorf("dockyard: image %q has bad arch %q", e.Name, a)
			}
		}
		if e.Setup != "" && filepath.Base(e.Setup) != e.Setup {
			return nil, fmt.Errorf("dockyard: image %q setup must be a file name under images/", e.Name)
		}
		byName[e.Name] = e
	}
	for _, e := range c.Images {
		if e.Base == "" {
			continue
		}
		if byName[e.Base] == nil || e.Base == e.Name {
			return nil, fmt.Errorf("dockyard: image %q has unknown base %q", e.Name, e.Base)
		}
	}
	return &c, nil
}

// LoadCatalog reads and parses a catalog file.
func LoadCatalog(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCatalog(b)
}

// fileSuffixes maps an Image.Files role to its release asset suffix.
var fileSuffixes = map[string]string{
	FileRootfs: "rootfs.ext4.zst",
	FileKernel: "vmlinuz",
	FileInitrd: "initrd.img",
}

// BuildIndex hashes the built files in dir and returns the index. Each image
// and arch contributes dir/<name>-<arch>.rootfs.ext4.zst, <name>-<arch>.vmlinuz
// and <name>-<arch>.initrd.img; URLs are baseURL + "/" + file name.
func (c *Catalog) BuildIndex(version, baseURL, dir string) (*Index, error) {
	ix := &Index{Version: version}
	for _, e := range c.Images {
		for _, arch := range e.Arch {
			img := Image{Name: e.Name, Description: e.Description, Arch: arch, Files: map[string]File{}}
			for role, suffix := range fileSuffixes {
				name := fmt.Sprintf("%s-%s.%s", e.Name, arch, suffix)
				f, err := describeFile(filepath.Join(dir, name), baseURL+"/"+name)
				if err != nil {
					return nil, err
				}
				img.Files[role] = f
			}
			ix.Images = append(ix.Images, img)
		}
	}
	return ix, nil
}
