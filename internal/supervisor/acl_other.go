//go:build !linux

package supervisor

import (
	"errors"
	"os"
)

// aclOf is the access control list of fh that is kept with a write: here
// none is.
func aclOf(*os.File) (string, error) { return "", nil }

func setACL(_ *os.File, acl string) error {
	if acl != "" {
		return errors.ErrUnsupported
	}
	return nil
}
