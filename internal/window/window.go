// Package window takes the reports that the hooks of an agent's session
// send when a person started the session in their own window, and keeps the
// last one of each window in memory, to list it with the host's terminals
// (design, section 4.8).
//
// A report holds no text of the session: no prompt, message or tool input.
// Nothing of it is written anywhere: not to the event log, the database, a
// chat, the supervisor or a plugin. After a restart the list is built again
// from the next reports.
//
// The reports come over a Unix socket in a directory that only the
// instance's OS user can open; the hook checks that before it connects.
package window

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Tmux is the holder name of a window in tmux.
const Tmux = "tmux"

// Report is what one hook call of a session in a person's window tells the
// daemon.
type Report struct {
	Holder string `json:"holder"` // where the window is: Tmux
	// Server is the tmux server, as TMUX names it: its socket's path and
	// its process id, joined by a comma. Pane ids start again at %0 on a
	// new server, so the pane alone does not name a window.
	Server      string `json:"server"`
	Pane        string `json:"pane"`   // the tmux pane's id, such as %3
	Device      string `json:"device"` // the terminal of the agent's process
	PID         int    `json:"pid"`    // the agent's process: the hook's nearest ancestor with a terminal
	Agent       string `json:"agent"`  // the adapter's name
	Session     string `json:"session"`
	Dir         string `json:"dir"`
	ConfigDir   string `json:"config_dir,omitempty"` // the agent's configuration directory, when one was set
	Event       string `json:"event"`                // the hook event's name
	Model       string `json:"model,omitempty"`
	Interactive bool   `json:"interactive"` // a session a person types into, not one run for a single prompt

	at time.Time // when the daemon took it
}

// Window names the report's window: the holder, then its reference.
func (r Report) Window() string { return r.Holder + ":" + r.Server + "," + r.Pane }

// Ends is the event after which a session no longer runs in its window.
const Ends = "SessionEnd"

// MaxReport bounds one report on the socket, in bytes.
const MaxReport = 16 << 10

// maxWindows bounds the windows kept; the oldest report goes first.
const maxWindows = 256

// maxField bounds every text field of a report, in bytes.
const maxField = 4096

// maxSocketLen is the longest socket path used; longer ones are moved.
const maxSocketLen = 100

// SocketDir and SocketFile are where the socket is, under the root: a
// directory of its own, so that its mode is the socket's alone.
const (
	SocketDir  = "state/windows"
	SocketFile = "reports.sock"
)

var paneID = regexp.MustCompile(`^%[0-9]+$`)

// SocketPath returns where the daemon of root takes reports:
// <root>/state/windows/reports.sock, or, when that is too long for a Unix
// socket, a short path under /tmp named after a hash of the root. The same
// in every process whatever its environment, since a hook runs in the
// agent's.
func SocketPath(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("window: %w", err)
	}
	p := filepath.Join(abs, SocketDir, SocketFile)
	if len(p) <= maxSocketLen {
		return p, nil
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join("/tmp", "hataori-windows-"+hex.EncodeToString(sum[:8]), SocketFile), nil
}

// PrivateDir reports whether dir is a directory of this user that nobody
// else can enter, and why not.
func PrivateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("window: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("window: %s is not a private directory of this user", dir)
	}
	return nil
}

// Send hands r to the daemon of root and returns without waiting for an
// answer, giving up at the deadline. Nothing is sent when the socket's
// directory is not private to this user.
func Send(ctx context.Context, root string, r Report) error {
	sock, err := SocketPath(root)
	if err != nil {
		return err
	}
	if err := PrivateDir(filepath.Dir(sock)); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return fmt.Errorf("window: %w", err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	return nil
}

// Server takes reports on root's socket and keeps them.
type Server struct {
	ln      *net.UnixListener
	sock    string
	outside bool // the socket is under /tmp, not under the root
	now     func() time.Time
	alive   func(pid int) bool // whether a process exists

	mu      sync.Mutex
	reports map[string]Report // by window
	ended   map[string]string // by window, the session whose end came last
	wg      sync.WaitGroup
}

func newServer() *Server {
	return &Server{now: time.Now, alive: processAlive, reports: map[string]Report{}, ended: map[string]string{}}
}

// processAlive reports whether pid exists, whoever runs it.
func processAlive(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// Listen starts taking reports for the instance at root. It makes the
// socket's directory when it is missing, and refuses one that another user
// could enter.
func Listen(root string) (*Server, error) {
	sock, err := SocketPath(root)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("window: %w", err)
	}
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	os.Remove(sock) // left by a daemon that died; the instance's lock is ours
	// The directory keeps everyone else out; the socket's own mode is
	// narrowed too, without changing the process's umask, which every
	// thread shares.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("window: listen: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("window: %w", err)
	}
	abs, _ := filepath.Abs(root)
	s := newServer()
	s.ln, s.sock, s.outside = ln, sock, !strings.HasPrefix(sock, abs+string(filepath.Separator))
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Close stops taking reports and removes the socket, and the directory
// made for it outside the root when it is empty.
func (s *Server) Close() error {
	err := s.ln.Close() // removes the socket
	s.wg.Wait()
	if s.outside {
		os.Remove(filepath.Dir(s.sock))
	}
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond) // such as no descriptor left
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			c.SetDeadline(time.Now().Add(2 * time.Second))
			line, err := bufio.NewReader(io.LimitReader(c, MaxReport+1)).ReadBytes('\n')
			if err != nil || len(line) > MaxReport {
				return
			}
			var r Report
			if json.Unmarshal(line, &r) != nil {
				return
			}
			s.take(r)
		}()
	}
}

// valid reports whether r names a window and a session.
func (r Report) valid() bool {
	for _, f := range []string{r.Server, r.Device, r.Agent, r.Session, r.Dir, r.ConfigDir, r.Event, r.Model} {
		if len(f) > maxField {
			return false
		}
	}
	return r.Holder == Tmux && r.Server != "" && paneID.MatchString(r.Pane) && strings.HasPrefix(r.Device, "/dev/") &&
		r.PID > 1 && r.Agent != "" && r.Session != "" && r.Event != ""
}

// Starts is the event with which a session starts or resumes.
const Starts = "SessionStart"

// take keeps r as its window's last report. A session that is not
// interactive is not listed, so its reports are dropped: one started from
// the shell of an interactive session sees the same window in its
// environment. The end of the window's session removes it, and a report of
// that session that comes after its end, from a hook that ran late, is
// dropped until the session starts again.
func (s *Server) take(r Report) {
	if !r.valid() || !r.Interactive {
		return
	}
	r.at = s.now()
	w := r.Window()
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.reports[w]
	if r.Event == Ends {
		if had && old.Session == r.Session {
			delete(s.reports, w)
		}
		if len(s.ended) >= maxWindows {
			clear(s.ended)
		}
		s.ended[w] = r.Session
		return
	}
	if s.ended[w] == r.Session && r.Event != Starts {
		return
	}
	delete(s.ended, w)
	if had && old.Session == r.Session && r.Model == "" {
		r.Model = old.Model // only some events name the model
	}
	if !had && len(s.reports) >= maxWindows {
		var oldest string
		for k, v := range s.reports {
			if oldest == "" || v.at.Before(s.reports[oldest].at) {
				oldest = k
			}
		}
		delete(s.reports, oldest)
	}
	s.reports[w] = r
}

// Annotate gives each tmux pane of list the report of the session that runs
// in it: the report names the pane, the agent's terminal is the pane's, and
// the agent's process still exists. A report whose process is gone is
// dropped, as is one that names no listed pane, unless it came after since,
// when the list began to be read: its window is gone, or is on a tmux server
// that is not listed.
func (s *Server) Annotate(list []control.HostTerminal, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w, r := range s.reports {
		if !s.alive(r.PID) { // ended with no SessionEnd, such as when killed
			delete(s.reports, w)
		}
	}
	seen := map[string]bool{}
	for i, t := range list {
		if t.Tmux == nil {
			continue
		}
		for w, r := range s.reports {
			if r.Pane == t.Tmux.Pane && r.Device == t.Device {
				list[i].Reported = &control.ReportedSession{Window: w, Agent: r.Agent, Session: r.Session,
					Dir: r.Dir, Event: r.Event, At: r.at}
				seen[w] = true
				break
			}
		}
	}
	for w, r := range s.reports {
		if !seen[w] && r.at.Before(since) {
			delete(s.reports, w)
		}
	}
}

// Last returns the report kept for window, for a test.
func (s *Server) Last(window string) (Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reports[window]
	return r, ok
}

// HostTerms is what lists the host's terminals and reads a pane's screen.
type HostTerms interface {
	List(ctx context.Context, owned []int) ([]control.HostTerminal, error)
	Screen(ctx context.Context, pane string, owned []int) (string, bool, error)
	View(ctx context.Context, pane string, owned []int) (control.HostView, bool, error)
}

// Terms lists the host's terminals with t, and gives each tmux pane the
// report of the session that runs in it.
func (s *Server) Terms(t HostTerms) HostTerms { return terms{t, s} }

type terms struct {
	HostTerms
	s *Server
}

func (t terms) List(ctx context.Context, owned []int) ([]control.HostTerminal, error) {
	since := t.s.now()
	list, err := t.HostTerms.List(ctx, owned)
	if err == nil {
		t.s.Annotate(list, since)
	}
	return list, err
}
