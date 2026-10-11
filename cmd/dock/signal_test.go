package main

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

func TestShutdownSignalsIncludeSIGTERM(t *testing.T) {
	if !slices.Contains[[]os.Signal, os.Signal](shutdownSignals, syscall.SIGTERM) {
		t.Fatal("SIGTERM must end a command like Ctrl-C so temp files are cleaned up")
	}
}
