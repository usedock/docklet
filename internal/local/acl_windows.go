//go:build windows

package local

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// secureDir gives dir a protected DACL (the current user, SYSTEM and Administrators only)
// that existing and new children inherit. os.MkdirAll ignores the mode on Windows, so a
// DOCK_DATA_DIR on a drive root such as D:\ would otherwise inherit read access for
// every local user, and with it every dock's agent token.
func secureDir(dir string) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if ctl, _, err := sd.Control(); err == nil && ctl&windows.SE_DACL_PROTECTED != 0 {
		return nil
	}
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", user.User.Sid.String())
	want, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := want.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
