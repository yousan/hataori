package host

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// drainTimeout is how long the host keeps reading output after the command
// exited, for bytes still in flight or held by a process the command left.
const drainTimeout = 2 * time.Second

// host is one running session host.
type host struct {
	dir   string
	spec  Spec
	cmd   *exec.Cmd
	input io.Writer // the terminal or the command's stdin
	inMu  sync.Mutex
	tty   *os.File // the terminal's controlling side, or nil on pipes
	ln    *net.UnixListener
	sock  string

	events *lineLog
	done   chan struct{} // closed when the command has exited
}

// Run is `hataori host <state-dir>`: it runs the Spec in stateDir until the
// command exits, and returns after recording how it ended.
func Run(stateDir string) error {
	b, err := os.ReadFile(filepath.Join(stateDir, SpecFile))
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	h := &host{dir: stateDir, done: make(chan struct{})}
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

	readers, err := h.start()
	if err != nil {
		writeStatus(stateDir, Status{Error: err.Error(), Code: -1})
		return err
	}
	if err := h.listen(); err != nil {
		syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
		h.cmd.Wait()
		writeStatus(stateDir, Status{Error: err.Error(), Code: -1})
		return err
	}
	go h.serve()

	waitErr := h.cmd.Wait()
	close(h.done)
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
	}
	if h.tty != nil {
		h.tty.Close()
	}

	// The socket goes first, so that a reader that sees the status also
	// finds the socket gone.
	h.ln.Close()
	os.Remove(h.sock)
	os.Remove(filepath.Join(stateDir, pointerFile))
	return writeStatus(stateDir, exitStatus(h.cmd, waitErr))
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
		h.tty, h.input = m, m
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
		if err := os.MkdirAll(filepath.Dir(h.sock), 0o700); err != nil {
			return fmt.Errorf("host: %w", err)
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
	Op   string `json:"op"`
	Data []byte `json:"data,omitempty"`
}

type response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	PID   int    `json:"pid,omitempty"`
}

func (h *host) serveConn(c *net.UnixConn) {
	defer c.Close()
	dec := json.NewDecoder(bufio.NewReader(c))
	enc := json.NewEncoder(c)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		resp := h.handle(req)
		if err := enc.Encode(resp); err != nil {
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
	}
	return response{Error: fmt.Sprintf("unknown operation %q", req.Op)}
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
