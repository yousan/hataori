package chat

import (
	"fmt"
	"os"
)

// openedPath returns the path of the file f, as the kernel names it.
func openedPath(f *os.File) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
