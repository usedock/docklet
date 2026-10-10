package exec

import (
	"strings"
	"testing"
)

func TestRunSuccess(t *testing.T) {
	if err := Run("true"); err != nil {
		t.Fatal(err)
	}
}

func TestRunErrorCarriesOutput(t *testing.T) {
	err := Run("sh", "-c", "echo oops >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "oops") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
}
