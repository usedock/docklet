// Package exec owns host command execution. One shared Runner type keeps
// disk, net, and host signatures assignable without conversions.
package exec

import (
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes a host command.
type Runner func(name string, args ...string) error

// Run executes through os/exec, folding command output into errors so
// host failures stay diagnosable from the control plane.
func Run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
