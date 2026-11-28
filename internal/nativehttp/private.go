package nativehttp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// private checks the object itself; callers must use Lstat or an open
// descriptor, never Stat on an untrusted pathname.
func private(info fs.FileInfo, kind fs.FileMode, perm fs.FileMode) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Type() != kind || info.Mode().Perm() != perm || info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return errors.New("object is not of the required type, owner and private mode")
	}
	if kind != fs.ModeDir && stat.Nlink != 1 {
		return errors.New("object has multiple links")
	}
	return nil
}

// trustedAncestors refuses a pathname that another ordinary OS user could
// redirect. A root-owned sticky temporary directory is an allowed parent.
// EvalSymlinks is used only for ancestors: the private leaf must not be a link.
func trustedAncestors(path string) error {
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || int(stat.Uid) != os.Geteuid() && stat.Uid != 0 {
			return fmt.Errorf("untrusted ancestor %s", p)
		}
		if info.Mode().Perm()&0o022 != 0 && !(stat.Uid == 0 && info.Mode()&fs.ModeSticky != 0) {
			return fmt.Errorf("writable ancestor %s", p)
		}
		if err := checkACL(p); err != nil {
			return err
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func openPrivateRoot(path string) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(abs))
	if err := trustedAncestors(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := private(info, fs.ModeDir, 0o700); err != nil {
		return nil, fmt.Errorf("private directory %s: %w", path, err)
	}
	if err := checkACL(path); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		r.Close()
		return nil, errors.New("private directory changed while opening")
	}
	return r, nil
}

func privateDir(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := private(info, fs.ModeDir, 0o700); err != nil {
		return nil, fmt.Errorf("private directory %s: %w", name, err)
	}
	if err := checkACL(filepath.Join(parent.Name(), name)); err != nil {
		return nil, err
	}
	r, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		r.Close()
		return nil, errors.New("private directory changed while opening")
	}
	return r, nil
}

func privateFile(root *os.Root, name string, flags int) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil && !(errors.Is(err, fs.ErrNotExist) && flags&os.O_CREATE != 0) {
		return nil, err
	}
	if before != nil {
		if err := private(before, 0, 0o600); err != nil {
			return nil, err
		}
	}
	// os.Root may resolve an in-root link after OpenFile returns ELOOP.
	// O_NOFOLLOW alone therefore does not establish the no-link contract.
	f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = private(info, 0, 0o600)
	}
	if err == nil {
		err = checkACL(filepath.Join(root.Name(), name))
	}
	if err == nil {
		current, statErr := root.Lstat(name)
		if statErr != nil {
			err = statErr
		} else if err = private(current, 0, 0o600); err == nil && (!os.SameFile(current, info) || before != nil && !os.SameFile(before, info)) {
			err = errors.New("private file changed while opening")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
