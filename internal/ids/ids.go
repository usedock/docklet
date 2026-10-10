// Package ids validates control-plane and host-minted identifiers before they
// reach filesystem paths, interface names, or process arguments.
package ids

import (
	"errors"
	"fmt"
	"regexp"
)

// pattern admits UUIDs (dock and runtime ids) and 32-hex generation ids; it
// caps length at 58 so legacy "dock-"+id container names never truncate; it refuses separators, dots, and whitespace so no id can escape a join.
var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,57}$`)

// ErrInvalid marks a rejected identifier; servers map it to a 400.
var ErrInvalid = errors.New("invalid id")

// Valid reports whether id is a safe identifier.
func Valid(id string) bool { return pattern.MatchString(id) }

// Check returns a validation error naming kind when id is unsafe.
func Check(kind, id string) error {
	if !Valid(id) {
		return fmt.Errorf("%w: %s must match [A-Za-z0-9][A-Za-z0-9_-]{0,57}", ErrInvalid, kind)
	}
	return nil
}
