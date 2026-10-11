package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func resetFlag(t *testing.T, e *Engine, id string) bool {
	t.Helper()
	raw, err := os.ReadFile(e.file(id, "guest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg GuestConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.ResetHostKeys
}

func cycle(t *testing.T, e *Engine, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Stop(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestBranchResetsHostKeysOnFirstBootOnly(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEngine(t)
	src, err := e.New(ctx, Spec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if resetFlag(t, e, src.ID) {
		t.Fatal("a dock from the image keeps the keys it generates")
	}
	made, err := e.Branch(ctx, "src", 2, "", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if resetFlag(t, e, src.ID) {
		t.Fatal("the source must keep its keys when it is stopped and resumed for the copy")
	}
	for _, d := range made {
		if !resetFlag(t, e, d.ID) {
			t.Fatalf("branch %s must reset the copied host keys", d.ID)
		}
		if _, err := os.Stat(e.file(d.ID, resetKeysMarker)); !os.IsNotExist(err) {
			t.Fatalf("the marker must be consumed after the first boot: %v", err)
		}
		cycle(t, e, d.ID)
		if resetFlag(t, e, d.ID) {
			t.Fatalf("branch %s must keep its keys across stop and resume", d.ID)
		}
	}
	cycle(t, e, src.ID)
	if resetFlag(t, e, src.ID) {
		t.Fatal("the source keeps its keys across stop and resume")
	}
}

func TestImportResetsHostKeysOnFirstBootOnly(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEngine(t)
	cached, err := e.EnsureImage(ctx, DefaultImage)
	if err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(t.TempDir(), "d.raw")
	_ = os.WriteFile(raw, []byte("disk"), 0o600)
	d, err := e.Import(ctx, Import{Name: "pulled", Image: DefaultImage, Cached: cached, RawPath: raw, CPUs: 1, MemoryMiB: 512, DiskGiB: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if !resetFlag(t, e, d.ID) {
		t.Fatal("a pulled dock must reset the host keys it arrived with")
	}
	cycle(t, e, d.ID)
	if resetFlag(t, e, d.ID) {
		t.Fatal("the pulled dock keeps its fresh keys across stop and resume")
	}
}

func TestFailedBootKeepsTheResetMarker(t *testing.T) {
	ctx := context.Background()
	e, drv, _ := newTestEngine(t)
	if _, err := e.New(ctx, Spec{Name: "src"}); err != nil {
		t.Fatal(err)
	}
	drv.dieOnStart = true
	made, err := e.Branch(ctx, "src", 1, "", Spec{})
	if err == nil {
		t.Fatalf("the branch must fail to boot, got %v", made)
	}
	recs, _ := e.store().List()
	for _, r := range recs {
		if r.Parent == "" {
			continue
		}
		if _, err := os.Stat(e.file(r.ID, resetKeysMarker)); err != nil {
			t.Fatalf("a boot that never came up must retry the reset: %v", err)
		}
	}
}
