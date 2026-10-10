//go:build linux

package disk

import (
	"os"
	"syscall"
)

// statIdentity extracts the change time, inode, and device of a file. Unlike
// mtime, ctime cannot be set from userspace, so restoring an old mtime after
// an in-place rewrite still moves it.
func statIdentity(info os.FileInfo) (ctimeNano int64, ino, dev uint64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0
	}
	return int64(st.Ctim.Sec)*1e9 + int64(st.Ctim.Nsec), uint64(st.Ino), uint64(st.Dev) //nolint:unconvert
}
