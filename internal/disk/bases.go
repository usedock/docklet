package disk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	dockexec "github.com/usedock/docklet/internal/exec"
)

// Golden images are content-addressed: every base a host holds lives at
// <images>/bases/<fingerprint>/base.ext4 and is never rewritten or removed by
// the docklet, while <images>/base.ext4 is a symlink naming the current one.
// A delta generation is rebuilt onto exactly the base it names, so replacing
// the current image never strands an older generation.

// BasesDir is the directory under the image dir that holds every base.
const BasesDir = "bases"

const baseFile = "base.ext4"

// baseLabelSuffix names the file beside a dock rootfs recording its base.
const baseLabelSuffix = ".base"

// ErrBaseUnavailable means the base a delta generation needs is not on this host.
var ErrBaseUnavailable = errors.New("base image unavailable")

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidFingerprint reports whether fp is a lowercase hex sha256 digest.
func ValidFingerprint(fp string) bool {
	return fingerprintPattern.MatchString(fp)
}

// BasePath is where the base with fingerprint fp lives beside pointer.
func BasePath(pointer, fp string) string {
	return filepath.Join(filepath.Dir(pointer), BasesDir, fp, baseFile)
}

// addressedFingerprint returns the fingerprint a bases/<fp>/base.ext4 path names.
func addressedFingerprint(path string) (string, bool) {
	dir := filepath.Dir(path)
	fp := filepath.Base(dir)
	if filepath.Base(path) != baseFile || filepath.Base(filepath.Dir(dir)) != BasesDir || !ValidFingerprint(fp) {
		return "", false
	}
	return fp, true
}

// CurrentBase resolves pointer to the file new docks are provisioned from.
// fp is the fingerprint its address names, empty for a legacy plain file.
func CurrentBase(pointer string) (path, fp string, err error) {
	path, err = filepath.EvalSymlinks(pointer)
	if err != nil {
		return "", "", fmt.Errorf("resolve current base %s: %w", pointer, err)
	}
	fp, _ = addressedFingerprint(path)
	return path, fp, nil
}

// FindBase locates the local base whose fingerprint is fp.
func FindBase(pointer, fp string) (string, error) {
	if !ValidFingerprint(fp) {
		return "", fmt.Errorf("%w: invalid base fingerprint %q", ErrBaseUnavailable, fp)
	}
	path := BasePath(pointer, fp)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path, nil
	}
	// A host that has not migrated yet still holds its one base as a plain file.
	if cur, addressed, err := CurrentBase(pointer); err == nil && addressed == "" {
		if got, err := Fingerprint(cur); err == nil && got == fp {
			return cur, nil
		}
	}
	return "", fmt.Errorf("%w: base image %s is not on this host", ErrBaseUnavailable, fp)
}

// ListBases returns the fingerprints of every base held beside pointer,
// including a legacy plain current base on a host whose migration failed.
func ListBases(pointer string) ([]string, error) {
	fps := []string{}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(pointer), BasesDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidFingerprint(e.Name()) {
			continue
		}
		if info, err := os.Stat(BasePath(pointer, e.Name())); err == nil && info.Mode().IsRegular() {
			fps = append(fps, e.Name())
		}
	}
	if cur, addressed, err := CurrentBase(pointer); err == nil && addressed == "" {
		if fp, err := Fingerprint(cur); err == nil && !slices.Contains(fps, fp) {
			fps = append(fps, fp)
		}
	}
	sort.Strings(fps)
	return fps, nil
}

// SetCurrentBase atomically repoints pointer at the held base fp.
func SetCurrentBase(pointer, fp string) error {
	if _, err := FindBase(pointer, fp); err != nil {
		return err
	}
	tmp := pointer + ".next"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join(BasesDir, fp, baseFile), tmp); err != nil {
		return fmt.Errorf("point %s at base %s: %w", pointer, fp, err)
	}
	if err := os.Rename(tmp, pointer); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("point %s at base %s: %w", pointer, fp, err)
	}
	return nil
}

// ProvisionFromBase copies the current base to dest and labels dest with the
// base it derives from, so its captures diff against that same base later.
func ProvisionFromBase(run dockexec.Runner, pointer, dest string) error {
	path, fp, err := CurrentBase(pointer)
	if err != nil {
		return err
	}
	if err := Provision(run, path, dest); err != nil {
		return err
	}
	if fp == "" {
		return nil
	}
	return LabelBase(dest, fp)
}

// LabelBase records the base a dock rootfs derives from.
func LabelBase(rootfs, fp string) error {
	return os.WriteFile(rootfs+baseLabelSuffix, []byte(fp+"\n"), 0o644)
}

// unknownBase labels a disk restored whole from an image this host does not hold.
const unknownBase = "unknown"

// LabelUnknownBase records that a dock rootfs derives from no base this host
// holds, so it is never reported as the current base's image.
func LabelUnknownBase(rootfs string) error {
	return os.WriteFile(rootfs+baseLabelSuffix, []byte(unknownBase+"\n"), 0o644)
}

// BaseLabel returns the base a dock rootfs derives from, empty when unknown.
func BaseLabel(rootfs string) string {
	raw, err := os.ReadFile(rootfs + baseLabelSuffix)
	if err != nil {
		return ""
	}
	fp := strings.TrimSpace(string(raw))
	if !ValidFingerprint(fp) {
		return ""
	}
	return fp
}

// sourceFile records, inside bases/<fp>/, the Dockyard image the base came
// from. Host-built bases have none.
const sourceFile = "source.json"

type baseSource struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// RecordBaseSource notes that the held base fp is the Dockyard image name, so
// a dock derived from it can report which image it runs.
func RecordBaseSource(pointer, fp, name, version string) error {
	if !ValidFingerprint(fp) {
		return fmt.Errorf("record base source: invalid fingerprint %q", fp)
	}
	raw, err := json.Marshal(baseSource{Name: name, Version: version})
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(BasePath(pointer, fp)), sourceFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BaseSourceName names the Dockyard image the held base fp was pulled as, or ""
// for a host-built base. Bases pulled before sources were recorded are matched
// through the images/<name>/image.json records instead.
func BaseSourceName(pointer, fp string) string {
	if !ValidFingerprint(fp) {
		return ""
	}
	if raw, err := os.ReadFile(filepath.Join(filepath.Dir(BasePath(pointer, fp)), sourceFile)); err == nil {
		var src baseSource
		if json.Unmarshal(raw, &src) == nil && src.Name != "" {
			return src.Name
		}
	}
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(pointer), "images"))
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(pointer), "images", e.Name(), "image.json"))
		if err != nil {
			continue
		}
		var rec struct {
			Name        string `json:"name"`
			Fingerprint string `json:"fingerprint"`
		}
		if json.Unmarshal(raw, &rec) == nil && rec.Fingerprint == fp && rec.Name != "" {
			return rec.Name
		}
	}
	return ""
}

// ImageFingerprint returns the held base the Dockyard image name resolves to
// on this host: its own pointer when pulled under that name, else the current
// base when that is the image. Empty when unknown.
func ImageFingerprint(pointer, name string) string {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return ""
	}
	if path, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(pointer), "images", name, baseFile)); err == nil {
		if fp, ok := addressedFingerprint(path); ok {
			return fp
		}
	}
	if _, fp, err := CurrentBase(pointer); err == nil && fp != "" && BaseSourceName(pointer, fp) == name {
		return fp
	}
	return ""
}

// DockImage names the Dockyard image a dock rootfs derives from: through its
// base label while the host holds it, else the current base. Empty for a
// host-built base, a disk labelled unknown, or an unknown base.
func DockImage(pointer, rootfs string) string {
	fp := BaseLabel(rootfs)
	if fp == "" {
		if raw, err := os.ReadFile(rootfs + baseLabelSuffix); err == nil && strings.TrimSpace(string(raw)) == unknownBase {
			return ""
		}
		var err error
		if _, fp, err = CurrentBase(pointer); err != nil {
			return ""
		}
	}
	return BaseSourceName(pointer, fp)
}
