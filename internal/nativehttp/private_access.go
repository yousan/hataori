package nativehttp

import (
	"errors"
	"os"
	"path/filepath"
)

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

// OpenPrivateDatabase reads an existing regular database below a checked 0700
// parent. Legacy SQLite files can be 0644: private ancestors still exclude
// other users. Group/other writes, executability, foreign owners, links and
// unknown ACLs are refused; permissions are never changed.
func OpenPrivateDatabase(parent *os.Root, name string) (*os.File, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode().Perm()&0o133 != 0 || private(before, 0, before.Mode().Perm()) != nil {
		return nil, errors.New("desktop: untrusted database")
	}
	if err := checkACL(filepath.Join(parent.Name(), name)); err != nil {
		return nil, err
	}
	f, err := parent.Open(name)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, errors.New("desktop: database changed while opening")
	}
	return f, nil
}

// OpenPrivateExecutable opens an immutable versioned desktop binary below a
// checked private directory. Execute permission does not relax owner/ACL/link
// checks, and the opened identity must still match its name.
func OpenPrivateExecutable(parent *os.Root, name string) (*os.File, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := private(before, 0, 0o700); err != nil {
		return nil, err
	}
	if err := checkACL(filepath.Join(parent.Name(), name)); err != nil {
		return nil, err
	}
	f, err := parent.Open(name)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, errors.New("desktop: executable changed while opening")
	}
	return f, nil
}

// OpenOwnerDirectory is for the user's existing LaunchAgents directory, whose
// listing need not be secret. It still forbids another user redirecting a
// registration: ancestors, owner, ACL, spelling and opened identity are checked.
// Instance/config/runtime directories must keep using OpenPrivateRoot instead.
func OpenOwnerDirectory(path string) (*os.Root, error) {
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
	before, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if before.Mode().Perm()&0o022 != 0 || private(before, os.ModeDir, before.Mode().Perm()) != nil {
		return nil, errors.New("desktop: untrusted registration directory")
	}
	if err := checkACL(abs); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		return nil, errors.New("desktop: registration directory changed")
	}
	return r, nil
}
