package workerinstall

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// getattrlist is a native structured query; no ls/chmod output is parsed.
// FSOPT_REPORT_FULLSIZE ensures a truncated result is refused by safeDarwinACL.
func platformACL(path string) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	attrs := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, 8192)
	_, _, errno := syscall.Syscall6(unix.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), unix.FSOPT_NOFOLLOW|unix.FSOPT_REPORT_FULLSIZE, 0)
	runtime.KeepAlive(p)
	runtime.KeepAlive(&attrs)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return errno
	}
	if err := safeDarwinACL(buffer); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return syscall.ESTALE
	}
	return nil
}
