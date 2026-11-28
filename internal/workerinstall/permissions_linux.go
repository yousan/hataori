package workerinstall

// Linux POSIX ACL write grants are bounded by the group-class mode mask.
// owned/secureAncestors already refuse effective group/other write permission.
func platformACL(string) error { return nil }
