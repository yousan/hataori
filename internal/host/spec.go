// Package host is the session host: a small process that runs one command
// on a pseudo-terminal or on pipes, outlives whoever started it, records
// what the command reports, and accepts input on a Unix socket.
//
// A host can also run no command (Spec.Relay), for a terminal that another
// holder holds: it then only relays hook calls, until it is told to stop.
//
// The host interprets nothing. It knows a command, a directory, an
// environment and a few files in a state directory:
//
//	host.json      the Spec, written by Start
//	host.pid       the host's process id
//	command.pid    the command's process id, which is also its process group; not for a relay
//	events.ndjson  on pipes, every line of the command's standard output; and every hook call
//	terminal.log   on a terminal, its raw output, bounded
//	stderr.log     on pipes, its standard error, bounded
//	host.sock      the socket, while the command runs, or until a relay is stopped
//	status.json    how the command ended, written when it has; for a relay, when it was stopped
package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Spec says what the host runs.
type Spec struct {
	Argv     []string `json:"argv"`
	Dir      string   `json:"dir"`
	Env      []string `json:"env"` // exactly the command's environment; nothing is inherited
	Terminal bool     `json:"terminal"`
	Rows     uint16   `json:"rows,omitempty"`      // terminal size; 24 when zero
	Cols     uint16   `json:"cols,omitempty"`      // terminal size; 80 when zero
	LogLimit int64    `json:"log_limit,omitempty"` // bound of terminal.log and stderr.log; DefaultLogLimit when zero
	// Relay runs no command: the host only listens on its socket, appends
	// each hook call to events.ndjson and holds the calls that wait, for a
	// terminal that another holder holds. It ends when told to stop. Argv
	// is then empty and Terminal false.
	Relay bool `json:"relay,omitempty"`
}

// errRelayCommand refuses a Spec that is a relay and names a command.
var errRelayCommand = errors.New("host: a relay runs no command and has no terminal")

// Size returns the terminal's size, with the defaults for a zero field.
func (s Spec) Size() (cols, rows uint16) {
	cols, rows = s.Cols, s.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return cols, rows
}

// ReadSpec returns the Spec written into stateDir for its host. ok is false
// when none was written there yet.
func ReadSpec(stateDir string) (s Spec, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(stateDir, SpecFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("host: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, fmt.Errorf("host: %s: %w", SpecFile, err)
	}
	return s, true, nil
}

// DefaultLogLimit bounds the raw logs when Spec.LogLimit is zero.
const DefaultLogLimit = 8 << 20

// Files in the state directory.
const (
	SpecFile     = "host.json"
	HostPIDFile  = "host.pid"
	PIDFile      = "command.pid"
	EventsFile   = "events.ndjson"
	TerminalLog  = "terminal.log"
	StderrLog    = "stderr.log"
	SocketFile   = "host.sock"
	StatusFile   = "status.json"
	pointerFile  = "host.sock.path"
	maxSocketLen = 100
)

// Status is how the command ended.
type Status struct {
	Exited   bool   `json:"exited"`           // it ended by itself or by a signal
	Code     int    `json:"code"`             // exit code, or -1 when a signal ended it
	Signal   string `json:"signal,omitempty"` // the signal that ended it
	Stopped  bool   `json:"stopped,omitempty"`
	StopNote string `json:"stop_note,omitempty"` // which signals a stop sent
	Error    string `json:"error,omitempty"`     // the command could not be started
	// Relay says the host ran no command (Spec.Relay): it relayed hook
	// calls until it was told to stop, and had nothing to signal. Exited is
	// then false and Stopped true. A relay refused at start records Error
	// only.
	Relay bool `json:"relay,omitempty"`
}

// Command is how to run the host program itself: the hataori binary with
// the arguments that select `hataori host`. The state directory is
// appended.
type Command struct {
	Path string
	Args []string
	Env  []string // the host's own environment; the command never sees it
}

// Start writes spec into stateDir and starts the host there, in a session
// of its own, detached from the caller: it keeps running when the caller
// exits or is killed. It returns the host's process id once the host
// listens on its socket.
func Start(c Command, stateDir string, spec Spec) (int, error) {
	if spec.Relay && (len(spec.Argv) > 0 || spec.Terminal) {
		return 0, errRelayCommand
	}
	if len(spec.Argv) == 0 && !spec.Relay {
		return 0, errors.New("host: empty command")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return 0, fmt.Errorf("host: %w", err)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(stateDir, SpecFile), b, 0o600); err != nil {
		return 0, fmt.Errorf("host: %w", err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer null.Close()
	cmd := exec.Command(c.Path, append(append([]string{}, c.Args...), stateDir)...)
	cmd.Env = append([]string{}, c.Env...) // nothing from the caller's environment
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("host: start: %w", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	return pid, WaitReady(stateDir)
}

// WaitReady returns once the host of stateDir listens, or has recorded how
// it ended; an error when it recorded why it could not start, or neither
// happened in time.
func WaitReady(stateDir string) error {
	deadline := time.Now().Add(startTimeout)
	for {
		if st, ok, _ := ReadStatus(stateDir); ok {
			if st.Error != "" {
				return fmt.Errorf("host: %s", st.Error)
			}
			return nil // the command already ended
		}
		if c, err := net.Dial("unix", SocketPath(stateDir)); err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host: not ready after %v", startTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startTimeout bounds how long Start waits for the host to listen.
const startTimeout = 10 * time.Second

// SocketPath returns the socket of the host in stateDir. When
// <stateDir>/host.sock is too long for a Unix socket, it is a short path in
// the user's runtime directory, and the host records it in
// <stateDir>/host.sock.path.
func SocketPath(stateDir string) string {
	if b, err := os.ReadFile(filepath.Join(stateDir, pointerFile)); err == nil {
		return strings.TrimSpace(string(b))
	}
	return defaultSocket(stateDir)
}

func defaultSocket(stateDir string) string {
	p := filepath.Join(stateDir, SocketFile)
	if len(p) <= maxSocketLen {
		return p
	}
	// The same path in every process, whatever its environment: the host
	// runs with none, and the caller's HookEnv must name the same socket.
	abs, _ := filepath.Abs(stateDir)
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join("/tmp", "hataori-host-"+hex.EncodeToString(sum[:8]), SocketFile)
}

// ReadStatus returns the recorded status, and false while the command has
// not ended.
func ReadStatus(stateDir string) (Status, bool, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, StatusFile))
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, false, fmt.Errorf("host: %s: %w", StatusFile, err)
	}
	return s, true, nil
}
