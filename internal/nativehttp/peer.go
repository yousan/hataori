// Package nativehttp provides the private Unix transport for the owner's
// dashboard. It does not assign HTTP callers or start a daemon.
package nativehttp

import (
	"fmt"
	"net"
	"os"
)

// checkPeer uses the kernel's credentials, never an HTTP header or address.
func checkPeer(conn *net.UnixConn) error {
	uid, err := peerUID(conn)
	if err != nil {
		return fmt.Errorf("native HTTP: peer credentials: %w", err)
	}
	if uid != os.Geteuid() {
		return fmt.Errorf("native HTTP: peer belongs to another OS user")
	}
	return nil
}
