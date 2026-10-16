package local

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/usedock/docklet/internal/dockyard"
)

// PushOpts picks the Dockyard base a gVisor dock's work directory is pushed onto. A gVisor dock
// has no disk image, so the remote dock starts from a Dockyard image with the work directory
// copied in. Zero fields take defaults; qemu docks ignore it.
type PushOpts struct {
	// Image is the Dockyard image name (default DefaultImage).
	Image string
	// DiskGiB is the size of the pushed disk (default: the dock's, else DefaultDiskGiB). A
	// gVisor dock's disk size is not enforced, so the work data may exceed it.
	DiskGiB int
}

// gvPushDiskGiB is the disk size of a pushed gVisor dock.
func gvPushDiskGiB(rec Record, opt PushOpts) int {
	switch {
	case opt.DiskGiB >= 1:
		return opt.DiskGiB
	case rec.DiskGiB >= 1:
		return rec.DiskGiB
	}
	return DefaultDiskGiB
}

// gvPushScript is the helper that builds the disk: repair the base, grow it to the file size,
// run the debugfs script (its stderr goes to a file, debugfs exits 0 on failed commands), then
// require a clean check. e2fsck exits 1 when it repaired something, which is fine for the base.
const gvPushScript = `e2fsck -fy /x/disk.raw >&2; rc=$?
if [ "$rc" -gt 1 ]; then echo "e2fsck of the base image failed ($rc)" >&2; exit 10; fi
resize2fs /x/disk.raw >&2 || { echo "resize2fs failed" >&2; exit 11; }
debugfs -w -f /x/script /x/disk.raw >/dev/null 2>/x/debugfs.err || { cat /x/debugfs.err >&2; exit 12; }
e2fsck -fn /x/disk.raw >&2 || { echo "e2fsck found errors in the finished disk" >&2; exit 13; }
`

// gvListCaps lets the root helper read the dock user's 0600 files and 0700 directories.
var gvListCaps = []string{"DAC_READ_SEARCH"}

// gvPushCaps adds DAC_OVERRIDE to use the host-owned 0700 transfer directory. The work volume
// is mounted read-only, so DAC_OVERRIDE cannot change it.
var gvPushCaps = []string{"DAC_READ_SEARCH", "DAC_OVERRIDE"}

// gvWorkBytes is the space the listing's data takes in the disk: file sizes (a hard-linked
// file once) and directories, in 4 KiB blocks.
func gvWorkBytes(listing []byte) int64 {
	const fields, block = 10, 4096
	parts := strings.Split(string(listing), "\x00")
	var total int64
	seen := map[string]bool{}
	for i := 0; i+fields <= len(parts); i += fields {
		typ, size, inode := parts[i], parts[i+5], parts[i+7]
		switch typ {
		case "d":
			total += block
		case "f":
			if seen[inode] {
				continue
			}
			seen[inode] = true
			n, _ := strconv.ParseInt(size, 10, 64)
			total += (max(n, 0) + block - 1) / block * block
		}
	}
	return total
}

// flattenGVisor builds a raw ext4 disk in dir: the Dockyard base image with the dock's work
// directory copied in. Only the work volume is read; the agent config is a bind mount outside
// it, so no token reaches the disk and none needs rotating.
func (e *Engine) flattenGVisor(ctx context.Context, rec Record, ref, dir string, stop bool, opt PushOpts) (*Flat, error) {
	image := opt.Image
	if image == "" {
		image = DefaultImage
	}
	diskGiB := gvPushDiskGiB(rec, opt)
	running := e.aliveRec(ctx, rec)
	if running && !stop {
		return nil, errf("dock_running", "dock %s is running; stop it first or pass --stop", ref)
	}
	im, err := e.ResolveImage(ctx, image)
	if err != nil {
		return nil, err
	}
	// Fetched before the dock stops, so a failed download leaves it running.
	zst, err := e.Cache.EnsureRootfs(ctx, im)
	if err != nil {
		return nil, err
	}
	if running {
		e.logf("stopping %s", ref)
		if err := e.stopGVisor(ctx, rec, false); err != nil {
			return nil, err
		}
	}
	g := e.gvisor()
	helperImage, err := g.EnsureImage(ctx, func(s string) { e.logf("%s", s) })
	if err != nil {
		return nil, err
	}
	rt := rec.gvRuntime()
	work := diskMount{Source: gvVolume(rec.ID), Target: gvWorkSrc, ReadOnly: true}
	e.logf("reading the work directory of %s", ref)
	listing, stderr, err := g.docker(ctx, nil, diskHelperArgs(rt, helperImage, gvListCaps, []diskMount{work}, `exec "$@"`, gvListArgs()...)...)
	if err != nil {
		return nil, dockerFailure("push_failed", "list the work directory", err, stderr)
	}
	script, warnings, err := buildDebugfsScript([]byte(listing))
	if err != nil {
		return nil, err
	}
	disk := int64(diskGiB) << 30
	data := gvWorkBytes([]byte(listing))
	if data > disk {
		return nil, errf("push_too_big", "the work directory holds %s but the pushed disk would be %d GiB; pass --disk with a larger size", HumanBytes(data), diskGiB)
	}
	var zstSize int64
	if st, err := os.Stat(zst); err == nil {
		zstSize = st.Size()
	}
	if err := e.checkFreeSpace(pushSpaceNeed(disk, zstSize*rawDataRatio+data), "push", ""); err != nil {
		return nil, err
	}

	raw := filepath.Join(dir, "disk.raw")
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(raw)
		}
	}()
	e.logf("preparing a %d GiB %s disk", diskGiB, im.Name)
	if err := e.decompressRootfs(ctx, im, zst, raw, disk); err != nil {
		return nil, err
	}
	if used := allocatedBytes(raw); used+data > disk {
		return nil, errf("push_too_big", "the %s image (%s) and the work directory (%s) do not fit in a %d GiB disk; pass --disk with a larger size",
			im.Name, HumanBytes(used), HumanBytes(data), diskGiB)
	}
	if err := os.Truncate(raw, disk); err != nil {
		return nil, errf("transfer_failed", "size the disk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script"), []byte(script), 0o600); err != nil {
		return nil, errf("transfer_failed", "%v", err)
	}
	e.logf("copying the work directory into the disk")
	_, stderr, err = g.docker(ctx, nil, diskHelperArgs(rt, helperImage, gvPushCaps, []diskMount{work, {Source: dir, Target: "/x"}}, gvPushScript)...)
	if err != nil {
		return nil, dockerFailure("push_failed", "build the disk", err, lastLines(stderr, 15))
	}
	dbgErr, err := os.ReadFile(filepath.Join(dir, "debugfs.err"))
	if err != nil {
		return nil, errf("push_failed", "the disk helper left no debugfs result: %v", err)
	}
	if errs := debugfsErrors(string(dbgErr)); len(errs) > 0 {
		return nil, errf("push_failed", "copying the work directory into the disk failed:\n%s", lastLines(strings.Join(errs, "\n"), 10))
	}
	for _, w := range warnings {
		e.logf("warning: %s", w)
	}
	ok = true
	return &Flat{Record: rec, RawPath: raw, Image: im.Name, DiskGiB: diskGiB}, nil
}

// decompressRootfs expands the cached rootfs to raw. A qemu dock's Ensure deletes the cached
// zst once it builds the base, so when it vanishes under us the rootfs is fetched once more.
func (e *Engine) decompressRootfs(ctx context.Context, im dockyard.Image, zst, raw string, max int64) error {
	err := DecompressFile(ctx, zst, raw, max)
	if err != nil && ErrCode(err) == "cache_error" {
		if _, serr := os.Stat(zst); serr != nil {
			if zst, err = e.Cache.EnsureRootfs(ctx, im); err != nil {
				return err
			}
			return DecompressFile(ctx, zst, raw, max)
		}
	}
	return err
}
