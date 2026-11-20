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
	"os/signal"
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
	dir     string
	spec    Spec
	cmd     *exec.Cmd
	input   io.Writer // the terminal or the command's stdin
	inMu    sync.Mutex
	tty     *os.File    // the terminal's controlling side, or nil on pipes
	ptyName string      // the name of the terminal the command runs in, such as /dev/pts/3; "" on pipes
	term    *boundedLog // the terminal's raw log, or nil on pipes
	run     string      // this run of the host; positions in term count from its start
	cols    int
	rows    int

	follows  atomic.Int32 // follow and attach requests being served
	attached atomic.Int32 // attach requests being served: a person's terminal is connected
	detached atomic.Int64 // when the last attached terminal went, in Unix milliseconds; 0 for never
	ln       *net.UnixListener
	sock     string

	events   *lineLog
	calls    callTable
	log      *hostLog      // the host's own record; nil when it could not be opened
	done     chan struct{} // closed when the command has exited; for a relay, when it is told to stop
	endRelay sync.Once     // closes done, once, for a relay

	stopMu   sync.Mutex
	stopOne  sync.Mutex     // one stop at a time
	closing  bool           // the host is shutting down; no new stop starts
	stops    sync.WaitGroup // stops in progress
	stopNote string         // the signals the last stop sent

	terminated   chan struct{} // closed when the host first got SIGTERM: a shutdown
	terminatedAt atomic.Int64  // when it last got one, in Unix nanoseconds
}

// shutdownGrace is how long the host, whose command ended without being
// told to stop, waits for a SIGTERM of its own before it records the end:
// a shutdown sends SIGTERM to every process, the command's may come first,
// and the host's a moment later.
const shutdownGrace = 200 * time.Millisecond

// shutdownWindow is how long after the host's SIGTERM its command's end
// counts as the shutdown's: a service manager sends SIGKILL after its stop
// timeout (systemd's default is 90 seconds), which ends the host too, so a
// host that still runs after that was not in a shutdown.
const shutdownWindow = 90 * time.Second

// Run is `hataori host <state-dir>`: it runs the Spec in stateDir until the
// command exits, and returns after recording how it ended.
func Run(stateDir string) (err error) {
	// First of all: a second host for the same state directory leaves
	// everything to the first, and records nothing.
	lk, err := lock(stateDir)
	if err != nil {
		return err
	}
	defer unlock(lk)
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
	h := &host{dir: stateDir, done: make(chan struct{}), log: hl, run: newRun(), terminated: make(chan struct{})}
	// SIGTERM is what a machine's shutdown, or a service manager stopping
	// hataori's service, sends every process. The host does not end on it:
	// it stays to record how its command ended, and records that it got
	// SIGTERM, so the end is a shutdown's and not a crash (design 4.5).
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	quit := make(chan struct{})
	defer func() { signal.Stop(term); close(quit) }()
	go func() {
		for first := true; ; first = false {
			select {
			case <-term:
				// Each one, the last counts: a stop of the service that left
				// the host running can come long before the shutdown.
				h.terminatedAt.Store(time.Now().UnixNano())
				if first {
					close(h.terminated) // first: the log's write must not take from the grace
				}
				hl.printf("got SIGTERM: everything is being stopped; the command's end is recorded as a shutdown")
			case <-quit:
				return
			}
		}
	}()
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
	if h.spec.Relay && (len(h.spec.Argv) > 0 || h.spec.Terminal) {
		writeStatus(stateDir, Status{Error: errRelayCommand.Error(), Code: -1})
		return errRelayCommand
	}

	// Listen before the command starts, so that its first hook call finds
	// the socket; connections wait in the backlog until serve accepts them.
	if err := h.listen(); err != nil {
		writeStatus(stateDir, Status{Error: err.Error(), Code: -1})
		return err
	}
	if h.spec.Relay {
		return h.relay()
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

	st := exitStatus(h.cmd, waitErr)
	if h.stopNote != "" {
		st.Stopped, st.StopNote = true, h.stopNote
	} else {
		select { // ended without a stop: a crash, or a shutdown that reached the command first
		case <-h.terminated:
		case <-time.After(shutdownGrace):
		}
	}
	select {
	case <-h.terminated:
		st.Shutdown = time.Since(time.Unix(0, h.terminatedAt.Load())) <= shutdownWindow
	default:
	}
	// The socket goes first, so that a reader that sees the status also
	// finds the socket gone.
	h.ln.Close()
	h.removeSocket()
	return writeStatus(stateDir, st)
}

// relay serves the socket with no command until a stop arrives, then
// records that it ran none. It writes no command.pid and opens no log of
// a terminal: the terminal is another holder's.
func (h *host) relay() error {
	h.log.printf("a relay: no command")
	go h.serve()
	<-h.done
	h.log.printf("told to stop")
	h.stopMu.Lock()
	h.closing = true // no stop registers from now on, so Wait races no Add
	h.stopMu.Unlock()
	h.stops.Wait() // every stop's answer is written before the host ends
	h.ln.Close()
	h.removeSocket()
	return writeStatus(h.dir, Status{Relay: true, Stopped: true})
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
		size := &pty.Winsize{}
		size.Cols, size.Rows = h.spec.Size()
		tty, err := openBounded(filepath.Join(h.dir, TerminalLog), h.spec.LogLimit)
		if err != nil {
			return nil, err
		}
		// As pty.StartWithSize does, keeping the terminal's name: the
		// command runs in a new session, with the terminal as its
		// controlling terminal; its process group is its pid.
		m, name, err := startInTerminal(cmd, size)
		if err != nil {
			tty.Close()
			return nil, fmt.Errorf("host: start %s: %w", h.spec.Argv[0], err)
		}
		h.tty, h.input, h.term, h.ptyName = m, m, tty, name
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
	return LookPath(argv0, env, dir)
}

// LookPath finds argv0 on the PATH of the command's own environment, as the
// host does before it starts a command: for another holder that executes
// the same spec (`hataori exec`). Relative entries are taken from dir.
func LookPath(argv0 string, env []string, dir string) (string, error) {
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
			// A relay has no command whose end ends it: only a stop on this
			// socket does, so it keeps accepting through a passing error,
			// such as running out of descriptors.
			if h.spec.Relay && !errors.Is(err, net.ErrClosed) {
				h.log.dropped("accepting a connection", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		go h.serveConn(c)
	}
}

// startInTerminal starts cmd on a new pseudo-terminal of the given size, in
// a new session with the terminal as its controlling terminal, and returns
// the terminal's controlling side and the name of the side the command
// holds.
func startInTerminal(cmd *exec.Cmd, size *pty.Winsize) (*os.File, string, error) {
	m, t, err := pty.Open()
	if err != nil {
		return nil, "", err
	}
	defer t.Close() // the command holds its own copy once started
	if err := pty.Setsize(m, size); err != nil {
		m.Close()
		return nil, "", err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = t, t, t
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		m.Close()
		return nil, "", err
	}
	return m, t.Name(), nil
}

// errNoTerminal answers, on a relay, the requests that need the terminal:
// another holder holds it.
const errNoTerminal = "the host runs no command: its terminal is another holder's"

// request is one call on the host's socket; response its answer.
type request struct {
	Op        string        `json:"op"`
	Data      []byte        `json:"data,omitempty"`
	ID        string        `json:"id,omitempty"`        // answer: the call answered
	Wait      bool          `json:"wait,omitempty"`      // call: keep it open for an answer
	Interrupt time.Duration `json:"interrupt,omitempty"` // stop: grace after SIGINT
	Terminate time.Duration `json:"terminate,omitempty"` // stop: grace after SIGTERM
	Run       string        `json:"run,omitempty"`       // follow: the run that After counts in
	After     uint64        `json:"after,omitempty"`     // follow, screen: the position to continue after
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
	Run     string        `json:"run,omitempty"`     // follow, screen: the host's run
	Pos     uint64        `json:"pos,omitempty"`     // follow, screen: the position after Data
	Reset   bool          `json:"reset,omitempty"`   // follow: Data is the end of the log, not a continuation; screen: part of what followed After was dropped
	Cols    int           `json:"cols,omitempty"`    // follow: the terminal's size
	Rows    int           `json:"rows,omitempty"`
	End     bool          `json:"end,omitempty"`    // follow: the terminal is closed and everything was sent
	Device  string        `json:"device,omitempty"` // status: the terminal the command runs in; empty on pipes
	// Attached, for status, is how many terminals of people are attached;
	// Detached is when the last one went, in Unix milliseconds, 0 for never.
	Attached int   `json:"attached,omitempty"`
	Detached int64 `json:"detached,omitempty"`
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
		if req.Op == "attach" {
			h.follows.Add(1)
			h.attach(c, dec, enc, req)
			h.follows.Add(-1)
			return // the connection is the stream now, and carries keys
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
		if h.spec.Relay {
			return response{Error: errNoTerminal}
		}
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
		if h.spec.Relay { // no command: no process id and no terminal
			return response{OK: true}
		}
		device := h.ptyName
		select {
		case <-h.done: // the command has exited; its terminal only drains
			device = ""
		default:
		}
		return response{OK: true, PID: h.cmd.Process.Pid, Device: device,
			Attached: int(h.attached.Load()), Detached: h.detached.Load()}
	case "tail":
		return response{OK: true, Data: h.tail(int(req.Interrupt))}
	case "screen":
		if h.spec.Relay {
			return response{Error: errNoTerminal}
		}
		if h.term == nil {
			return response{Error: "the command runs on pipes: there is no screen"}
		}
		data, pos, cut, err := h.term.since(req.After)
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Run: h.run, Pos: pos, Reset: cut, Data: data}
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
// on without looking at it. A relay has none.
func (h *host) tail(n int) []byte {
	if h.spec.Relay {
		return nil
	}
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
