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
