//go:build darwin

package worker

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func bootID() (string, error) {
	t, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("boot-%d-%d", t.Sec, t.Usec), nil
}

func memoryLoad() (uint64, float64, error) {
	// Real user-session/resource conformance is the #969 gate. Unknown data
	// deliberately prevents admission instead of advertising macOS capacity.
	return 0, 0, unix.ENOTSUP
}
