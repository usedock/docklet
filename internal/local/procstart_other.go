//go:build !linux && !darwin && !windows

package local

// procStart is unknown on this OS; the pid alone identifies the VM.
func procStart(int) (uint64, bool) { return 0, false }
