package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// LockFile is held by the running host of a state directory, for as long
// as the host runs, so that one state directory never has two hosts.
const LockFile = "host.lock"

// ErrHostRunning is returned by Run when another host holds the state
// directory.
var ErrHostRunning = errors.New("host: another host runs in this state directory")

// lock takes the state directory's host lock, or returns ErrHostRunning.
// The lock goes when the process ends.
func lock(stateDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(stateDir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("host: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrHostRunning
		}
		return nil, fmt.Errorf("host: lock: %w", err)
	}
	return f, nil
}

// unlock releases the lock f holds and closes f. The lock is released
// first: it belongs to the open file, and a process started a moment ago
// has a copy of the descriptor until it execs, so closing alone would
// leave the lock held by that copy for that moment.
func unlock(f *os.File) {
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	f.Close()
}

// probed, when a test sets it, is called with the probe's file while
// Running holds the lock.
var probed func(*os.File)

// Running reports whether a host holds stateDir. A host that is starting
// and has not taken the lock yet is not seen; it exits if it finds the
// lock taken by another.
func Running(stateDir string) bool {
	f, err := lock(stateDir)
	if err != nil {
		return errors.Is(err, ErrHostRunning)
	}
	if probed != nil {
		probed(f)
	}
	unlock(f)
	return false
}
