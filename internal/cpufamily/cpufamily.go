// Package cpufamily identifies the host CPU so snapshots only restore on
// compatible hardware. Firecracker gives no cross-family guarantee, so the
// family string is recorded per snapshot and placement refuses mismatches.
package cpufamily

import (
	"os"
	"strings"
)

// Current reads vendor/model/stepping of the first CPU as family triple.
func Current(cpuinfoPath string) (string, error) {
	raw, err := os.ReadFile(cpuinfoPath)
	if err != nil {
		return "", err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := split(line)
		if !ok {
			continue
		}
		if _, done := fields[key]; !done {
			fields[key] = value
		}
		if fields["vendor_id"] != "" && fields["model"] != "" && fields["stepping"] != "" {
			break
		}
	}
	if fields["vendor_id"] == "" || fields["model"] == "" {
		return "unknown", nil
	}
	return fields["vendor_id"] + ":" + fields["model"] + ":" + fields["stepping"], nil
}

// Compatible reports whether a snapshot family restores on this host.
func Compatible(snapshot, host string) bool {
	return snapshot != "" && snapshot == host
}

func split(line string) (string, string, bool) {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}
