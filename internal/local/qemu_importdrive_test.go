package local

import (
	"slices"
	"strings"
	"testing"
)

// valuesOf returns the value after every occurrence of flag.
func valuesOf(args []string, flag string) []string {
	var vals []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			vals = append(vals, args[i+1])
		}
	}
	return vals
}

func TestArgvImportDriveOnlyOnTheImportBoot(t *testing.T) {
	plain := BuildArgv(spec(), "amd64", "kvm")
	if len(valuesOf(plain, "-drive")) != 1 || strings.Contains(strings.Join(plain, " "), ImportDriveSerial) {
		t.Fatalf("a normal boot has one drive and no import drive: %v", plain)
	}
	s := spec()
	s.ImportDrive = "/d/work.tar.clean"
	for _, goarch := range []string{"amd64", "arm64"} {
		args := BuildArgv(s, goarch, "kvm")
		drives := valuesOf(args, "-drive")
		if len(drives) != 2 || !strings.HasPrefix(drives[0], "file=/d/disk.qcow2,") || !strings.Contains(drives[0], "format=qcow2") {
			t.Fatalf("%s: the root disk must stay the first drive: %v", goarch, drives)
		}
		if drives[1] != "file=/d/work.tar.clean,if=none,id=dockimport,format=raw,readonly=on" {
			t.Fatalf("%s: import drive %q", goarch, drives[1])
		}
		// The serial is a device property (the raw driver refuses it), and the device follows the root disk.
		devs := valuesOf(args, "-device")
		if !slices.Contains(devs, "virtio-blk-pci,drive=dockimport,serial="+ImportDriveSerial) {
			t.Fatalf("%s: devices %v", goarch, devs)
		}
	}
	s.ImportDrive = `C:\Users\a,b\work.tar`
	if got := valuesOf(BuildArgv(s, "amd64", "whpx"), "-drive")[1]; !strings.HasPrefix(got, `file=C:\Users\a,,b\work.tar,`) {
		t.Fatalf("commas in the path must be doubled: %q", got)
	}
}
