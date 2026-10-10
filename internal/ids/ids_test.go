package ids

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	good := []string{"33333333-3333-4333-8333-333333333333", "0123456789abcdef0123456789abcdef", "dock-1", "pool-ab12", "x"}
	for _, id := range good {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
	bad := []string{"", "..", "../etc", "a/b", `a\b`, "a b", ".hidden", "-lead", "a:b", "a\x00b", "a\n", "firecracker --id x", strings.Repeat("a", 59)}
	for _, id := range bad {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
		if Check("dockId", id) == nil {
			t.Errorf("Check(%q) = nil, want error", id)
		}
	}
}
