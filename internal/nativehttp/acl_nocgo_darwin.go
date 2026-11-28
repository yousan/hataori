//go:build darwin && !cgo

package nativehttp

// Cross-builds remain possible without cgo, but native access must refuse
// until this build can inspect ACLs. The TCP dashboard remains separate.
func checkACL(path string) error { return ErrACLUnavailable }
