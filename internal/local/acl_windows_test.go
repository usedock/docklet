//go:build windows

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func daclSDDL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// A DOCK_DATA_DIR on a drive like D:\ inherits read access for Authenticated Users; the
// engine must cut that off before a token is written, including for files already there.
func TestDataDirIsRestrictedToTheUserOnWindows(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// SDDL may render the user as an alias (LA for the built-in Administrator).
	owner, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String())
	if err != nil {
		t.Fatal(err)
	}
	me := ";" + strings.TrimPrefix(owner.String(), "O:") + ")"
	dir := filepath.Join(t.TempDir(), "dock")
	if err := os.MkdirAll(filepath.Join(dir, "docks", "dk_old"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Simulate a drive root's ACL: every authenticated user may read.
	open, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FR;;;AU)(A;OICI;FA;;;%s)", user.User.Sid))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := open.DACL()
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "docks", "dk_old", "token")
	if err := os.WriteFile(old, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(daclSDDL(t, old), ";AU)") {
		t.Fatalf("setup: the token must start out readable by AU: %s", daclSDDL(t, old))
	}

	e := &Engine{Dir: dir}
	if err := e.mkDockDir("dk_new"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.file("dk_new", "token"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, old, e.file("dk_new", "token")} {
		s := daclSDDL(t, p)
		if strings.Contains(s, ";AU)") || strings.Contains(s, ";BU)") || strings.Contains(s, ";WD)") {
			t.Fatalf("%s is readable by other users: %s", p, s)
		}
		if !strings.Contains(s, me) {
			t.Fatalf("%s must stay usable by its owner: %s", p, s)
		}
	}
	if _, err := os.ReadFile(old); err != nil {
		t.Fatal(err)
	}
	if err := e.mkDockDir("dk_again"); err != nil {
		t.Fatalf("an already restricted dir is left as is: %v", err)
	}
}
