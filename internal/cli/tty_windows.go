//go:build windows

package cli

import "golang.org/x/sys/windows"

// enableVT turns on escape sequence processing for console output so the guest shell's
// colors and cursor movement render; the returned func restores the previous mode.
func enableVT(fd int) func() {
	h := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}
