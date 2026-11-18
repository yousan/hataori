package chat

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openedPath returns the path of the file f, as the kernel names it.
// macOS has two names for a file behind a firmlink (/private/var and
// /System/Volumes/Data/private/var); the one that is real is returned
// when the kernel gives it.
func openedPath(f *os.File, real string) (string, error) {
	p, err := getPath(f, unix.F_GETPATH)
	if err != nil || p == real {
		return p, err
	}
	return getPath(f, unix.F_GETPATH_NOFIRMLINK)
}

func getPath(f *os.File, cmd uintptr) (string, error) {
	buf := make([]byte, unix.PathMax) // the most F_GETPATH writes
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), cmd, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	n := bytes.IndexByte(buf, 0)
	if n < 0 {
		n = len(buf)
	}
	return string(buf[:n]), nil
}
