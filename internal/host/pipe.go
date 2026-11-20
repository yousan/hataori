package host

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// StartFile, beside TerminalLog of a terminal another holder holds, is the
// position of the log's first byte: 0 until the log is trimmed. Its lock
// is held while the log is trimmed, so that a reader takes the log and its
// start of one moment.
const StartFile = "terminal.start"

// ResetPipe starts the log of a terminal another holder holds afresh, at
// position 0, before anything writes to it: a reader never takes an
// earlier run's log for this one's.
func ResetPipe(stateDir string) error {
	if err := os.WriteFile(filepath.Join(stateDir, TerminalLog), nil, 0o600); err != nil {
		return err
	}
	return writeFile(filepath.Join(stateDir, StartFile), "0")
}

// lockStart takes the lock of stateDir's StartFile, shared or not.
func lockStart(stateDir string, how int) func() {
	f, err := os.OpenFile(filepath.Join(stateDir, StartFile+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
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
