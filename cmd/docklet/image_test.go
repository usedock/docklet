package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/usedock/docklet/internal/host"
)

func TestRunImageUsageAndUnknown(t *testing.T) {
	p := host.NewPuller(t.TempDir())
	var out, errb bytes.Buffer
	if code := runImage(nil, &out, &errb, p); code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	if code := runImage([]string{"pull"}, &out, &errb, p); code != 2 {
		t.Fatalf("pull without name exit = %d, want 2", code)
	}
	if code := runImage([]string{"rm", "x"}, &out, &errb, p); code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
}

func TestRunImageLsEmpty(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runImage([]string{"ls"}, &out, &errb, host.NewPuller(t.TempDir())); code != 0 {
		t.Fatalf("ls exit = %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "NAME") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("ls output = %q, want only the header", out.String())
	}
}

func TestRunImagePullRefusesBadInput(t *testing.T) {
	p := host.NewPuller(t.TempDir())
	p.IndexURL = "http://127.0.0.1:1/dockyard.json" // never reached for these names
	var out, errb bytes.Buffer
	if code := runImage([]string{"pull", "../etc"}, &out, &errb, p); code != 1 {
		t.Fatalf("bad name exit = %d, want 1", code)
	}
	if code := runImage([]string{"pull", "ubuntu-24.04"}, &out, &errb, p); code != 1 {
		t.Fatalf("default image exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "host-built") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestNewPullerNeedsBothOverrides(t *testing.T) {
	t.Setenv("DOCKLET_DOCKYARD_INDEX_URL", "http://example.test/dockyard.json")
	t.Setenv("DOCKLET_DOCKYARD_INDEX_SHA256", "")
	if _, err := newPuller(t.TempDir()); err == nil {
		t.Fatal("URL without checksum must be refused")
	}
	t.Setenv("DOCKLET_DOCKYARD_INDEX_SHA256", strings.Repeat("ab", 32))
	p, err := newPuller(t.TempDir())
	if err != nil || p.IndexURL != "http://example.test/dockyard.json" {
		t.Fatalf("override not applied: %+v, %v", p, err)
	}
}
