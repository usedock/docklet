package local

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

// Check is one line of `dock doctor`.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// Optional rows are informational: a failing one never fails `dock doctor`.
	Optional bool `json:"optional,omitempty"`
}

// minFreeBytes is the free space below which a new dock is likely to fail.
const minFreeBytes = 5 << 30

// Doctor reports what the local engine needs and what is missing. It never boots a dock.
func (e *Engine) Doctor(ctx context.Context) []Check {
	var checks []Check
	add := func(c Check) { checks = append(checks, c) }
	q, isQEMU := e.Driver.(*QEMU)
	// gvisor is informational and Linux only; gvErr is nil when it can run docks.
	showGVisor := isQEMU && q.GOOS == "linux"
	var gvErr error
	var gvSUID bool
	if showGVisor {
		gvSUID, gvErr = e.gvisor().Probe(ctx)
	}
	if !isQEMU {
		add(Check{Name: "driver", OK: e.Driver.Preflight(ctx) == nil, Detail: "custom driver"})
	} else {
		sys, img, err := q.Binaries()
		if err != nil {
			add(Check{Name: "qemu", Detail: err.Error(), Fix: InstallHint(q.GOOS)})
		} else {
			add(Check{Name: "qemu", OK: true, Detail: sys})
			add(Check{Name: "qemu-img", OK: true, Detail: img})
		}
		accel, aerr := q.Accel()
		switch {
		case aerr != nil:
			c := Check{Name: "accelerator", Detail: aerr.Error()}
			if showGVisor && ErrCode(aerr) == "no_accelerator" {
				c.Fix = kvmFix(q.kvmErr())
				if gvErr == nil {
					c.Fix += " Or, without KVM: dock new --runtime gvisor."
				}
			}
			add(c)
		case err != nil:
			add(Check{Name: "accelerator", Detail: accel + " (cannot verify without QEMU)"})
		default:
			if cerr := q.CheckAccel(ctx, accel); cerr != nil {
				c := Check{Name: "accelerator", Detail: cerr.Error()}
				if accel == "whpx" {
					c.Fix = WHPXFix
				}
				add(c)
			} else {
				add(Check{Name: "accelerator", OK: true, Detail: accel})
			}
		}
	}
	if showGVisor {
		c := Check{Name: "gvisor", OK: gvErr == nil, Optional: true, Detail: "docker with runsc: dock new --runtime gvisor"}
		if gvErr == nil && gvSUID {
			c.Detail += " (sudo and a persistent rootfs: runsc-suid found)"
		} else if gvErr == nil {
			c.Detail += " (no sudo and only /home/user/work persists: runsc-suid is not registered)"
			c.Fix = GVisorSUIDFix
		}
		if gvErr != nil {
			c.Detail = gvErr.Error()
			if code := ErrCode(gvErr); code == "docker_not_found" || code == "runsc_not_found" {
				c.Fix = GVisorInstallFix
			}
		}
		add(c)
	}
	if showGVisor && gvErr == nil {
		checks = append(checks, e.gvHostBlockChecks(ctx)...)
	}
	if recs, err := e.store().List(); err == nil {
		for _, r := range recs {
			if r.runtime() == RuntimeGVisor && e.importIncomplete(r) != nil {
				add(Check{Name: "dock " + r.Name, Detail: "the import of its work directory did not finish", Fix: "dock rm " + r.Name + ", then pull again"})
			}
		}
	}
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		add(Check{Name: "data dir", Detail: err.Error()})
	} else if free, err := freeBytes(e.Dir); err != nil {
		add(Check{Name: "disk space", OK: true, Detail: e.Dir + " (free space unknown)"})
	} else {
		c := Check{Name: "disk space", OK: free >= minFreeBytes, Detail: fmt.Sprintf("%s free in %s", HumanBytes(int64(free)), e.Dir)}
		if !c.OK {
			c.Fix = "Free at least 5 GiB, or set DOCK_DATA_DIR to a larger drive."
		}
		add(c)
	}
	ix, err := e.Catalog.Index(ctx)
	if err != nil {
		fix := "Check your network, or set DOCK_DOCKYARD_URL (and DOCK_DOCKYARD_SHA256) to a reachable index."
		if ErrCode(err) == "no_dockyard_release" {
			fix = "Images are published by Dockyard releases (images-*); this dock build has none pinned yet."
		}
		add(Check{Name: "dockyard", Detail: err.Error(), Fix: fix})
		return checks
	}
	if im, err := findImage(ix, DefaultImage, e.Arch); err != nil {
		add(Check{Name: "dockyard", Detail: err.Error()})
	} else {
		add(Check{Name: "dockyard", OK: true, Detail: fmt.Sprintf("%d images for %s/%s", len(ix.Images), runtime.GOOS, e.Arch)})
		c := Check{Name: "default image", OK: e.Cache.Cached(im), Detail: im.Name + " is cached"}
		if !c.OK {
			c.Detail = im.Name + " is not downloaded yet (dock new downloads it)"
			c.Fix = "dock images pull " + im.Name
			// Not downloaded is a hint, not a failure.
			c.OK = true
		}
		add(c)
	}
	return checks
}

// gvHostBlockChecks verifies the host firewall rule of every running network-on gvisor dock. A
// host firewall reload or flush removes it silently. No such dock, or the block turned off, adds no row.
func (e *Engine) gvHostBlockChecks(ctx context.Context) []Check {
	g := e.gvisor()
	if g.AllowHost {
		return nil
	}
	recs, err := e.store().List()
	if err != nil {
		return nil
	}
	var out []Check
	for _, r := range recs {
		if r.runtime() != RuntimeGVisor || r.gvNoNet() || !g.running(ctx, r.ID) {
			continue
		}
		c := Check{Name: "host block " + r.Name}
		ok, err := g.checkHostBlock(ctx, r.ID)
		switch {
		case err != nil:
			// Unknown, not missing: informational so a Docker hiccup does not fail doctor.
			c.Optional = true
			c.Detail = "cannot verify the host firewall rule: " + err.Error()
		case !ok:
			c.Detail = "the firewall rule that blocks this dock from the machine's services is not installed (a firewall reload or flush removed it, or the dock was started with " + gvHostBlockEnv + "=1 or by an older dock)"
			c.Fix = "dock stop " + r.Name + " && dock resume " + r.Name
		default:
			c.OK = true
			c.Detail = "the dock cannot reach the machine's services"
		}
		out = append(out, c)
	}
	return out
}
