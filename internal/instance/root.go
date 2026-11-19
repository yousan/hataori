package instance

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile is the lock's path relative to the root.
const LockFile = "state/lock"

// The directories Open creates when they are missing.
var dirs = []string{"state", "workspaces", "mirrors"}

// Instance is an open instance. It holds the root's lock until Close.
type Instance struct {
	Path   string // the root, as given to Open
	Root   *os.Root
	Config *Config
	lock   *os.File
}

// Open reads the config under root, creates the directories that are
// missing and takes the instance's lock. Every file it opens is opened
// through an os.Root, so nothing outside root is read or written.
func Open(root string) (*Instance, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("instance: open root: %w", err)
	}
	in := &Instance{Path: root, Root: r}
	if err := in.open(); err != nil {
		in.Close()
		return nil, err
	}
	return in, nil
}

func (in *Instance) open() error {
	cfg, err := readConfig(in.Root)
	if err != nil {
		return err
	}
	in.Config = cfg
	for _, d := range dirs {
		err := in.Root.Mkdir(d, 0o700)
		if errors.Is(err, fs.ErrExist) {
			var st fs.FileInfo
			if st, err = in.Root.Stat(d); err == nil && !st.IsDir() {
				err = errors.New("exists and is not a directory")
			}
		}
		if err != nil {
			return fmt.Errorf("instance: create %s: %w", d, err)
		}
	}
	f, err := in.Root.OpenFile(LockFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("instance: open lock %s: %w", filepath.Join(in.Path, LockFile), err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("instance: another hataori holds the lock %s", filepath.Join(in.Path, LockFile))
		}
		return fmt.Errorf("instance: lock %s: %w", filepath.Join(in.Path, LockFile), err)
	}
	in.lock = f
	return nil
}

func readConfig(r *os.Root) (*Config, error) {
	f, err := r.Open(ConfigFile)
	if err != nil {
		return nil, fmt.Errorf("instance: read config: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("instance: read config: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("instance: config %s is not a regular file", ConfigFile)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("instance: config %s is accessible to other users (mode %04o); run chmod 600 on it", ConfigFile, st.Mode().Perm())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("instance: read config: %w", err)
	}
	return ParseConfig(data)
}

// Close releases the lock and the root.
func (in *Instance) Close() error {
	if in.lock != nil {
		// Unlocked before it is closed: a process started a moment ago has a
		// copy of the descriptor until it execs, and closing ours would
		// leave the lock with that copy.
		syscall.Flock(int(in.lock.Fd()), syscall.LOCK_UN)
		in.lock.Close()
		in.lock = nil
	}
	return in.Root.Close()
}
