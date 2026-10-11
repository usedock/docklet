//go:build darwin

package local

import "golang.org/x/sys/unix"

// procStart is the start time of pid in microseconds since the epoch.
func procStart(pid int) (uint64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return 0, false
	}
	t := kp.Proc.P_starttime
	return uint64(t.Sec)*1e6 + uint64(t.Usec), true
}
