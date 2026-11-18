//go:build !linux && !darwin

package chat

import (
	"errors"
	"os"
)

// openedPath cannot name the file f here, so every file is refused.
func openedPath(*os.File) (string, error) {
	return "", errors.New("the system cannot name an open file")
}
