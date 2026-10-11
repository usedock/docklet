//go:build !windows

package host

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (created if missing),
// blocking until it is free. The returned func releases it. flock locks belong
// to the open file, so two pulls in one process serialize like two processes.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
