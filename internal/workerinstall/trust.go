package workerinstall

import (
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// ReadTrustKey reads an independently provisioned public key. Release inputs
// may be untrusted; this enrollment authority must be protected from mutation
// by other users, including through replaceable physical ancestors or ACLs.
func ReadTrustKey(path string) (ed25519.PublicKey, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	if err = secureAncestors(parent); err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(abs))
	i, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.Mode().IsRegular() || st.Uid != 0 && st.Uid != uint32(os.Geteuid()) || i.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("worker install: trust key must be protected from other users")
	}
	if err = checkPlatformACL(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(i, opened) {
		return nil, errors.New("worker install: trust key identity changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, ed25519.PublicKeySize+1))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("worker install: trust key must contain exactly 32 bytes")
	}
	return ed25519.PublicKey(b), nil
}
