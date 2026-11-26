//go:build !linux && !darwin

package host

import "errors"

func socketPeerPID(uintptr) (int, error) {
	return 0, errors.New("attached terminal identity is unavailable on this platform")
}
