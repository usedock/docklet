//go:build windows

package disk

// AllocatedBytes is unknown off unix hosts; 0 means "not recorded".
func AllocatedBytes(string) int64 { return 0 }
