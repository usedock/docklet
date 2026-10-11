//go:build windows

package host

// lockFile is a no-op where flock is unavailable; hosts run Linux.
func lockFile(string) (func(), error) { return func() {}, nil }
