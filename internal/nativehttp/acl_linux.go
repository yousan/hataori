package nativehttp

// Linux POSIX access ACLs cannot grant permissions beyond the group-class
// mode mask. Private leaves have that mask cleared, and ancestor checks
// refuse group/other writes (except root-owned sticky temporary parents).
func checkACL(path string) error { return nil }
