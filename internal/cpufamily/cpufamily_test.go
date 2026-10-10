package cpufamily

import (
	"os"
	"path/filepath"
	"testing"
)

const fixture = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 142
model name	: Intel(R) Core(TM) i7-8550U CPU @ 1.80GHz
stepping	: 10
`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTriple(t *testing.T) {
	family, err := Current(write(t, fixture))
	if err != nil || family != "GenuineIntel:142:10" {
		t.Fatalf("family = %q,%v", family, err)
	}
}

func TestCompatible(t *testing.T) {
	if !Compatible("GenuineIntel:142:10", "GenuineIntel:142:10") {
		t.Fatal("identical families must be compatible")
	}
	if Compatible("GenuineIntel:142:10", "AuthenticAMD:1:1") {
		t.Fatal("different families must refuse")
	}
	if Compatible("", "GenuineIntel:142:10") {
		t.Fatal("empty snapshot family must refuse")
	}
}
