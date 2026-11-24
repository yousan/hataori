package host

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// StartFile, beside TerminalLog of a terminal another holder holds, is the
// position of the log's first byte: 0 until the log is trimmed. Its lock
// is held while the log is trimmed, so that a reader takes the log and its
// start of one moment.
const StartFile = "terminal.start"

// pipeLock, beside TerminalLog, is held by a Pipe for as long as it runs.
const pipeLock = "terminal.pipe.lock"

// pipeWait bounds how long ResetPipe waits for an earlier Pipe to end.
const pipeWait = 10 * time.Second

// ResetPipe starts the log of a terminal another holder holds afresh, at
// position 0, before anything writes to it: a reader never takes an
// earlier run's log for this one's. It waits for the Pipe of an earlier
// run to end first, which may still be writing what its terminal left,
// and fails when it does not end in time.
func ResetPipe(stateDir string) error {
	f, err := os.OpenFile(filepath.Join(stateDir, pipeLock), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for deadline := time.Now().Add(pipeWait); ; time.Sleep(20 * time.Millisecond) {
		if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("host: the pipe of an earlier run of this terminal still runs")
		}
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if err := os.WriteFile(filepath.Join(stateDir, TerminalLog), nil, 0o600); err != nil {
		return err
	}
	return writeFile(filepath.Join(stateDir, StartFile), "0")
}

// PipeRuns reports whether a Pipe of stateDir runs; it says no when it
// cannot tell.
func PipeRuns(stateDir string) bool {
	f, err := os.Open(filepath.Join(stateDir, pipeLock))
	if err != nil {
		return false
	}
	defer f.Close()
	return errors.Is(unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB), unix.EWOULDBLOCK)
}

// lockStart takes the lock of stateDir's StartFile, shared or not.
func lockStart(stateDir string, how int) func() {
	return lockFile(filepath.Join(stateDir, StartFile+".lock"), how)
}

// lockFile takes the lock of the file path, made when missing, and returns
// its unlock; it does nothing when the file cannot be opened.
func lockFile(path string, how int) func() {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}
	}
	if unix.Flock(int(f.Fd()), how) != nil {
		f.Close()
		return func() {}
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }
}

// Pipe writes r, the raw output of an agent's terminal that another holder
// holds, into stateDir's TerminalLog, bounded as the host's own is:
// `hataori pipe`, which tmux's pipe-pane -O runs (design, section 4.3). The
// holder starts the log afresh first (ResetPipe), so positions count from
// the agent's first byte; StartFile says where the kept bytes begin. It
// returns when r ends.
func Pipe(stateDir string, r io.Reader) error {
	defer lockFile(filepath.Join(stateDir, pipeLock), unix.LOCK_EX)()
	start := filepath.Join(stateDir, StartFile)
	l, err := openBounded(filepath.Join(stateDir, TerminalLog), DefaultLogLimit)
	if err != nil {
		return err
	}
	defer l.Close()
	l.lock = func() func() { return lockStart(stateDir, unix.LOCK_EX) }
	l.trimmed = func(at uint64) { writeFile(start, strconv.FormatUint(at, 10)) }
	_, err = io.Copy(l, r)
	return err
}

// PipeSince returns what a Pipe of stateDir wrote after position from, as
// far as the log still keeps it, and the position at its end: as Screen
// does for the host's own terminal.
func PipeSince(stateDir string, from uint64) ([]byte, uint64) {
	unlock := lockStart(stateDir, unix.LOCK_SH)
	b, err := os.ReadFile(filepath.Join(stateDir, TerminalLog))
	sb, _ := os.ReadFile(filepath.Join(stateDir, StartFile))
	unlock()
	if err != nil {
		return nil, from
	}
	start, _ := strconv.ParseUint(string(bytes.TrimSpace(sb)), 10, 64)
	end := start + uint64(len(b))
	switch {
	case from >= end:
		return nil, end
	case from < start:
		return b, end
	}
	return b[from-start:], end
}
