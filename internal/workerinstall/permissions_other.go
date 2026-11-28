//go:build !linux && !darwin

package workerinstall

import "errors"

func platformACL(string) error {
	return errors.New("worker install: unsupported filesystem permission model")
}
