package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
)

// server is a tmux server of the test's own, on a socket in a directory of
// its own, with no configuration file. It never reaches the user's server.
type server struct {
	t    *testing.T
	tmux string
	dir  string
	sock string
	n    int
}

func newServer(t *testing.T) *server {
	t.Helper()
	prog, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	// A short path: a Unix socket's path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "hx")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, tmux: prog, dir: dir, sock: filepath.Join(dir, "s")}
	t.Cleanup(func() {
		exec.Command(prog, "-S", s.sock, "kill-server").Run()
		os.RemoveAll(dir)
	})
	return s
}

// run runs a tmux command on the test's server.
func (s *server) run(args ...string) string {
	s.t.Helper()
	cmd := exec.Command(s.tmux, append([]string{"-S", s.sock, "-f", "/dev/null"}, args...)...)
	cmd.Env = env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// env is the test's environment without TMUX, so that nothing reaches a
// server the test did not start.
func env() []string {
	var out []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "TMUX_PANE=") {
			out = append(out, e)
		}
	}
	return out
}

// pane starts command in a new pane, in a new window of a session of its
// own, and returns the pane's id and its process.
func (s *server) pane(command string) (string, int) {
	s.t.Helper()
	format := "#{pane_id} #{pane_pid}" // -P prints a tab as "_"
	var out string
	if s.n == 0 {
		out = s.run("new-session", "-d", "-s", "t", "-x", "100", "-y", "30", "-P", "-F", format, command)
	} else {
		out = s.run("new-window", "-d", "-t", "t:", "-P", "-F", format, command)
	}
	s.n++
	f := strings.Fields(out)
	if len(f) != 2 {
		s.t.Fatalf("new pane: %q", out)
	}
	pid, err := strconv.Atoi(f[1])
	if err != nil {
		s.t.Fatalf("new pane: %q", out)
	}
	return f[0], pid
}

// window is the holder's window for pane, as a hook in it would name it,
// with claude as the process that reported.
func (s *server) window(pane string, claude int) holder.Window {
	s.t.Helper()
	w, err := Window(s.sock+","+s.run("display-message", "-p", "#{pid}")+",0", pane, claude)
	if err != nil {
		s.t.Fatal(err)
	}
	return w
}

// waitFile waits for a file the pane's command writes.
func waitFile(t *testing.T, p string) []byte {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never written", filepath.Base(p))
		}
	}
}

// reader starts a pane whose program switched bracketed paste on, with its
// terminal in raw mode, and writes every byte it reads to a file.
func (s *server) reader() (holder.Window, string) {
	s.t.Helper()
	out, ready := filepath.Join(s.dir, fmt.Sprintf("out%d", s.n)), filepath.Join(s.dir, fmt.Sprintf("ready%d", s.n))
	pane, pid := s.pane(fmt.Sprintf(`stty raw -echo; printf '\033[?2004h'; echo x > %s; exec cat > %s`, ready, out))
	waitFile(s.t, ready)
	time.Sleep(500 * time.Millisecond) // tmux has read the switch
	return s.window(pane, pid), out
}

// waitBytes waits until the file at p holds n bytes.
func waitBytes(t *testing.T, p string, n int) string {
	t.Helper()
	var b []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ = os.ReadFile(p)
		if len(b) >= n {
			break
		}
	}
	return string(b)
}

// paste is a paste of several lines with a tab, a DEL, Japanese and a
// leading "-", which a command line would take for an option.
const paste = "-first line, a tab:\tthen a DEL:\x7f\n日本語の二行目\nlast `$HOME` <&|;>"

func TestInputArrivesByteForByte(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	h := &Holder{}
	parts := []agent.Part{{Paste: paste}, {Key: agent.Enter}, {Key: agent.Escape}, {Key: agent.Up}, {Key: agent.Down}}
	if err := h.Input(t.Context(), w, parts); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[200~" + paste + "\x1b[201~" + "\r" + "\x1b" + "\x1b[A" + "\x1b[B"
	if got := waitBytes(t, out, len(want)); got != want {
		t.Fatalf("the pane read\n%q\nwant\n%q", got, want)
	}
	if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
		t.Fatalf("buffers left: %q", b)
	}
}

// An input that is refused hands over nothing, not even its parts before
// the one refused: an unknown key, an empty paste, a window of another
// server on the same socket, an input whose context has ended.
func TestRefusedInputHandsOverNothing(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	h := &Holder{}
	r, _ := parseRef(w)
	stale, _ := Window(fmt.Sprintf("%s,%d,0", r.Socket, r.Server+1), r.Pane, w.Process)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		name  string
		ctx   context.Context
		w     holder.Window
		parts []agent.Part
	}{
		{"an unknown key", t.Context(), w, []agent.Part{{Paste: "never"}, {Key: agent.Enter}, {Key: "Tab"}}},
		{"an empty paste", t.Context(), w, []agent.Part{{Paste: "never"}, {Key: agent.Enter}, {}}},
		{"a part both", t.Context(), w, []agent.Part{{Paste: "never"}, {Paste: "x", Key: agent.Enter}}},
		{"another server", t.Context(), stale, []agent.Part{{Paste: "never"}, {Key: agent.Enter}}},
		{"an ended context", ended, w, []agent.Part{{Paste: "never"}, {Key: agent.Enter}}},
	} {
		if err := h.Input(c.ctx, c.w, c.parts); err == nil {
			t.Errorf("%s: handed over", c.name)
		}
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: "ok"}}); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[200~ok\x1b[201~"
	if got := waitBytes(t, out, len(want)); got != want {
		t.Fatalf("the pane read %q, want only %q", got, want)
	}
	if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
		t.Fatalf("buffers left: %q", b)
	}
}

// wrapper is a tmux that records its arguments and runs the real one, and
// fails a paste-buffer when fail is set.
func (s *server) wrapper(fail bool) (string, string) {
	s.t.Helper()
	log := filepath.Join(s.dir, "args.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n"
	if fail {
		script += "case \"$*\" in *paste-buffer*) echo 'made to fail' >&2; exit 1;; esac\n"
	}
	script += "exec " + s.tmux + " \"$@\"\n"
	p := filepath.Join(s.dir, "tmux-wrapper")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		s.t.Fatal(err)
	}
	return p, log
}

// A marker in a paste is in no process's arguments while it is delivered,
// and no buffer of the holder's is left, also after a failed paste.
func TestPasteTextIsNeverAnArgument(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	prog, log := s.wrapper(false)
	h := &Holder{Tmux: prog}
	marker := fmt.Sprintf("marker-%d-%d", os.Getpid(), time.Now().UnixNano())
	text := strings.Repeat(marker+" ", 2000)

	stop := make(chan struct{})
	var seen []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // every process's arguments, as often as ps can list them
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, _ := exec.Command("ps", "-A", "-o", "args=").Output()
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, marker) && !strings.Contains(l, " ps ") {
					seen = append(seen, l)
				}
			}
		}
	}()
	for i := 0; i < 5; i++ {
		if err := h.Input(t.Context(), w, []agent.Part{{Paste: text}}); err != nil {
			t.Fatal(err)
		}
		if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
			t.Fatalf("buffers left after input %d: %q", i+1, b)
		}
	}
	close(stop)
	wg.Wait()
	if len(seen) > 0 {
		t.Fatalf("the marker was in a process's arguments: %.200q", seen[0])
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "load-buffer") || strings.Contains(string(args), marker) {
		t.Fatalf("tmux was run with these arguments:\n%s", args)
	}
	if got := waitBytes(t, out, 5*(len(text)+12)); !strings.Contains(got, marker) {
		t.Fatal("the paste never arrived")
	}

	failing, _ := s.wrapper(true)
	h = &Holder{Tmux: failing}
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: text}}); err == nil {
		t.Fatal("a failed paste-buffer was not an error")
	}
	if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
		t.Fatalf("buffers left after a failed input: %q", b)
	}
}

// state is what must not change in a pane: its processes, its size and its
// options.
func (s *server) state(pane string) string {
	s.t.Helper()
	panes := s.run("list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_tty} #{pane_width}x#{pane_height} #{pane_dead} #{window_width}x#{window_height}")
	var tty string
	for _, l := range strings.Split(panes, "\n") {
		if f := strings.Fields(l); f[0] == pane {
			tty = strings.TrimPrefix(f[2], "/dev/")
		}
	}
	ps, _ := exec.Command("ps", "-A", "-o", "pid=,pgid=,tty=,comm=").Output()
	var procs []string
	for _, l := range strings.Split(string(ps), "\n") {
		if f := strings.Fields(l); len(f) >= 3 && f[2] == tty {
			procs = append(procs, strings.Join(f, " "))
		}
	}
	return strings.Join([]string{panes, strings.Join(procs, "\n"),
		s.run("show-options", "-p", "-t", pane), s.run("show-options", "-w", "-t", pane), s.run("show-options", "-s")}, "\n--\n")
}

func TestCapsStopAndNothingChanges(t *testing.T) {
	s := newServer(t)
	pane, pid := s.pane("exec cat > /dev/null")
	w := s.window(pane, pid)
	h := &Holder{Poll: 20 * time.Millisecond}
	if c := h.Caps(); c.CanStart || c.RawScreen {
		t.Fatalf("caps %+v: it starts nothing and has snapshots only", c)
	}
	before := s.state(pane)
	ctx := t.Context()

	if _, err := h.Start(ctx, s.dir, holder.Spec{Argv: []string{"true"}}); !errors.Is(err, holder.ErrCannotStart) {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.Stop(ctx, w, time.Second, time.Second); !errors.Is(err, holder.ErrNotOwned) {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.Input(ctx, w, []agent.Part{{Paste: "text"}, {Key: agent.Enter}, {Key: agent.Escape}, {Key: agent.Up}, {Key: agent.Down}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Input(ctx, w, []agent.Part{{Paste: "never"}, {Key: "Tab"}}); err == nil {
		t.Fatal("an unknown key was handed over")
	}
	if l, _ := h.Alive(ctx, w); l.State != holder.Running {
		t.Fatalf("Alive: %+v", l)
	}
	if err := h.CheckTakeIn(ctx, w); err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	ch, err := h.Screen(sctx, w, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := <-ch; !ok || !c.Reset || c.Cols != 100 || c.Rows != 30 {
		t.Fatalf("snapshot %+v, %t", c, ok)
	}
	cancel()
	for range ch {
	}
	time.Sleep(100 * time.Millisecond)
	if after := s.state(pane); after != before {
		t.Fatalf("the pane changed:\n%s\nafter:\n%s", before, after)
	}
}

func TestAlive(t *testing.T) {
	s := newServer(t)
	pane, pid := s.pane("exec sleep 1000")
	other, _ := s.pane("exec sleep 1001") // keeps the server up when the first goes
	h := &Holder{}
	ctx := t.Context()
	state := func(w holder.Window) holder.State {
		t.Helper()
		l, err := h.Alive(ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		return l.State
	}
	w := s.window(pane, pid)
	if st := state(w); st != holder.Running {
		t.Fatalf("a pane whose process leads the foreground group: %v", st)
	}

	// Another server on the same socket names its panes from %0 again.
	var r ref
	if err := jsonRef(w, &r); err != nil {
		t.Fatal(err)
	}
	stale, _ := Window(fmt.Sprintf("%s,%d,0", r.Socket, r.Server+1), pane, pid)
	if st := state(stale); st != holder.Gone {
		t.Fatalf("a reference to another server: %v", st)
	}

	gone := exec.Command("true")
	gone.Run()
	if st := state(s.window(pane, gone.Process.Pid)); st != holder.Exited {
		t.Fatalf("a process that ended in a pane that stays: %v", st)
	}

	// A socket file removed under a server that runs on: it cannot be
	// reached, and that is not gone.
	moved := s.sock + ".moved"
	if err := os.Rename(s.sock, moved); err != nil {
		t.Fatal(err)
	}
	st := state(w)
	if err := os.Rename(moved, s.sock); err != nil {
		t.Fatal(err)
	}
	if st != holder.Unknown {
		t.Fatalf("a server whose socket file was removed: %v", st)
	}

	s.run("kill-pane", "-t", pane)
	if st := state(w); st != holder.Gone {
		t.Fatalf("a pane that is not listed: %v", st)
	}
	_ = other

	fake := filepath.Join(s.dir, "fake-tmux")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'lost the connection' >&2\nexit 1\n"), 0o700)
	if st := state2(t, &Holder{Tmux: fake}, s.window(other, pid)); st != holder.Unknown {
		t.Fatalf("tmux that cannot be reached: %v", st)
	}
}

func state2(t *testing.T, h *Holder, w holder.Window) holder.State {
	t.Helper()
	l, err := h.Alive(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	return l.State
}

func jsonRef(w holder.Window, r *ref) error {
	got, err := parseRef(w)
	*r = got
	return err
}

// Two processes in one pane: the one that leads the foreground group
// passes the check; one in the background of the same terminal, and one on
// another terminal, do not.
func TestCheckTakeIn(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash for job control")
	}
	s := newServer(t)
	bg := filepath.Join(s.dir, "bg")
	pane, pid := s.pane(fmt.Sprintf(`exec bash -c 'set -m; sleep 1000 & echo $! > %s; exec sleep 1001'`, bg))
	back, err := strconv.Atoi(strings.TrimSpace(string(waitFile(t, bg))))
	if err != nil {
		t.Fatal(err)
	}
	other, otherPID := s.pane("exec sleep 1002")
	h := &Holder{}
	ctx := t.Context()
	for deadline := time.Now().Add(5 * time.Second); h.CheckTakeIn(ctx, s.window(pane, pid)) != nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the foreground process: %v", h.CheckTakeIn(ctx, s.window(pane, pid)))
		}
	}
	if err := h.CheckTakeIn(ctx, s.window(pane, back)); !errors.Is(err, ErrNotForeground) {
		t.Fatalf("a background process on the pane's terminal: %v", err)
	}
	if err := h.CheckTakeIn(ctx, s.window(pane, otherPID)); !errors.Is(err, ErrNotItsTerminal) {
		t.Fatalf("a process of another pane: %v", err)
	}
	if err := h.CheckTakeIn(ctx, s.window(pane, os.Getpid())); !errors.Is(err, ErrNotItsTerminal) {
		t.Fatalf("a process without the pane's terminal: %v", err)
	}
	if l, _ := h.Alive(ctx, s.window(pane, back)); l.State != holder.Unknown {
		t.Fatalf("Alive of a background process: %+v", l)
	}
	_ = other
}

func TestWindowFromHookEnvironment(t *testing.T) {
	w, err := Window("/tmp/a,b/default,4242,3", "%12", 77)
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseRef(w)
	if err != nil || r != (ref{Socket: "/tmp/a,b/default", Server: 4242, Pane: "%12"}) || w.Process != 77 || w.Owned || w.Holder != Name {
		t.Fatalf("%+v %+v %v", w, r, err)
	}
	for _, bad := range [][2]string{{"/tmp/s,x,0", "%1"}, {"/tmp/s", "%1"}, {"/tmp/s,1,0", "1"}, {"/tmp/s,1,0", "%1; rm"}} {
		if _, err := Window(bad[0], bad[1], 1); err == nil {
			t.Errorf("%q %q accepted", bad[0], bad[1])
		}
	}
}
