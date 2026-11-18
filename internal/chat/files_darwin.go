package chat

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// openedPath returns the path of the file f, as the kernel names it.
func openedPath(f *os.File) (string, error) {
	buf := make([]byte, 1024) // MAXPATHLEN, the size F_GETPATH writes at most
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	n := bytes.IndexByte(buf, 0)
	if n < 0 {
		n = len(buf)
	}
	return string(buf[:n]), nil
}
