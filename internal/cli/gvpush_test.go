package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/usedock/docklet/internal/local"
)

// gvDocker is a fake docker for a stopped gVisor dock: it answers the image and volume checks,
// the work listing and the disk build helper.
type gvDocker struct {
	mu      sync.Mutex
	calls   [][]string
	listing string
}

func (g *gvDocker) run(_ context.Context, _ io.Reader, args ...string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, args)
	switch args[0] {
	case "inspect":
		return "false\n", "", nil
	case "image":
		return "", "", nil
	case "volume":
		if args[1] == "inspect" && args[len(args)-1] == "dock-local-dk_gv-work" {
			return "[]", "", nil
		}
	case "run":
		if slices.Contains(args, "find") {
			return g.listing, "", nil
		}
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":/x") {
				dir := strings.TrimSuffix(args[i+1], ":/x")
				if err := os.WriteFile(filepath.Join(dir, "debugfs.err"), nil, 0o600); err != nil {
					return "", err.Error(), err
				}
			}
		}
		return "", "", nil
	}
	return "", "unexpected docker " + args[0], errors.New("exit status 1")
}

// gvisorPushEnv is a push env with a stopped gVisor dock "gv" whose work volume lists one file.
func gvisorPushEnv(t *testing.T) (*testEnv, *fakePlane, *gvDocker) {
	t.Helper()
	te, p := transferEnv(t, "direct")
	g := &gvDocker{listing: strings.Join([]string{"d", "755", "1001", "1001", "1700000000.0", "4096", "", "64:1", "2", "",
		"f", "600", "1001", "1001", "1700000001.0", "11", "", "64:2", "1", "secret.txt", ""}, "\x00")}
	te.app.Engine.GVisor = &local.GVisor{GOOS: "linux", Run: g.run, LookPath: func(string) (string, error) { return "/usr/bin/docker", nil }}
	st := &local.Store{Dir: te.app.Engine.Dir}
	err := st.Update(func(recs *[]local.Record) error {
		*recs = append(*recs, local.Record{ID: "dk_gv", Name: "gv", Image: "dock-local-gvisor", Runtime: local.RuntimeGVisor,
			GVisorRuntime: "runsc-suid", CPUs: 2, MemoryMiB: 1024, DiskGiB: 10, Network: "on"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return te, p, g
}

func TestPushGVisorSendsADockyardImageAndNotesTheWorkDirOnly(t *testing.T) {
	te, p, g := gvisorPushEnv(t)
	mustExit(t, te, 0, "push", "gv", "--to", "lab", "--disk", "1gb")
	b := p.importBody
	if b["image"] != local.DefaultImage || b["diskGiB"] != 1.0 || b["name"] != "gv" || b["cpus"] != 2.0 {
		t.Fatalf("import body %v", b)
	}
	if want := "note: only /home/user/work is pushed; packages and files outside it in this gVisor dock are not. The remote dock starts from " + local.DefaultImage + " with your work dir."; !strings.Contains(te.errb.String(), want) {
		t.Fatalf("stderr %q", te.errb)
	}
	// The listing helper and the build helper both ran under the dock's runtime.
	var helpers int
	for _, c := range g.calls {
		if c[0] == "run" {
			helpers++
			if !slices.Contains(c, "--runtime=runsc-suid") || !slices.Contains(c, "DAC_READ_SEARCH") {
				t.Fatalf("helper %v", c)
			}
		}
	}
	if helpers != 2 {
		t.Fatalf("helpers %d: %v", helpers, g.calls)
	}
	if ents := tmpEntries(t, te); len(ents) != 0 {
		t.Fatalf("the transfer directory must be removed: %v", ents)
	}
}

func TestPushGVisorImageFlag(t *testing.T) {
	te, p, _ := gvisorPushEnv(t)
	mustExit(t, te, 1, "push", "gv", "--to", "lab", "--image", "no-such-image")
	if p.importBody != nil {
		t.Fatalf("an unknown image must fail before the import: %v", p.importBody)
	}
	// An image only another architecture has is not available either.
	mustExit(t, te, 1, "push", "gv", "--to", "lab", "--image", "other-arch")
	mustExit(t, te, 2, "push", "gv", "--to", "lab", "--disk", "bogus")
	mustExit(t, te, 0, "push", "gv", "--to", "lab", "--image", local.DefaultImage, "--disk", "1")
	if p.importBody["image"] != local.DefaultImage {
		t.Fatalf("import body %v", p.importBody)
	}
}

func TestPushImageAndDiskFlagsAreForGVisorDocksOnly(t *testing.T) {
	te, p := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	mustExit(t, te, 2, "push", "web", "--to", "lab", "--image", local.DefaultImage)
	mustExit(t, te, 2, "push", "web", "--to", "lab", "--disk", "20gb")
	if p.importBody != nil || strings.Contains(te.errb.String(), "only /home/user/work") {
		t.Fatalf("a qemu push must neither import nor print the gVisor note: %q %v", te.errb, p.importBody)
	}
}
