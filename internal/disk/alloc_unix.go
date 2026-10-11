//go:build !windows

package disk

import "syscall"

// AllocatedBytes is the space a file takes on disk (its allocated blocks), or 0 when it cannot
// be read. A sparse file counts only what was written.
func AllocatedBytes(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return int64(st.Blocks) * 512
}
