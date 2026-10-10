package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/ids"
)

func TestFailedMemoryCaptureResumesNonTerminalSource(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "src", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	driver.snapErr = errors.New("memory capture failed")
	if _, _, _, err := h.SnapshotCreate(ctx, "src", nil, apiv1.SnapshotFull, true, true); err == nil {
		t.Fatal("capture must report the failure")
	}
	if driver.paused != 1 || driver.resumed != 1 {
		t.Fatalf("paused=%d resumed=%d, a failed branch capture must thaw the source", driver.paused, driver.resumed)
	}
}

func TestFailedFreezeResumesNonTerminalSource(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "src", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.jailRoot("src"), fc.RootfsName)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.SnapshotCreate(ctx, "src", nil, apiv1.SnapshotDiff, true, false); err == nil {
		t.Fatal("freezing a missing rootfs must fail")
	}
	if driver.resumed != 1 {
		t.Fatalf("resumed=%d, a failed freeze must thaw the source", driver.resumed)
	}
}

func TestFailedTerminalCaptureDoesNotResume(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "src", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	driver.snapErr = errors.New("memory capture failed")
	if _, _, _, err := h.SnapshotCreate(ctx, "src", nil, apiv1.SnapshotFull, false, true); err == nil {
		t.Fatal("capture must report the failure")
	}
	if driver.resumed != 0 {
		t.Fatalf("resumed=%d, a terminal capture must stay frozen for the caller", driver.resumed)
	}
}

func TestUnsafeIDsAreRefusedBeforeTouchingDisk(t *testing.T) {
	ctx := context.Background()
	h, driver, dir := testHost(t)
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "../../victim"
	if err := h.Destroy(ctx, bad); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("destroy err = %v, want ErrInvalid", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("destroy of a traversal id must not delete anything: %v", err)
	}
	if _, err := h.Provision(ctx, bad, resDefault, "", ""); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("provision err = %v, want ErrInvalid", err)
	}
	if _, err := h.Provision(ctx, "dock-1", resDefault, "", "../gen"); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("provision snapshot err = %v, want ErrInvalid", err)
	}
	if _, err := h.SnapshotLoad(ctx, "dock-1", resDefault, "a/b"); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("load err = %v, want ErrInvalid", err)
	}
	parent := "../p"
	if _, _, _, err := h.SnapshotCreate(ctx, "dock-1", &parent, apiv1.SnapshotDiff, false, false); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("snapshot parent err = %v, want ErrInvalid", err)
	}
	if err := h.Pause(ctx, bad); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("pause err = %v, want ErrInvalid", err)
	}
	if _, err := h.GenerationTree(ctx, bad); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("tree err = %v, want ErrInvalid", err)
	}
	if _, err := h.ProvisionFast(ctx, bad, resDefault, "", "tok", "/w", nil); !errors.Is(err, ids.ErrInvalid) {
		t.Fatalf("fast err = %v, want ErrInvalid", err)
	}
	if driver.boots != 0 || driver.terminated != 0 || driver.paused != 0 {
		t.Fatalf("boots=%d terminated=%d paused=%d, refused ids must never reach the driver", driver.boots, driver.terminated, driver.paused)
	}
}
