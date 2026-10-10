package main

import (
	"bytes"
	"testing"
)

func TestPrintVersion(t *testing.T) {
	old := version
	version = "v1.2.3"
	defer func() { version = old }()
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var b bytes.Buffer
		if !printVersion(&b, args) || b.String() != "v1.2.3\n" {
			t.Fatalf("%v: got %q", args, b.String())
		}
	}
	for _, args := range [][]string{nil, {"ls"}, {"version", "x"}} {
		var b bytes.Buffer
		if printVersion(&b, args) || b.Len() != 0 {
			t.Fatalf("%v should not be handled", args)
		}
	}
}
