package worker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yousan/hataori/internal/workerproto"
	"golang.org/x/sys/unix"
)

const (
	registrationFile = "state/worker/registration.json"
	journalFile      = "state/worker/journal.json"
	lockFile         = "state/worker/resident.lock"
	maxJournal       = 16 << 20
)

var ErrBinding = errors.New("worker: installation binding does not match")
var ErrLocked = errors.New("worker: another resident owns this root")

type registration struct {
	Version int                 `json:"version"`
	Binding workerproto.Binding `json:"binding"`
	Device  uint64              `json:"device"`
	Inode   uint64              `json:"inode"`
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // An installation/run must never fall back to a predictable ID.
	}
	return hex.EncodeToString(b[:])
}

func private(info os.FileInfo, directory bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode()&0o077 != 0 || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() {
		return errors.New("worker: expected a private, owned regular file or directory")
	}
	return nil
}

func openRoot(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", errors.New("worker: root must be a canonical absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if err := private(info, true); err != nil {
		return nil, "", err
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(physical)
	// Keep the operator's canonical spelling for deterministic socket names;
	// os.Root itself is opened on the resolved directory and pins its identity.
	return r, path, err
}

func privateDir(root *os.Root, name string) error {
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		if err := root.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := root.Lstat(p)
		if err != nil {
			return err
		}
		if err := private(info, true); err != nil {
			return err
		}
	}
	return nil
}

func readPrivate(root *os.Root, name string, max int64, value any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if err := private(info, false); err != nil {
		return err
	}
	if info.Size() > max {
		return errors.New("worker: state file exceeds its limit")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("worker: state identity changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return errors.New("worker: cannot read bounded state")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("worker: invalid state: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("worker: trailing state data")
	}
	return nil
}

func syncDir(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// writeState never follows an existing destination symlink. Replacement is
// only for our private journal, never a workspace or an operator config.
func writeState(root *os.Root, name string, value any, replace bool) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > maxJournal {
		return errors.New("worker: cannot encode bounded state")
	}
	if info, err := root.Lstat(name); err == nil {
		if !replace {
			return os.ErrExist
		}
		if err := private(info, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".write-"+newID())
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temp) // Only this call's newly created scratch file.
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if replace {
		err = root.Rename(temp, name)
	} else {
		// Link is exclusive even if another initializer raced the earlier stat.
		err = root.Link(temp, name)
	}
	if err != nil {
		return err
	}
	return syncDir(root, filepath.Dir(name))
}

// Initialize explicitly registers an empty root. Existing data is never
// adopted, emptied, repaired or rebound. Repeating the same registration is
// harmless; a different main is refused.
func Initialize(path, main string) (workerproto.Binding, error) {
	if !workerproto.ValidID(main) {
		return workerproto.Binding{}, ErrBinding
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return workerproto.Binding{}, err
	}
	root, _, err := openRoot(path)
	if err != nil {
		return workerproto.Binding{}, err
	}
	defer root.Close()
	var reg registration
	if err := readPrivate(root, registrationFile, 4096, &reg); err == nil {
		if reg.Version != 1 || reg.Binding.Main != main || !workerproto.ValidID(reg.Binding.Worker) || !rootIdentity(root, reg) {
			return workerproto.Binding{}, ErrBinding
		}
		return reg.Binding, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workerproto.Binding{}, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return workerproto.Binding{}, err
	}
	entries, err := dir.ReadDir(1)
	dir.Close()
	if len(entries) != 0 || err != io.EOF {
		return workerproto.Binding{}, errors.New("worker: unregistered root is not empty; refusing to adopt data")
	}
	for _, dir := range []string{"state/worker", "state/sessions", "config", "workspaces"} {
		if err := privateDir(root, dir); err != nil {
			return workerproto.Binding{}, err
		}
	}
	info, err := root.Stat(".")
	if err != nil {
		return workerproto.Binding{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return workerproto.Binding{}, ErrBinding
	}
	reg = registration{Version: 1, Binding: workerproto.Binding{Main: main, Worker: newID()}, Device: uint64(st.Dev), Inode: uint64(st.Ino)}
	if err := writeState(root, registrationFile, reg, false); err != nil {
		return workerproto.Binding{}, err
	}
	if err := writeState(root, journalFile, emptyJournal(reg.Binding), false); err != nil {
		return workerproto.Binding{}, err
	}
	if err := syncDir(root, "state"); err != nil {
		return workerproto.Binding{}, err
	}
	if err := syncDir(root, "."); err != nil {
		return workerproto.Binding{}, err
	}
	return reg.Binding, nil
}

func takeLock(root *os.Root) (*os.File, error) {
	if info, err := root.Lstat(lockFile); err == nil {
		if err := private(info, false); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

func rootIdentity(root *os.Root, reg registration) bool {
	info, err := root.Stat(".")
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == reg.Device && uint64(st.Ino) == reg.Inode
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return f.Close()
}
