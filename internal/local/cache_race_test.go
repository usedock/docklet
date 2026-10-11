package local

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// gatedDriver holds the first ConvertRaw until released, so a second Ensure can run
// through the whole preparation in between.
type gatedDriver struct {
	*fakeDriver
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedDriver) ConvertRaw(ctx context.Context, raw, qcow2 string) error {
	first := false
	g.once.Do(func() { first = true })
	if first {
		close(g.entered)
		<-g.release
	}
	return g.fakeDriver.ConvertRaw(ctx, raw, qcow2)
}

// Two dock commands preparing the same image at once must both succeed with an intact base.
func TestCacheEnsureConcurrentPreparation(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	drv := &gatedDriver{fakeDriver: newFakeDriver(), entered: make(chan struct{}), release: make(chan struct{})}
	dir := t.TempDir()
	a, b := &Cache{Dir: dir, Driver: drv}, &Cache{Dir: dir, Driver: drv}
	errA := make(chan error, 1)
	go func() { _, err := a.Ensure(context.Background(), im); errA <- err }()
	<-drv.entered
	if _, err := b.Ensure(context.Background(), im); err != nil {
		t.Fatalf("second preparation: %v", err)
	}
	close(drv.release)
	if err := <-errA; err != nil {
		t.Fatalf("first preparation must not fail because another one finished: %v", err)
	}
	p := a.paths(im)
	if base, _ := os.ReadFile(p.Base); string(base) != "qcow2:rootfs-raw-bytes" {
		t.Fatalf("base %q", base)
	}
	for _, pat := range []string{"*.raw", "*.part"} {
		if left, _ := filepath.Glob(filepath.Join(p.Dir, pat)); len(left) > 0 {
			t.Fatalf("temp files left behind: %v", left)
		}
	}
}
