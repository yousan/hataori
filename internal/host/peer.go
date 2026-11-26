package host

import (
	"fmt"
	"net"
)

func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var lookup error
	if err := raw.Control(func(fd uintptr) { pid, lookup = socketPeerPID(fd) }); err != nil {
		return 0, err
	}
	if lookup != nil {
		return 0, lookup
	}
	if pid < 1 {
		return 0, fmt.Errorf("invalid attached terminal PID %d", pid)
	}
	return pid, nil
}
