package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// sizeValue accepts a TOML string ("8gb") or integer (plain unit).
type sizeValue struct{ raw string }

// UnmarshalTOML implements toml.Unmarshaler.
func (s *sizeValue) UnmarshalTOML(v any) error {
	switch t := v.(type) {
	case string:
		s.raw = t
	case int64:
		s.raw = strconv.FormatInt(t, 10)
	default:
		return fmt.Errorf("must be a string like \"8gb\" or an integer")
	}
	return nil
}

// dockFile is the dock.toml schema.
type dockFile struct {
	Name        string    `toml:"name"`
	Image       string    `toml:"image"`
	CPUs        int       `toml:"cpus"`
	Memory      sizeValue `toml:"memory"`
	Disk        sizeValue `toml:"disk"`
	Environment string    `toml:"environment"`
}

// dockSpec is the merged size and placement request for new and branch.
type dockSpec struct {
	Name, Image, Environment string
	CPUs, MemoryMiB, DiskGiB int
}

// readDockFile loads dir/dock.toml; ok is false when it does not exist.
func readDockFile(dir string) (dockSpec, bool, error) {
	path := filepath.Join(dir, "dock.toml")
	var df dockFile
	md, err := toml.DecodeFile(path, &df)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dockSpec{}, false, nil
		}
		return dockSpec{}, false, newError("invalid_dock_toml", fmt.Sprintf("%s: %v", path, err))
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return dockSpec{}, false, newError("invalid_dock_toml", fmt.Sprintf("%s: unknown keys: %s", path, strings.Join(keys, ", ")))
	}
	spec := dockSpec{Name: df.Name, Image: df.Image, Environment: df.Environment, CPUs: df.CPUs}
	if spec.CPUs < 0 {
		return dockSpec{}, false, newError("invalid_dock_toml", fmt.Sprintf("%s: cpus must be positive", path))
	}
	if df.Memory.raw != "" {
		if spec.MemoryMiB, err = ParseMemoryMiB(df.Memory.raw); err != nil {
			return dockSpec{}, false, newError("invalid_dock_toml", fmt.Sprintf("%s: memory: %v", path, err))
		}
	}
	if df.Disk.raw != "" {
		if spec.DiskGiB, err = ParseDiskGiB(df.Disk.raw); err != nil {
			return dockSpec{}, false, newError("invalid_dock_toml", fmt.Sprintf("%s: disk: %v", path, err))
		}
	}
	return spec, true, nil
}

// body renders the spec as an API request body, omitting unset fields.
func (s dockSpec) body() map[string]any {
	b := map[string]any{}
	if s.Name != "" {
		b["name"] = s.Name
	}
	if s.Image != "" {
		b["image"] = s.Image
	}
	if s.Environment != "" {
		b["environment"] = s.Environment
	}
	if s.CPUs > 0 {
		b["cpus"] = s.CPUs
	}
	if s.MemoryMiB > 0 {
		b["memoryMiB"] = s.MemoryMiB
	}
	if s.DiskGiB > 0 {
		b["diskGiB"] = s.DiskGiB
	}
	return b
}
