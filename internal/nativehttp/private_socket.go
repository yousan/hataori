package nativehttp

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// DialOwnerSocket opens a caller-computed Unix endpoint below a checked private
// directory. It is for read-only desktop compatibility probes, not discovery or
// HTTP Caller assignment. A stored pointer must never supply the argument.
// Unknown Darwin ACL support still fails closed, including in non-cgo builds.
func DialOwnerSocket(ctx context.Context, path string) (net.Conn, error) {
	parent, err := openPrivateRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	name := filepath.Base(path)
	path = filepath.Join(parent.Name(), name)
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := private(before, fs.ModeSocket, 0o600); err != nil {
		return nil, err
	}
	if err := checkACL(path); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	refuse := func(err error) (net.Conn, error) { conn.Close(); return nil, err }
	if err := checkPeer(conn.(*net.UnixConn)); err != nil {
		return refuse(err)
	}
	after, err := parent.Lstat(name)
	if err != nil {
		return refuse(err)
	}
	if err := private(after, fs.ModeSocket, 0o600); err != nil {
		return refuse(err)
	}
	if !os.SameFile(before, after) {
		return refuse(errors.New("desktop: host endpoint changed while connecting"))
	}
	if err := checkACL(path); err != nil {
		return refuse(err)
	}
	return conn, nil
}
