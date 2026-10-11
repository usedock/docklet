// Package dockyard describes Dockyard, the signed catalog of ready-made dock
// images published as release assets: the dockyard.json index the local engine
// downloads, and the checks that make a downloaded file trustworthy.
package dockyard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Well-known keys of Image.Files.
const (
	FileRootfs = "rootfs.ext4.zst"
	FileKernel = "vmlinuz"
	FileInitrd = "initrd.img"
)

// Index is the dockyard.json document.
type Index struct {
	Version string  `json:"version"`
	Images  []Image `json:"images"`
}

// Image is one image for one architecture. Files is keyed by role (see the
// File* constants).
type Image struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Arch        string          `json:"arch"`
	Files       map[string]File `json:"files"`
}

// File is one downloadable asset with its expected size and lowercase hex
// SHA-256.
type File struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ParseIndex decodes and validates a dockyard.json document.
func ParseIndex(data []byte) (*Index, error) {
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return nil, fmt.Errorf("dockyard: parse index: %w", err)
	}
	if ix.Version == "" {
		return nil, errors.New("dockyard: index has no version")
	}
	seen := map[string]bool{}
	for _, img := range ix.Images {
		if img.Name == "" || img.Arch == "" {
			return nil, errors.New("dockyard: image needs a name and an arch")
		}
		key := img.Name + "/" + img.Arch
		if seen[key] {
			return nil, fmt.Errorf("dockyard: duplicate image %s", key)
		}
		seen[key] = true
		for role, f := range img.Files {
			if f.URL == "" || f.Size < 0 || !validSHA256(f.SHA256) {
				return nil, fmt.Errorf("dockyard: image %s file %s is malformed", key, role)
			}
		}
	}
	return &ix, nil
}

// Marshal renders the index as indented JSON with a trailing newline.
func (ix *Index) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Find returns the image with the given name and arch.
func (ix *Index) Find(name, arch string) (*Image, bool) {
	for i := range ix.Images {
		if ix.Images[i].Name == name && ix.Images[i].Arch == arch {
			return &ix.Images[i], true
		}
	}
	return nil, false
}

// VerifyFile checks that the file at path has exactly f.Size bytes and the
// SHA-256 f.SHA256.
func VerifyFile(path string, f File) error {
	got, err := describeFile(path, f.URL)
	if err != nil {
		return err
	}
	if got.Size != f.Size {
		return fmt.Errorf("dockyard: %s is %d bytes, want %d", path, got.Size, f.Size)
	}
	if got.SHA256 != strings.ToLower(f.SHA256) {
		return fmt.Errorf("dockyard: %s sha256 is %s, want %s", path, got.SHA256, f.SHA256)
	}
	return nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func describeFile(path, url string) (File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer fh.Close()
	h := sha256.New()
	n, err := io.Copy(h, fh)
	if err != nil {
		return File{}, err
	}
	return File{URL: url, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
