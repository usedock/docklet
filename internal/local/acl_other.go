//go:build !windows

package local

// secureDir is a no-op where MkdirAll's 0700 mode already keeps other users out.
func secureDir(string) error { return nil }
