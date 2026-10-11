package local

import (
	"errors"
	"fmt"
)

// Error is a user-facing engine failure with a stable code, like the API's error envelope.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// ErrCode returns the engine error code of err, or "" for other errors.
func ErrCode(err error) string {
	var le *Error
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}
