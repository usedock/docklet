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
