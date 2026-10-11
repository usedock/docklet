//go:build linux

package local

import (
	"os"
	"strconv"
	"strings"
)

// procStart is the start time of pid in clock ticks since boot (field 22 of /proc/pid/stat).
func procStart(pid int) (uint64, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The command name may contain spaces and parentheses; the fields follow the last ')'.
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[19], 10, 64)
	return n, err == nil
}
