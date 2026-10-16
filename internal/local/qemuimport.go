package local

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"time"
)

// Pulling a fleet gVisor dock into a QEMU dock: a fresh dock is made from the Dockyard base and
// the work tar (rewritten by sanitizeWorkTar, so only the canonical copy reaches the guest) is
// attached to the first boot as a read-only raw virtio drive. The guest agent unpacks it into
// /home/user/work as root, the owners move from 1001 (the gVisor user) to 1000 (the Dockyard
// user), and the dock is stopped. Its next boot has no such drive. The agent has no stdin for
// exec, so streaming the tar through it is not possible; a drive also avoids any size cap on
// the agent connection.

const (
	// qemuImportTimeout bounds the unpack inside the guest.
	qemuImportTimeout = 30 * time.Minute
	// qemuChownMap moves the files from the gVisor dock user (1001, the owner sanitizeWorkTar
	// sets) to the Dockyard user (1000).
	qemuChownMap = "1001:1001 1000:1000"
)

// qemuImportScript runs in the guest as the dock user through the agent and does the unpack as
// root. It finds the import drive by its virtio serial (/sys/block/vdX/serial needs no udev),
// refuses a work directory that is not empty, unpacks without absolute names, and maps the owners.
const qemuImportScript = `sudo -n bash -s <<'DOCK_IMPORT'
set -eu
w=` + gvWorkDest + `
dev=
for d in /sys/block/vd*; do
  if [ -r "$d/serial" ] && [ "$(cat "$d/serial")" = ` + ImportDriveSerial + ` ]; then dev=/dev/${d##*/}; fi
done
if [ -z "$dev" ] || [ ! -b "$dev" ]; then echo "the import drive was not found in the guest" >&2; exit 2; fi
mkdir -p "$w"
if [ -n "$(ls -A "$w")" ]; then echo "$w is not empty" >&2; exit 3; fi
tar -x -p --numeric-owner -f "$dev" -C "$w"
chown -hR --from=` + qemuChownMap + ` "$w"
sync
DOCK_IMPORT
`

// WorkTarQEMU describes a pulled, decompressed work tar that becomes a new QEMU dock.
type WorkTarQEMU struct {
	Name string
	// Image is the Dockyard base the new dock starts from; empty means DefaultImage.
	Image string
	// TarPath is the decompressed tar.
	TarPath                  string
	CPUs, MemoryMiB, DiskGiB int
	// Network is "on" (default) or "off".
	Network string
	// MaxBytes caps the bytes of files the tar may declare; 0 means DefaultMaxRawBytes.
	MaxBytes int64
}

// ImportWorkTarQEMU registers a stopped QEMU dock, made from a fresh Dockyard base, whose
// /home/user/work holds the files of the tar. Nothing is left behind on failure.
func (e *Engine) ImportWorkTarQEMU(ctx context.Context, in WorkTarQEMU) (Dock, error) {
	if err := e.CheckName(in.Name); err != nil {
		return Dock{}, err
	}
	spec, err := Spec{Name: in.Name, Image: in.Image, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB, DiskGiB: in.DiskGiB, Network: in.Network, Runtime: RuntimeQEMU}.withDefaults()
	if err != nil {
		return Dock{}, err
	}
	src, err := filepath.Abs(in.TarPath)
	if err != nil {
		return Dock{}, errf("invalid_argument", "%v", err)
	}
	// The canonical copy lives beside the download, so it goes with the pull's temp directory.
	clean := src + ".clean"
	defer os.Remove(clean)
	st, err := sanitizeWorkTar(src, clean, cmp.Or(in.MaxBytes, DefaultMaxRawBytes))
	if err != nil {
		return Dock{}, err
	}
	if w := skippedWarning(st); w != "" {
		e.logf("warning: %s", w)
	}
	rec, err := e.createQEMU(ctx, spec, true)
	if err != nil {
		return Dock{}, err
	}
	fail := func(err error) (Dock, error) {
		bg := context.WithoutCancel(ctx)
		_ = e.stopVM(bg, rec.ID, true)
		_ = e.dropRecord(rec.ID)
		_ = e.removeFiles(rec.ID)
		return Dock{}, err
	}
	e.logf("unpacking the work directory into %s", rec.ID)
	if err := e.bootWith(ctx, rec, clean); err != nil {
		return fail(err)
	}
	res, err := e.Exec(ctx, rec.ID, qemuImportScript, qemuImportTimeout)
	if err != nil {
		return fail(err)
	}
	if res.ExitCode != 0 {
		return fail(errf("import_failed", "unpack the work directory in the guest (exit %d): %s", res.ExitCode, lastLines(res.Stderr, 20)))
	}
	// The VM stops here, so the next boot is a normal one without the drive.
	if err := e.stopVM(ctx, rec.ID, false); err != nil {
		return fail(err)
	}
	if e.alive(rec.ID) {
		return fail(errf("import_failed", "the VM did not stop after the import"))
	}
	if err := os.Remove(e.file(rec.ID, gvImportingMarker)); err != nil {
		return fail(errf("store_error", "%v", err))
	}
	return e.view(ctx, rec), nil
}
