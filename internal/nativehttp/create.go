package nativehttp

import (
	"os"
	"path/filepath"
)

// CreatePrivateLeaf creates only a missing leaf below existing trusted
// ancestors. Existing paths are checked, never chmodded, replaced or repaired.
// Desktop setup checks spelling collisions before calling this function.
func CreatePrivateLeaf(path string) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	if err := trustedAncestors(abs); err != nil {
		return nil, err
	}
	if err := os.Mkdir(abs, 0o700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	return openPrivateRoot(abs)
}
