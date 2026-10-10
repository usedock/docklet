package stats

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Allocated sums the host blocks held under path, so sparse rootfs images
// count what they occupy rather than their apparent size. Hard links count
// once and symlinks are never followed.
func Allocated(path string) (int64, error) {
	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	var total int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Entries removed mid-walk are normal on a live disk.
			if os.IsNotExist(err) && p != path {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size()
			return nil
		}
		if st.Nlink > 1 && !info.IsDir() {
			key := inode{uint64(st.Dev), uint64(st.Ino)}
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		total += int64(st.Blocks) * 512
		return nil
	})
	return total, err
}
