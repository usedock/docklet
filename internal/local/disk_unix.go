//go:build !windows

package local

import "golang.org/x/sys/unix"

// freeBytes is the space available to an unprivileged user under dir.
func freeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// sparseTemp reports that qemu-img writes a flattened raw disk sparse here, so it takes only
// the space of its data.
const sparseTemp = true

// allocatedBytes is the space a file takes on disk (its allocated blocks), or 0.
func allocatedBytes(path string) int64 {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0
	}
	return int64(st.Blocks) * 512
}
