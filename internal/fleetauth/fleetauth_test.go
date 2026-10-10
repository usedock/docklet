package fleetauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBearerFallsBackToEnrollTokenUntilIssued(t *testing.T) {
	c := Open("", "enroll")
	if got := c.Bearer(); got != "enroll" {
		t.Fatalf("bearer = %q, want enrollment token", got)
	}
	if err := c.Set("hc_issued"); err != nil {
		t.Fatal(err)
	}
	if got := c.Bearer(); got != "hc_issued" {
		t.Fatalf("bearer = %q, want issued credential", got)
	}
}

func TestSetPersistsOwnerOnlyAndOpenReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "fleet-credential")
	if err := Open(path, "enroll").Set("hc_issued"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("credential file mode = %o, want 600", perm)
		}
	}
	if got := Open(path, "").Bearer(); got != "hc_issued" {
		t.Fatalf("reloaded bearer = %q, want hc_issued", got)
	}
}
