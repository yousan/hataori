package supervisor

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// xattrACL is where Linux keeps a file's access control list, when it has
// one besides its mode.
const xattrACL = "system.posix_acl_access"

// aclOf is the access control list of fh, as Linux keeps it; "" for none.
func aclOf(fh *os.File) (string, error) {
	b := make([]byte, 64<<10) // the most that an attribute may have
	n, err := unix.Fgetxattr(int(fh.Fd()), xattrACL, b)
	switch {
	case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
		return "", nil
	case err != nil:
		return "", err
	}
	return string(b[:n]), nil
}

// setACL gives fh acl, or takes the one it has for "".
func setACL(fh *os.File, acl string) error {
	if acl == "" {
		err := unix.Fremovexattr(int(fh.Fd()), xattrACL)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
			return nil // it has none
		}
		return err
	}
	return unix.Fsetxattr(int(fh.Fd()), xattrACL, []byte(acl), 0)
}
