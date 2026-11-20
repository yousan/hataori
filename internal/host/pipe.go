package host

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// StartFile, beside TerminalLog of a terminal another holder holds, is the
// position of the log's first byte: 0 until the log is trimmed.
const StartFile = "terminal.start"

// Pipe writes r, the raw output of an agent's terminal that another holder
// holds, into stateDir's TerminalLog, bounded as the host's own is:
// `hataori pipe`, which tmux's pipe-pane -O runs (design, section 4.3). A
// run starts the log afresh, so its positions count from the agent's first
// byte; StartFile says where the kept bytes begin. It returns when r ends.
func Pipe(stateDir string, r io.Reader) error {
	path := filepath.Join(stateDir, TerminalLog)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	start := filepath.Join(stateDir, StartFile)
	if err := writeFile(start, "0"); err != nil {
		return err
	}
	l, err := openBounded(path, DefaultLogLimit)
	if err != nil {
		return err
	}
	defer l.Close()
	l.trimmed = func(at uint64) { writeFile(start, strconv.FormatUint(at, 10)) }
	_, err = io.Copy(l, r)
	return err
}

// PipeSince returns what a Pipe of stateDir wrote after position from, as
// far as the log still keeps it, and the position at its end: as Screen
// does for the host's own terminal.
func PipeSince(stateDir string, from uint64) ([]byte, uint64) {
	b, err := os.ReadFile(filepath.Join(stateDir, TerminalLog))
	if err != nil {
		return nil, from
	}
	sb, _ := os.ReadFile(filepath.Join(stateDir, StartFile))
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
