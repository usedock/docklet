package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// splitSize splits "8gb" into the number and a lowercase unit suffix.
func splitSize(s string) (uint64, string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i > 12 {
		return 0, "", fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil || n == 0 {
		return 0, "", fmt.Errorf("invalid size %q", s)
	}
	return n, strings.TrimSpace(s[i:]), nil
}

// ParseMemoryMiB parses a memory size to MiB; a plain number is MiB.
func ParseMemoryMiB(s string) (int, error) {
	n, unit, err := splitSize(s)
	if err != nil {
		return 0, err
	}
	var mib uint64
	switch unit {
	case "", "m", "mb", "mib":
		mib = n
	case "g", "gb", "gib":
		mib = n * 1024
	case "t", "tb", "tib":
		mib = n * 1024 * 1024
	default:
		return 0, fmt.Errorf("invalid size %q: unknown unit %q", s, unit)
	}
	return int(mib), nil
}

// ParseDiskGiB parses a disk size to GiB; a plain number is GiB.
func ParseDiskGiB(s string) (int, error) {
	n, unit, err := splitSize(s)
	if err != nil {
		return 0, err
	}
	switch unit {
	case "", "g", "gb", "gib":
		return int(n), nil
	case "t", "tb", "tib":
		return int(n * 1024), nil
	case "m", "mb", "mib":
		if n%1024 != 0 {
			return 0, fmt.Errorf("invalid size %q: disk must be whole GiB", s)
		}
		return int(n / 1024), nil
	default:
		return 0, fmt.Errorf("invalid size %q: unknown unit %q", s, unit)
	}
}

// formatMiB renders MiB as gb when whole, else mb.
func formatMiB(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dgb", mib/1024)
	}
	return fmt.Sprintf("%dmb", mib)
}
