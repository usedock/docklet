package local

import (
	"strings"
	"testing"
)

func TestArgvImportDriveOnlyOnTheImportBoot(t *testing.T) {
	plain := strings.Join(BuildArgv(spec(), "amd64", "kvm"), " ")
	if strings.Count(plain, "-drive") != 1 || strings.Contains(plain, ImportDriveSerial) {
		t.Fatalf("a normal boot has one drive and no import drive: %v", plain)
	}
	s := spec()
	s.ImportDrive = "/d/work.tar.clean"
	for _, goarch := range []string{"amd64", "arm64"} {
		args := BuildArgv(s, goarch, "kvm")
		var drives []string
		for i, a := range args {
			if a == "-drive" {
				drives = append(drives, args[i+1])
			}
		}
		if len(drives) != 2 || !strings.HasPrefix(drives[0], "file=/d/disk.qcow2,") || !strings.Contains(drives[0], "format=qcow2") {
			t.Fatalf("%s: the root disk must stay the first drive: %v", goarch, drives)
		}
		if drives[1] != "file=/d/work.tar.clean,if=virtio,format=raw,readonly=on,serial="+ImportDriveSerial {
			t.Fatalf("%s: import drive %q", goarch, drives[1])
		}
	}
	s.ImportDrive = `C:\Users\a,b\work.tar`
	args := BuildArgv(s, "amd64", "whpx")
	if got := args[len(args)-1]; !strings.HasPrefix(got, `file=C:\Users\a,,b\work.tar,`) {
		t.Fatalf("commas in the path must be doubled: %q", got)
	}
}
