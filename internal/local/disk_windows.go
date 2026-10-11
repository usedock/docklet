//go:build windows

package local

import (
	"os"

	"golang.org/x/sys/windows"
)

// freeBytes is the space available to the current user on dir's volume.
func freeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// sparseTemp is false: the flattened raw disk is written at its full size here (docklet#76).
const sparseTemp = false

// allocatedBytes is the size of a file, or 0.
func allocatedBytes(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
