package local

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func workTarEntries() []tent {
	return []tent{tdir("./"), tfile("./a.txt", "hello"), tdir("./d/"), tsym("./l", "a.txt"), tspecial("./pipe", '6')}
}

func TestImportWorkTarQEMU(t *testing.T) {
	e, drv, _ := newTestEngine(t)
	ctx := context.Background()
	src := writeTar(t, workTarEntries()...)
	d, err := e.ImportWorkTarQEMU(ctx, WorkTarQEMU{Name: "fromgv", TarPath: src, CPUs: 3, MemoryMiB: 768, DiskGiB: 12})
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "archived" || d.Runtime != RuntimeQEMU || d.Image != DefaultImage || d.CPUs != 3 || d.MemoryMiB != 768 || d.DiskGiB != 12 {
		t.Fatalf("dock %+v", d)
	}
	if len(drv.starts) != 1 || drv.starts[0].ImportDrive == "" || drv.starts[0].CPUs != 3 {
		t.Fatalf("the import boot must attach the drive: %+v", drv.starts)
	}
	if !slices.Equal(drv.resizes, []int{12}) {
		t.Fatalf("resizes %v", drv.resizes)
	}
	// The guest gets the canonical copy: every owner 1001, the special file left out.
	if len(drv.driveData) != 1 || len(drv.driveData[0]) == 0 {
		t.Fatalf("drive data %v", drv.driveData)
	}
	canon := filepath.Join(t.TempDir(), "canon.tar")
	if err := os.WriteFile(canon, drv.driveData[0], 0o600); err != nil {
		t.Fatal(err)
	}
	names, hdrs, bodies := readTar(t, canon)
	if slices.Contains(names, "pipe") || bodies["a.txt"] != "hello" || hdrs["a.txt"].Uid != 1001 || hdrs["l"].Linkname != "a.txt" {
		t.Fatalf("names %v hdrs %v", names, hdrs)
	}
	if _, err := os.Stat(drv.starts[0].ImportDrive); err == nil {
		t.Fatal("the canonical copy must not outlive the import")
	}
	// The marker exists while the drive is attached and is gone afterwards.
	if len(drv.driveMarker) != 1 || !drv.driveMarker[0] {
		t.Fatalf("the importing marker must exist during the import boot: %v", drv.driveMarker)
	}
	rec := mustLookup(t, e, "fromgv")
	if _, err := os.Stat(e.file(rec.ID, gvImportingMarker)); err == nil {
		t.Fatal("the marker must be removed once the import succeeded")
	}
	// The guest runs the unpack as root and maps the owners.
	if len(drv.scripts) != 1 {
		t.Fatalf("scripts %v", drv.scripts)
	}
	for _, want := range []string{"sudo -n bash -s", "serial", ImportDriveSerial, "tar -x -p --numeric-owner -f \"$dev\" -C \"$w\"", "chown -hR --from=1001:1001 1000:1000 \"$w\"", "is not empty"} {
		if !strings.Contains(drv.scripts[0], want) {
			t.Fatalf("script lacks %q:\n%s", want, drv.scripts[0])
		}
	}
	if strings.Contains(drv.scripts[0], "--absolute-names") {
		t.Fatal("never unpack absolute names")
	}
	// The next boot is a normal one: no drive.
	if _, err := e.Resume(ctx, "fromgv"); err != nil {
		t.Fatal(err)
	}
	if len(drv.starts) != 2 || drv.starts[1].ImportDrive != "" {
		t.Fatalf("a later boot must not attach the drive: %+v", drv.starts)
	}
}

func TestImportWorkTarQEMUFailureRemovesTheDock(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*fakeDriver){
		"guest script fails": func(d *fakeDriver) { d.importExit = 3 },
		"vm never answers":   func(d *fakeDriver) { d.neverAnswer = true },
	} {
		e, drv, _ := newTestEngine(t)
		e.BootTimeout = 300e6
		setup(drv)
		_, err := e.ImportWorkTarQEMU(ctx, WorkTarQEMU{Name: "bad", TarPath: writeTar(t, workTarEntries()...)})
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if name == "guest script fails" && (ErrCode(err) != "import_failed" || !strings.Contains(err.Error(), "boom")) {
			t.Fatalf("%s: %v", name, err)
		}
		if docks, _ := e.List(ctx); len(docks) != 0 {
			t.Fatalf("%s: docks left: %v", name, docks)
		}
		if ents, _ := os.ReadDir(filepath.Join(e.Dir, "docks")); len(ents) != 0 {
			t.Fatalf("%s: dock dirs left: %v", name, ents)
		}
		for pid, alive := range drv.alive {
			if alive {
				t.Fatalf("%s: VM %d still runs", name, pid)
			}
		}
	}
}

func TestImportWorkTarQEMURefusesAHostileTarBeforeAnyDock(t *testing.T) {
	e, drv, _ := newTestEngine(t)
	_, err := e.ImportWorkTarQEMU(context.Background(), WorkTarQEMU{Name: "evil", TarPath: writeTar(t, tdir("./"), tfile("../evil", "x"))})
	if ErrCode(err) != "invalid_archive" || len(drv.starts) != 0 || len(drv.overlays) != 0 {
		t.Fatalf("err %v starts %v overlays %v", err, drv.starts, drv.overlays)
	}
}

func TestImportWorkTarQEMUNameAndImage(t *testing.T) {
	e, _, _ := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.New(ctx, Spec{Name: "taken"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ImportWorkTarQEMU(ctx, WorkTarQEMU{Name: "taken", TarPath: writeTar(t, workTarEntries()...)}); ErrCode(err) != "dock_name_taken" {
		t.Fatalf("err %v", err)
	}
	if _, err := e.ImportWorkTarQEMU(ctx, WorkTarQEMU{Name: "x", Image: "nope", TarPath: writeTar(t, workTarEntries()...)}); err == nil {
		t.Fatal("an unknown base image must fail")
	}
	if docks, _ := e.List(ctx); len(docks) != 1 {
		t.Fatalf("docks %v", docks)
	}
}

func TestQEMUImportIncompleteIsRefused(t *testing.T) {
	e, drv, _ := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.New(ctx, Spec{Name: "crashed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "crashed", false); err != nil {
		t.Fatal(err)
	}
	rec := mustLookup(t, e, "crashed")
	if err := os.WriteFile(e.file(rec.ID, gvImportingMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	starts := len(drv.starts)
	if _, err := e.Resume(ctx, "crashed"); ErrCode(err) != "import_incomplete" || !strings.Contains(err.Error(), "dock rm crashed") {
		t.Fatalf("resume: %v", err)
	}
	if _, err := e.Branch(ctx, "crashed", 1, "", Spec{}); ErrCode(err) != "import_incomplete" {
		t.Fatalf("branch: %v", err)
	}
	if _, err := e.Flatten(ctx, "crashed", t.TempDir(), false); ErrCode(err) != "import_incomplete" {
		t.Fatalf("flatten (push): %v", err)
	}
	if len(drv.starts) != starts || len(drv.flattens) != 0 {
		t.Fatal("a refused dock must not start or flatten")
	}
	if docks, _ := e.List(ctx); len(docks) != 1 || docks[0].State != "import_incomplete" {
		t.Fatalf("ls: %v", docks)
	}
	found := false
	for _, c := range e.Doctor(ctx) {
		if c.Name == "dock crashed" && !c.OK {
			found = true
		}
	}
	if !found {
		t.Fatal("doctor must report the dock")
	}
	if err := e.Remove(ctx, "crashed"); err != nil {
		t.Fatal(err)
	}
}

func TestDecompressWorkFileNamesTheWorkArchive(t *testing.T) {
	dir := t.TempDir()
	zst := filepath.Join(dir, "a.zst")
	if err := os.WriteFile(zst, zstdBytes(t, make([]byte, 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	err := DecompressWorkFile(context.Background(), zst, filepath.Join(dir, "out"), 100)
	if err == nil || !strings.Contains(err.Error(), "the decompressed work archive is larger than") {
		t.Fatalf("err %v", err)
	}
	err = DecompressFile(context.Background(), zst, filepath.Join(dir, "out2"), 100)
	if err == nil || !strings.Contains(err.Error(), "the decompressed disk is larger than") {
		t.Fatalf("err %v", err)
	}
}
