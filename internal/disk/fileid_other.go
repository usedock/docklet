//go:build !linux

package disk

import "os"

// statIdentity has no portable ctime or inode off linux; the racy-timestamp
// guard in Fingerprint still keeps the cache correct there.
func statIdentity(info os.FileInfo) (ctimeNano int64, ino, dev uint64) {
	return 0, 0, 0
}
