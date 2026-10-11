package local

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataDir returns the per-user directory that holds docks and cached images:
// $DOCK_DATA_DIR, else $XDG_DATA_HOME/dock (Linux), ~/Library/Application Support/dock
// (macOS) or %LOCALAPPDATA%\dock (Windows).
func DataDir(getenv func(string) string) (string, error) {
	home, _ := os.UserHomeDir()
	return dataDirFor(runtime.GOOS, getenv, home)
}

func dataDirFor(goos string, getenv func(string) string, home string) (string, error) {
	if v := getenv("DOCK_DATA_DIR"); v != "" {
		return v, nil
	}
	switch goos {
	case "windows":
		if v := getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "dock"), nil
		}
		if home != "" {
			return filepath.Join(home, "AppData", "Local", "dock"), nil
		}
	case "darwin":
		if home != "" {
			return filepath.Join(home, "Library", "Application Support", "dock"), nil
		}
	default:
		if v := getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "dock"), nil
		}
		if home != "" {
			return filepath.Join(home, ".local", "share", "dock"), nil
		}
	}
	return "", errf("data_dir_unknown", "cannot find a user data directory; set DOCK_DATA_DIR")
}
