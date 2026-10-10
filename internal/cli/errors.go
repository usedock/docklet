package cli

import (
	"errors"
	"fmt"
	"runtime"
)

func isWindows() bool { return runtime.GOOS == "windows" }

// Error is a user-facing failure printed as "error: <code>: <message>".
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// usageError is a bad invocation and exits 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

// exitError carries a remote command's exit code.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

var errHelp = errors.New("help requested")
