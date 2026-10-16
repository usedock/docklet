package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Pulling a cloud dock's work directory into a gVisor dock: the exported disk is a bare ext4
// image, so a one-shot e2fsprogs helper under the dock's own runtime reads /home/user/work out
// of it with debugfs (no mount, no loop device, no host root) into the new dock's work volume.

// gvFreshMarker is a file in the dock directory of an imported dock that has no container yet;
// the first resume boots it as a first start and removes the marker.
const gvFreshMarker = "imported"

const (
	// gvDiskPath is where the helper sees the exported disk.
	gvDiskPath = "/x/disk.raw"
	// gvNoWorkdirMarker is printed by the helper when the disk has no gvWorkDest.
	gvNoWorkdirMarker = "DOCK_NO_WORKDIR"
	// gvDebugfsPrefix tags the helper's debugfs stderr lines so Docker's own warnings are not
	// mistaken for debugfs errors.
	gvDebugfsPrefix = "DEBUGFS: "
)

// gvImportScript copies gvWorkDest out of the disk into /work. $1 is a random stage name.
// debugfs exits 0 even when rdump fails, so its stderr is relayed with gvDebugfsPrefix and
// checked by the caller. After the move /work itself is made the dock user's (a fresh volume
// root is root-owned), then ids 1000 (the cloud user) are mapped to 1001 (the gVisor user).
// debugfs rdump leaves symlinks owned by root, so those get the dock user too.
const gvImportScript = `set -e
d=` + gvDiskPath + `
out=$(debugfs -R "stat ` + gvWorkDest + `" "$d" 2>&1 >/dev/null || true)
case "$out" in
*"File not found"*) echo ` + gvNoWorkdirMarker + ` ;;
*)
  s=` + gvWorkSrc + `/.stage-$1
  mkdir "$s"
  debugfs -R "rdump ` + gvWorkDest + ` $s" "$d" 2>&1 >/dev/null | sed 's/^/` + gvDebugfsPrefix + `/' >&2
  [ -d "$s/work" ] || { echo "` + gvDebugfsPrefix + `rdump produced no work directory" >&2; exit 1; }
  chmod --reference="$s/work" ` + gvWorkSrc + `
  find "$s/work" -mindepth 1 -maxdepth 1 -exec mv -t ` + gvWorkSrc + ` {} +
  rm -rf "$s"
  ;;
esac
chown 1001:1001 ` + gvWorkSrc + `
chown -hR --from=1000 1001 ` + gvWorkSrc + `
chown -hR --from=:1000 :1001 ` + gvWorkSrc + `
find ` + gvWorkSrc + ` -type l -user 0 -exec chown -h 1001:1001 {} +
`

// gvImportCaps are the capabilities of the import helper: rdump sets owners, and the helper
// moves and chowns files it does not own.
var gvImportCaps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}

// gvImporter is the container name of the import helper of dock id, so a failed import can remove it.
func gvImporter(id string) string { return "dock-local-" + id + "-import" }

// gvImportArgs is the docker run of the import helper for dock id. Its rootfs is read-only, so
// the only thing an untrusted disk can write to is the new dock's own work volume.
func gvImportArgs(runtime, image, id, disk, stage string) []string {
	a := diskHelperArgs(runtime, image, gvImportCaps, []diskMount{
		{Source: disk, Target: gvDiskPath, ReadOnly: true},
		{Source: gvVolume(id), Target: gvWorkSrc},
	}, gvImportScript, stage)
	return append([]string{a[0], "--name", gvImporter(id), "--read-only"}, a[1:]...)
}

// debugfsRdumpErrors returns the debugfs error lines the import helper relayed on stderr. Unlike
// debugfsErrors there is no expected "already exists": any line is a failure.
func debugfsRdumpErrors(stderr string) []string {
	var errs []string
	for _, l := range strings.Split(stderr, "\n") {
		l, ok := strings.CutPrefix(strings.TrimSpace(l), strings.TrimSpace(gvDebugfsPrefix))
		l = strings.TrimSpace(l)
		if !ok || l == "" || debugfsBanner.MatchString(l) {
			continue
		}
		errs = append(errs, l)
	}
	return errs
}

// ImportWork describes a pulled, decompressed raw disk whose work directory becomes a new gVisor dock.
type ImportWork struct {
	Name      string
	RawPath   string
	CPUs      int
	MemoryMiB int
	// Network is "on" (default) or "off".
	Network string
}

// ImportWork registers a stopped gVisor dock whose work volume holds /home/user/work of the raw
// ext4 disk. The first resume boots it. Nothing is left behind on failure.
func (e *Engine) ImportWork(ctx context.Context, in ImportWork) (Dock, error) {
	if err := e.CheckName(in.Name); err != nil {
		return Dock{}, err
	}
	spec, err := Spec{Name: in.Name, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB, Network: in.Network, Runtime: RuntimeGVisor}.withDefaults()
	if err != nil {
		return Dock{}, err
	}
	disk, err := filepath.Abs(in.RawPath)
	if err != nil {
		return Dock{}, errf("invalid_argument", "%v", err)
	}
	image, suid, err := e.gvPrepare(ctx)
	if err != nil {
		return Dock{}, err
	}
	rec, err := e.gvNewRecord(spec, false, suid)
	if err != nil {
		return Dock{}, err
	}
	g := e.gvisor()
	fail := func(err error) (Dock, error) {
		bg := context.WithoutCancel(ctx)
		// A cancelled docker run leaves its container running, and it would hold the volume.
		_, _, _ = g.docker(bg, nil, "rm", "-f", gvImporter(rec.ID))
		_ = g.removeVolume(bg, rec.ID)
		_ = e.dropRecord(rec.ID)
		_ = e.removeFiles(rec.ID)
		return Dock{}, err
	}
	e.logf("importing the work directory into %s", rec.ID)
	stdout, stderr, err := g.docker(ctx, nil, gvImportArgs(rec.gvRuntime(), image, rec.ID, disk, NewID())...)
	if err != nil {
		return fail(dockerFailure("import_failed", "copy the work directory out of the disk", err, lastLines(stderr, 20)))
	}
	if errs := debugfsRdumpErrors(stderr); len(errs) > 0 {
		return fail(errf("import_failed", "copying %s out of the disk failed: %s", gvWorkDest, strings.Join(errs, "; ")))
	}
	if strings.Contains(stdout, gvNoWorkdirMarker) {
		e.logf("the disk has no %s; the new dock starts with an empty work directory", gvWorkDest)
	}
	if err := os.WriteFile(e.file(rec.ID, gvFreshMarker), nil, 0o600); err != nil {
		return fail(errf("store_error", "%v", err))
	}
	return e.view(ctx, rec), nil
}

// PullRuntime picks the runtime a pull imports into: the flag, else the env value
// (DOCK_LOCAL_RUNTIME), else qemu, and runs that runtime's preflight so the pull fails before
// the export. A qemu pull whose hypervisor is unusable gets a hint to use gVisor.
func (e *Engine) PullRuntime(ctx context.Context, flag, env string) (string, error) {
	rt := flag
	if rt == "" {
		rt = env
	}
	if rt == "" {
		rt = RuntimeQEMU
	}
	switch rt {
	case RuntimeGVisor:
		if err := e.gvisor().Preflight(ctx); err != nil {
			return "", err
		}
		return rt, nil
	case RuntimeQEMU:
		if err := e.Driver.Preflight(ctx); err != nil {
			var le *Error
			if errors.As(err, &le) && (le.Code == "no_accelerator" || le.Code == "qemu_not_found") {
				return "", errf(le.Code, "%s. To pull into a gVisor dock instead, pass --runtime gvisor", strings.TrimRight(le.Message, ". "))
			}
			return "", err
		}
		return rt, nil
	}
	return "", errf("invalid_runtime", "runtime must be qemu or gvisor, not %q", rt)
}
