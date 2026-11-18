package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// drainTimeout is how long the host keeps reading output after the command
// exited, for bytes still in flight or held by a process the command left.
const drainTimeout = 2 * time.Second

// drainGoneTimeout bounds the same wait once no process of the group is
// left.
const drainGoneTimeout = 30 * time.Second

// host is one running session host.
type host struct {
	dir   string
	spec  Spec
	cmd   *exec.Cmd
	input io.Writer // the terminal or the command's stdin
	inMu  sync.Mutex
	tty   *os.File    // the terminal's controlling side, or nil on pipes
	term  *boundedLog // the terminal's raw log, or nil on pipes
	run   string      // this run of the host; positions in term count from its start
	cols  int
	rows  int

	follows atomic.Int32 // follow requests being served
	ln      *net.UnixListener
	sock    string

	events *lineLog
	calls  callTable
	log    *hostLog      // the host's own record; nil when it could not be opened
	done   chan struct{} // closed when the command has exited

	stopMu   sync.Mutex
	stopOne  sync.Mutex     // one stop at a time
	closing  bool           // the host is shutting down; no new stop starts
	stops    sync.WaitGroup // stops in progress
	stopNote string         // the signals the last stop sent
}

// Run is `hataori host <state-dir>`: it runs the Spec in stateDir until the
// command exits, and returns after recording how it ended.
func Run(stateDir string) (err error) {
	// First of all: a second host for the same state directory leaves
	// everything to the first, and records nothing.
	lk, err := lock(stateDir)
	if err != nil {
		return err
	}
	defer lk.Close()
	hl := openHostLog(stateDir)
	hl.printf("started, pid %d", os.Getpid())
	hl.recordSignals()
	defer func() {
		if err != nil {
			hl.printf("ended: %v", err)
		} else {
			hl.printf("ended")
		}
	}()
	b, err := os.ReadFile(filepath.Join(stateDir, SpecFile))
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	h := &host{dir: stateDir, done: make(chan struct{}), log: hl, run: newRun()}
	if err := json.Unmarshal(b, &h.spec); err != nil {
		return fmt.Errorf("host: %s: %w", SpecFile, err)
	}
	if h.spec.LogLimit <= 0 {
		h.spec.LogLimit = DefaultLogLimit
	}
	if err := writeFile(filepath.Join(stateDir, HostPIDFile), strconv.Itoa(os.Getpid())); err != nil {
		return err
	}
	if h.events, err = openLines(filepath.Join(stateDir, EventsFile)); err != nil {
		return fmt.Errorf("host: %w", err)
	}
	defer h.events.Close()

	// Listen before the command starts, so that its first hook call finds
	// the socket; connections wait in the backlog until serve accepts them.
	if err := h.listen(); err != nil {
		writeStatus(stateDir, Status{Error: err.Error(), Code: -1})
		return err
	}
	readers, err := h.start()
	if err != nil {
		h.ln.Close()
		h.removeSocket()
		writeStatus(stateDir, Status{Error: err.Error(), Code: -1})
		return err
	}
	go h.serve()

	waitErr := h.cmd.Wait()
	close(h.done)
	hl.printf("the command ended: %v", h.cmd.ProcessState)
	// A stop in progress ends the rest of the group first. While the output
	// drains, a stop can still end what the command left behind.
	h.stops.Wait()
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	// With the group gone every writer is closed, so the readers end; the
	// long bound only guards against a descriptor passed elsewhere.
	limit := drainTimeout
	if groupGone(h.cmd.Process.Pid) {
		limit = drainGoneTimeout
	}
	select {
	case <-drained:
	case <-time.After(limit):
	}
	// The terminal log is closed now; let its followers send what is left
	// and the end, for a bounded time.
	for deadline := time.Now().Add(followFlushTimeout); h.follows.Load() > 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	h.stopMu.Lock()
	h.closing = true // a stop asked for from now on finds the command gone
	h.stopMu.Unlock()
	h.stops.Wait()
	if h.tty != nil {
		h.tty.Close()
	}

	// The socket goes first, so that a reader that sees the status also
	// finds the socket gone.
	h.ln.Close()
	h.removeSocket()
	st := exitStatus(h.cmd, waitErr)
	if h.stopNote != "" {
		st.Stopped, st.StopNote = true, h.stopNote
	}
	return writeStatus(stateDir, st)
}

func exitStatus(cmd *exec.Cmd, err error) Status {
	st := Status{Exited: true, Code: cmd.ProcessState.ExitCode()}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		st.Signal = ws.Signal().String()
	}
	if err != nil && cmd.ProcessState == nil {
		st.Error = err.Error()
	}
	return st
}

// start starts the command and the goroutines that log its output.
func (h *host) start() (*sync.WaitGroup, error) {
	argv0, err := lookPath(h.spec.Argv[0], h.spec.Env, h.spec.Dir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv0, h.spec.Argv[1:]...)
	cmd.Args[0] = h.spec.Argv[0]
	cmd.Dir = h.spec.Dir
	cmd.Env = h.spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // never the host's own
	}
	h.cmd = cmd
	var readers sync.WaitGroup

	if h.spec.Terminal {
		size := &pty.Winsize{Rows: h.spec.Rows, Cols: h.spec.Cols}
		if size.Rows == 0 {
			size.Rows = 24
		}
		if size.Cols == 0 {
			size.Cols = 80
		}
		tty, err := openBounded(filepath.Join(h.dir, TerminalLog), h.spec.LogLimit)
		if err != nil {
			return nil, err
		}
		// pty.StartWithSize puts the command in a new session, with the
		// terminal as its controlling terminal; its process group is its pid.
		m, err := pty.StartWithSize(cmd, size)
		if err != nil {
			tty.Close()
			return nil, fmt.Errorf("host: start %s: %w", h.spec.Argv[0], err)
		}
		h.tty, h.input, h.term = m, m, tty
		h.cols, h.rows = int(size.Cols), int(size.Rows)
		readers.Add(1)
		go func() {
			defer readers.Done()
			defer tty.Close()
			io.Copy(tty, m) // ends with an error once the terminal is closed
		}()
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Pipes of our own, not cmd's: Wait closes those before the
		// readers have read everything.
		inR, stdin, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		stdout, outW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		stderr, errW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		errLog, err := openBounded(filepath.Join(h.dir, StderrLog), h.spec.LogLimit)
		if err != nil {
			return nil, err
		}
		err = cmd.Start()
		inR.Close() // the command holds its own copies now
		outW.Close()
		errW.Close()
		if err != nil {
			errLog.Close()
			return nil, fmt.Errorf("host: start %s: %w", h.spec.Argv[0], err)
		}
		h.input = stdin
		readers.Add(2)
		go func() {
			defer readers.Done()
			r := bufio.NewReaderSize(stdout, 64<<10)
			for {
				line, err := r.ReadBytes('\n')
				if len(line) > 0 {
					h.events.append(line[:len(line)-trailingNewline(line)])
				}
				if err != nil {
					return
				}
			}
		}()
		go func() {
			defer readers.Done()
			defer errLog.Close()
			io.Copy(errLog, stderr)
		}()
	}
	if err := writeFile(filepath.Join(h.dir, PIDFile), strconv.Itoa(cmd.Process.Pid)); err != nil {
		return &readers, err
	}
	return &readers, nil
}

func trailingNewline(line []byte) int {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		return 1
	}
	return 0
}

// lookPath finds argv0 on the PATH of the command's own environment.
// Relative entries are taken from dir, where the command runs.
func lookPath(argv0 string, env []string, dir string) (string, error) {
	if strings.Contains(argv0, "/") {
		return argv0, nil
	}
	for _, kv := range env {
		if path, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, d := range filepath.SplitList(path) {
				p := filepath.Join(d, argv0)
				if !filepath.IsAbs(p) && dir != "" {
					p = filepath.Join(dir, p)
				}
				if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					return p, nil
				}
			}
		}
	}
	return "", fmt.Errorf("host: %s not found on the command's PATH", argv0)
}

func (h *host) listen() error {
	h.sock = defaultSocket(h.dir)
	if h.sock != filepath.Join(h.dir, SocketFile) {
		if err := privateDir(filepath.Dir(h.sock)); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(h.dir, pointerFile), h.sock); err != nil {
			return err
		}
	}
	os.Remove(h.sock) // left by a host that died; this state directory is ours
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.sock, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return fmt.Errorf("host: listen: %w", err)
	}
	ln.SetUnlinkOnClose(false)
	h.ln = ln
	return nil
}

// removeSocket removes the socket, and the directory made for it outside
// the state directory with the pointer to it. The directory goes only when
// it is empty, so nothing else in it is touched.
func (h *host) removeSocket() {
	os.Remove(h.sock)
	if h.sock != filepath.Join(h.dir, SocketFile) {
		os.Remove(filepath.Dir(h.sock))
		os.Remove(filepath.Join(h.dir, pointerFile))
	}
}

// privateDir makes dir, which must then be a directory of this user that
// nobody else can enter.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("host: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("host: %s is not a private directory of this user", dir)
	}
	return nil
}

func (h *host) serve() {
	for {
		c, err := h.ln.AcceptUnix()
		if err != nil {
			return
		}
		go h.serveConn(c)
	}
}

// request is one call on the host's socket; response its answer.
type request struct {
	Op        string        `json:"op"`
	Data      []byte        `json:"data,omitempty"`
	ID        string        `json:"id,omitempty"`        // answer: the call answered
	Wait      bool          `json:"wait,omitempty"`      // call: keep it open for an answer
	Interrupt time.Duration `json:"interrupt,omitempty"` // stop: grace after SIGINT
	Terminate time.Duration `json:"terminate,omitempty"` // stop: grace after SIGTERM
	Run       string        `json:"run,omitempty"`       // follow: the run that After counts in
	After     uint64        `json:"after,omitempty"`     // follow: the position to continue after
}

type response struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	PID     int           `json:"pid,omitempty"`
	Already bool          `json:"already,omitempty"` // stop: nothing was running
	ID      string        `json:"id,omitempty"`      // call: the call's id
	Data    []byte        `json:"data,omitempty"`    // call: the answer
	Calls   []WaitingCall `json:"calls,omitempty"`   // waiting: the calls that wait
	Signals string        `json:"signals,omitempty"` // stop: the signals sent, in order
	Run     string        `json:"run,omitempty"`     // follow: the host's run
	Pos     uint64        `json:"pos,omitempty"`     // follow: the position after Data
	Reset   bool          `json:"reset,omitempty"`   // follow: Data is the end of the log, not a continuation
	Cols    int           `json:"cols,omitempty"`    // follow: the terminal's size
	Rows    int           `json:"rows,omitempty"`
	End     bool          `json:"end,omitempty"` // follow: the terminal is closed and everything was sent
}

func (h *host) serveConn(c *net.UnixConn) {
	defer c.Close()
	dec := json.NewDecoder(bufio.NewReader(c))
	enc := json.NewEncoder(c)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				h.log.dropped("reading a request", err) // the other end closing is not news
			}
			return
		}
		if req.Op == "call" {
			resp := h.call(c, req)
			if err := enc.Encode(resp); err != nil {
				h.log.dropped("writing the answer of a call", err)
			}
			return // a call has its own connection
		}
		if req.Op == "follow" {
			h.follows.Add(1)
			h.follow(c, enc, req)
			h.follows.Add(-1)
			return // the connection is the stream now
		}
		if req.Op == "stop" {
			resp, registered := h.stop(req)
			err := enc.Encode(resp)
			if registered {
				h.stops.Done()
			}
			if err != nil {
				h.log.dropped("writing the reply to stop", err)
				return
			}
			continue
		}
		resp := h.handle(req)
		if err := enc.Encode(resp); err != nil {
			h.log.dropped("writing the reply to "+req.Op, err)
			return
		}
	}
}

func (h *host) handle(req request) response {
	switch req.Op {
	case "input":
		h.inMu.Lock()
		defer h.inMu.Unlock()
		select {
		case <-h.done:
			return response{Error: "the command has exited"}
		default:
		}
		if _, err := h.input.Write(req.Data); err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true}
	case "status":
		return response{OK: true, PID: h.cmd.Process.Pid}
	case "tail":
		return response{OK: true, Data: h.tail(int(req.Interrupt))}
	case "waiting":
		return response{OK: true, Calls: h.calls.list()}
	case "answer":
		if !h.calls.answer(req.ID, req.Data) {
			return response{Error: fmt.Sprintf("no call %q is waiting", req.ID)}
		}
		return response{OK: true}
	}
	return response{Error: fmt.Sprintf("unknown operation %q", req.Op)}
}

// tail returns the last n bytes of the raw log: the terminal's, or
// standard error on pipes. It is for a person to read; the host passes it
// on without looking at it.
func (h *host) tail(n int) []byte {
	name := StderrLog
	if h.spec.Terminal {
		name = TerminalLog
	}
	b, _ := os.ReadFile(filepath.Join(h.dir, name))
	if n > 0 && len(b) > n {
		b = b[len(b)-n:]
	}
	return b
}

func writeFile(path, s string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return fmt.Errorf("host: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("host: %w", err)
	}
	return nil
}

func writeStatus(dir string, s Status) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, StatusFile), string(b))
}
