package local

import (
	"os"
	"strings"
	"testing"
)

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func TestEmbeddedEntrypointMatchesScripts(t *testing.T) {
	want, err := os.ReadFile("../../scripts/dock-container-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if lf(gvEntrypoint) != lf(string(want)) {
		t.Fatal("internal/local/assets/dock-container-entrypoint.sh drifted from scripts/dock-container-entrypoint.sh; copy it over")
	}
}
