package nativehttp

import "errors"

// ErrACLUnavailable refuses the native transport on a build that cannot
// establish the platform's ACL boundary. It never weakens browser sign-in.
var ErrACLUnavailable = errors.New("native HTTP: platform ACL verification is unavailable")
