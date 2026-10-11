//go:build windows

package local

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// freeBytes is the space available to the current user on dir's volume.
func freeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// markSparse flags f sparse so zero ranges that are never written take no space on NTFS.
// A volume without sparse support (FAT, exFAT) returns an error; callers ignore it.
func markSparse(f *os.File) error {
	var n uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil)
}

var procGetCompressedFileSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// allocatedBytes is the space a file takes on disk (a sparse file counts only its written
// ranges), or 0.
func allocatedBytes(path string) int64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != syscall.Errno(0) {
		return 0
	}
	return int64(high)<<32 | int64(uint32(low))
}
