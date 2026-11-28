package nativehttp

import "os"

// OpenPrivateRoot applies the native transport's owner, mode, ACL, link and
// ancestor checks to desktop-managed state too. It never creates or repairs a
// directory. Callers must keep all descendant access below the returned Root.
func OpenPrivateRoot(path string) (*os.Root, error) { return openPrivateRoot(path) }

// OpenPrivateDir opens one checked child, optionally creating it privately.
func OpenPrivateDir(parent *os.Root, name string, create bool) (*os.Root, error) {
	return privateDir(parent, name, create)
}

// OpenPrivateFile checks a regular single-link 0600 child before and after
// opening. Desktop state cannot silently follow links or repair permissions.
func OpenPrivateFile(parent *os.Root, name string, flags int) (*os.File, error) {
	return privateFile(parent, name, flags)
}
