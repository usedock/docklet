package fc

import (
	"fmt"
	"os"
	"syscall"
)

// ChownFile chowns a regular file without following a symlink: a link, FIFO
// or any other non-regular file at path is refused instead of being chowned.
func ChownFile(path string, uid, gid int) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("chown %s: not a regular file", path)
	}
	return f.Chown(uid, gid)
}
