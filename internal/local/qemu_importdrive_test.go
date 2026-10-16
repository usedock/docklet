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
	drives := valuesOf(plain, "-drive")
	if len(drives) != 1 || drives[0] != "file=/d/disk.qcow2,if=virtio,format=qcow2" || strings.Contains(strings.Join(plain, " "), ImportDriveSerial) {
		t.Fatalf("a normal boot has the one root drive and no import drive: %v", plain)
	}
	s := spec()
	s.ImportDrive = "/d/work.tar.clean"
	for _, goarch := range []string{"amd64", "arm64"} {
		args := BuildArgv(s, goarch, "kvm")
		drives := valuesOf(args, "-drive")
		if len(drives) != 2 || drives[0] != "file=/d/disk.qcow2,if=none,id=dockroot,format=qcow2" ||
			drives[1] != "file=/d/work.tar.clean,if=none,id=dockimport,format=raw,readonly=on" {
			t.Fatalf("%s: drives %v", goarch, drives)
		}
		// Both disks are explicit devices and the root one comes first, so it is vda (root=/dev/vda):
		// a default "if=virtio" disk would be created after the import device and take vdb.
		devs := valuesOf(args, "-device")
		root := slices.Index(devs, "virtio-blk-pci,drive=dockroot")
		imp := slices.Index(devs, "virtio-blk-pci,drive=dockimport,serial="+ImportDriveSerial)
		if root < 0 || imp < 0 || root > imp {
			t.Fatalf("%s: devices %v", goarch, devs)
		}
		if strings.Contains(strings.Join(args, " "), "if=virtio") {
			t.Fatalf("%s: no default-bus drive may remain: %v", goarch, args)
		}
	}
	s.ImportDrive = `C:\Users\a,b\work.tar`
	if got := valuesOf(BuildArgv(s, "amd64", "whpx"), "-drive")[1]; !strings.HasPrefix(got, `file=C:\Users\a,,b\work.tar,`) {
		t.Fatalf("commas in the path must be doubled: %q", got)
	}
}
