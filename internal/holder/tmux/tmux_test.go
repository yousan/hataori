package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		exec.Command(prog, "-S", s.sock, "kill-server").Run()
		os.RemoveAll(dir)
	})
	// Now and then a tmux server misses the end of a program, and sees it
	// with the next one's only.
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Second):
				exec.Command(prog, "-S", s.sock, "run-shell", "-b", "true").Run()
			}
		}
	}()
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

// A person who looks at the window may scroll back in it, or open tmux's
// own tree in it: the pane is then in a mode. Input is refused whole, and
// the mode stays, as does what the person looks at. Once the mode is left,
// input arrives as before.
func TestInputToAPaneInAModeIsRefused(t *testing.T) {
	for _, mode := range []string{"copy-mode", "clock-mode", "choose-tree"} {
		t.Run(mode, func(t *testing.T) {
			s := newServer(t)
			w, out := s.reader()
			r, _ := parseRef(w)
			s.run(mode, "-t", r.Pane)
			in := func() string {
				return s.run("display-message", "-p", "-t", r.Pane, "#{pane_in_mode} #{pane_mode} #{scroll_position}")
			}
			before := in()
			if !strings.HasPrefix(before, "1 ") {
				t.Fatalf("the pane is in no mode: %q", before)
			}
			h := &Holder{}
			for _, parts := range [][]agent.Part{
				{{Paste: paste}, {Key: agent.Enter}},
				{{Paste: paste}},
				{{Key: agent.Escape}},
				{{Key: agent.Down}, {Key: agent.Enter}},
				{{Key: agent.Escape}, {Paste: paste}, {Paste: "another"}},
			} {
				if err := h.Input(t.Context(), w, parts); err == nil {
					t.Errorf("%q was handed over", parts)
				}
			}
			if err := h.Input(t.Context(), w, []agent.Part{{Paste: "x"}}); !errors.Is(err, ErrTakesNoInput) {
				t.Errorf("a paste was refused with %v", err)
			}
			err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Escape}})
			if !errors.Is(err, ErrTakesNoInput) {
				t.Errorf("Escape was refused with %v", err)
			}
			if no := notSent(err); no == nil || !no.Refused {
				t.Errorf("the refusal is %#v, want one that says nothing was sent, and refused", err)
			}
			if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
				t.Errorf("buffers left: %q", b)
			}
			if after := in(); after != before {
				t.Errorf("the pane's mode after the input: %q, before it %q", after, before)
			}
			s.run("copy-mode", "-q", "-t", r.Pane)
			if err := h.Input(t.Context(), w, []agent.Part{{Paste: paste}, {Key: agent.Enter}}); err != nil {
				t.Fatal(err)
			}
			want := "\x1b[200~" + paste + "\x1b[201~" + "\r"
			if got := waitBytes(t, out, len(want)); got != want {
				t.Fatalf("the pane read\n%q\nwant only\n%q", got, want)
			}
		})
	}
}

// before is a tmux that runs the shell commands do right before its nth
// command, and then the command.
func (s *server) before(n int, do string) string {
	s.t.Helper()
	count := filepath.Join(s.dir, fmt.Sprintf("count-%d", s.n))
	os.Remove(count)
	script := "#!/bin/sh\necho x >> " + count + "\n" +
		fmt.Sprintf("if [ $(wc -l < %s) -eq %d ]; then %s; fi\n", count, n, do) +
		"exec " + s.tmux + " \"$@\"\n"
	p := filepath.Join(s.dir, "tmux-before")
	// No fork meanwhile: a child that holds the file open for writing until
	// it executes keeps the script from being executed (ETXTBSY).
	syscall.ForkLock.RLock()
	err := os.WriteFile(p, []byte(script), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		s.t.Fatal(err)
	}
	return p
}

// A person's session may have any name.
func TestAWindowInASessionNamedWithSpaces(t *testing.T) {
	for _, name := range []string{"作業\u3000メモ", "a\tb", "a\u00a0b", "a  b", " a ", "a\u2028b", "\u3000", ""} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			s := newServer(t)
			w, _ := s.reader()
			h := &Holder{}
			s.run("new-window", "-d", "-t", "t", "exec sleep 600") // keeps the server
			if out, err := exec.Command(s.tmux, "-S", s.sock, "rename-session", "-t", "t", name).CombinedOutput(); strings.Contains(string(out), "invalid session") {
				t.Skip("no such name in this tmux") // a tab since 3.7; none in some
			} else if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if err := h.CheckTakeIn(t.Context(), w); err != nil {
				t.Errorf("the take-in check: %v", err)
			}
			if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Running {
				t.Errorf("Alive: %+v %v", l, err)
			}
			r, err := parseRef(w)
			if err != nil {
				t.Fatal(err)
			}
			p, err := h.find(t.Context(), r)
			if got := s.run("-u", "display-message", "-p", "-t", r.Pane, "x#{session_name}x"); err != nil || "x"+p.session+"x" != strings.ReplaceAll(got, " ", "_") {
				t.Errorf("the session: %q %v, tmux says %q", p.session, err, got)
			}
			if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}}); err != nil {
				t.Errorf("Input: %v", err)
			}
			s.run("kill-pane", "-t", r.Pane)
			if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Gone {
				t.Errorf("Alive after it closed: %+v %v", l, err)
			}
		})
	}
}

// A name with a newline does not read as the line of a pane, its own or
// another's. tmux up to 3.1 prints it as it is, later ones escape it, and
// since 3.7 there is no such name.
func TestASessionsNameIsNoPane(t *testing.T) {
	s := newServer(t)
	w, _ := s.reader()
	r, err := parseRef(w)
	if err != nil {
		t.Fatal(err)
	}
	// One listed before the window's own session, one after it, and its own.
	for _, name := range []string{"0", "z", "t"} {
		if name != "t" {
			s.run("new-session", "-d", "-s", name, "exec sleep 600")
		}
		id := s.run("display-message", "-p", "-t", "="+name+":", "#{session_id}")
		for _, sep := range []string{" ", "\t", "\u3000"} {
			line := strings.ReplaceAll(fmt.Sprintf("%d %s /dev/pts/1 80 24 1 1 x0 - z", r.Server, r.Pane), " ", sep)
			if out, err := exec.Command(s.tmux, "-S", s.sock, "rename-session", "-t", id, name+"\n"+line).CombinedOutput(); strings.Contains(string(out), "invalid session") {
				t.Skip("no such name in this tmux")
			} else if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			p, err := (&Holder{}).find(t.Context(), r)
			if err != nil || p.dead || p.pid == 1 {
				t.Errorf("a session %q with the line %q: %+v %v", name, line, p, err)
			}
		}
	}
}

// The pane may enter a mode at any moment, also between any two of the
// holder's commands: the whole input arrives, or it is refused and nothing
// of it does.
func TestAModeEnteredWhileInputIsHandedOver(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	r, _ := parseRef(w)
	want := ""
	handed := 0
	for _, c := range []struct {
		parts []agent.Part
		bytes string
	}{
		{[]agent.Part{{Paste: paste}, {Key: agent.Enter}}, "\x1b[200~" + paste + "\x1b[201~\r"},
		{[]agent.Part{{Key: agent.Escape}, {Key: agent.Down}}, "\x1b\x1b[B"},
	} {
		for n := 1; n <= 6; n++ {
			h := &Holder{Tmux: s.before(n, s.tmux+" -S "+s.sock+" copy-mode -t "+r.Pane)}
			err := h.Input(t.Context(), w, c.parts)
			s.run("copy-mode", "-q", "-t", r.Pane)
			switch {
			case err == nil:
				handed++
				want += c.bytes
			case !errors.Is(err, ErrTakesNoInput):
				t.Errorf("%q, the mode entered before command %d: %v", c.parts, n, err)
			}
			if got := waitBytes(t, out, len(want)); got != want {
				t.Fatalf("%q, the mode entered before command %d (%v): the pane read\n%q\nwant\n%q", c.parts, n, err, got, want)
			}
		}
	}
	if handed == 0 || handed == 12 {
		t.Fatalf("%d of 12 inputs were handed over: the mode was entered always, or never, in time", handed)
	}
	time.Sleep(200 * time.Millisecond)
	if got := waitBytes(t, out, len(want)); got != want {
		t.Fatalf("the pane read\n%q\nwant\n%q", got, want)
	}
}

// attach attaches a client to the server's first session, as a person does,
// until the test ends.
func (s *server) attach() {
	s.t.Helper()
	cmd := exec.Command(s.tmux, "-S", s.sock, "-C", "attach-session", "-t", "t")
	cmd.Env = env()
	in, err := cmd.StdinPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() {
		in.Close()
		cmd.Wait()
	})
	for deadline := time.Now().Add(10 * time.Second); s.run("list-clients", "-t", "t") == ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatal("the client never attached")
		}
	}
}

// The window may go between the holder's last look and its input: the pane,
// also while a person's client looks at another one, and the whole server,
// whose successor has a pane of the same id, a person's.
func TestInputToAWindowThatGoesMeanwhile(t *testing.T) {
	s := newServer(t)
	theirs, out := s.reader()
	gone, goneOut := s.reader()
	s.attach()
	r, _ := parseRef(gone)
	h := &Holder{Tmux: s.before(2, s.tmux+" -S "+s.sock+" kill-pane -t "+r.Pane)}
	if err := h.Input(t.Context(), gone, []agent.Part{{Key: agent.Escape}}); !errors.Is(err, errGone) {
		t.Errorf("a pane killed meanwhile: %v", err)
	}
	// Another server on the socket: list-panes named the one before.
	r, _ = parseRef(theirs)
	stale, _ := Window(fmt.Sprintf("%s,%d,0", r.Socket, r.Server+1), r.Pane, theirs.Process)
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *list-panes*) %s \"$@\" | sed 's/^%d /%d /'; exit;; esac\nexec %s \"$@\"\n", s.tmux, r.Server, r.Server+1, s.tmux)
	prog := filepath.Join(s.dir, "tmux-stale")
	if err := os.WriteFile(prog, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&Holder{Tmux: prog}).Input(t.Context(), stale, []agent.Part{{Paste: "x"}, {Key: agent.Enter}}); err == nil {
		t.Error("input for a pane of a server that ended was handed to the next one's")
	}
	time.Sleep(200 * time.Millisecond)
	for _, p := range []string{out, goneOut} {
		if b, _ := os.ReadFile(p); len(b) > 0 {
			t.Errorf("a pane read %q", b)
		}
	}
}

// In a window whose panes are synchronized, tmux types each key sent to one
// pane into the others, such as a split a person opened beside the agent.
// Input is refused there, and to a pane whose input is off, which would
// drop it; also to the only pane of a window it goes whatever the option.
func TestInputThatWouldReachOtherPanesOrNoneIsRefused(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	r, _ := parseRef(w)
	h := &Holder{}
	theirs, ready := filepath.Join(s.dir, "theirs"), filepath.Join(s.dir, "theirs-ready")
	split := s.run("split-window", "-d", "-t", r.Pane, "-P", "-F", "#{pane_id}",
		fmt.Sprintf("stty raw -echo; echo x > %s; exec cat > %s", ready, theirs))
	waitFile(t, ready)
	in := []agent.Part{{Paste: "never"}, {Key: agent.Enter}}
	s.run("set-option", "-w", "-t", r.Pane, "synchronize-panes", "on")
	for _, parts := range [][]agent.Part{in, {{Key: agent.Escape}}} {
		if err := h.Input(t.Context(), w, parts); !errors.Is(err, ErrTakesNoInput) {
			t.Errorf("synchronized panes, %q: %v", parts, err)
		}
	}
	s.run("set-option", "-w", "-t", r.Pane, "synchronize-panes", "off")
	s.run("select-pane", "-d", "-t", r.Pane)
	for _, parts := range [][]agent.Part{in, {{Key: agent.Escape}}} {
		if err := h.Input(t.Context(), w, parts); !errors.Is(err, ErrTakesNoInput) {
			t.Errorf("input off, %q: %v", parts, err)
		}
	}
	s.run("select-pane", "-e", "-t", r.Pane)
	s.run("copy-mode", "-t", r.Pane)
	s.run("set-hook", "-g", "after-display-message", "select-pane -t :.9") // a person's, which fails
	if err := h.Input(t.Context(), w, in); !errors.Is(err, ErrTakesNoInput) {
		t.Errorf("a pane in copy mode beside a hook that fails: %v", err)
	}
	s.run("set-hook", "-gu", "after-display-message")
	s.run("copy-mode", "-q", "-t", r.Pane)
	s.run("kill-pane", "-t", split)
	s.run("set-option", "-w", "-t", r.Pane, "synchronize-panes", "on")
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: "ok"}, {Key: agent.Enter}}); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[200~ok\x1b[201~\r"
	if got := waitBytes(t, out, len(want)); got != want {
		t.Errorf("the pane read %q, want only %q", got, want)
	}
	if b, _ := os.ReadFile(theirs); len(b) > 0 {
		t.Errorf("the person's pane read %q", b)
	}
}

// A pane that outlives its program, as remain-on-exit keeps the one of a
// window hataori started, drops a key, and a paste to it ends the server of
// tmux up to 3.3a.
func TestInputToADeadPaneIsRefused(t *testing.T) {
	s := newServer(t)
	s.pane("exec sleep 600")
	s.run("set-option", "-g", "remain-on-exit", "on")
	pane, pid := s.pane("exec true")
	for deadline := time.Now().Add(10 * time.Second); s.run("display-message", "-p", "-t", pane, "#{pane_dead}") != "1"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the pane never died")
		}
	}
	w := s.window(pane, pid)
	for _, parts := range [][]agent.Part{{{Key: agent.Escape}}, {{Paste: "x"}, {Key: agent.Enter}}} {
		if err := (&Holder{}).Input(t.Context(), w, parts); !errors.Is(err, ErrTakesNoInput) {
			t.Errorf("%q: %v", parts, err)
		}
	}
	if got := s.run("list-buffers"); got != "" {
		t.Errorf("buffers left: %s", got)
	}
}

// agentBelow starts a pane as a person has one: a shell with job control, and
// the agent they started in it, which writes what it reads to the file
// returned. Once the agent leaves the foreground, the shell reads its
// terminal, as a shell at its prompt does, into "<file>-shell".
func (s *server) agentBelow(program string) (holder.Window, string) {
	s.t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		s.t.Skip("no bash for exec -a")
	}
	out, pidFile := filepath.Join(s.dir, fmt.Sprintf("below%d", s.n)), filepath.Join(s.dir, fmt.Sprintf("pid%d", s.n))
	if err := os.WriteFile(out+"-agent", []byte(fmt.Sprintf("echo $$ > %s\n%s > %s\n", pidFile, program, out)), 0o600); err != nil {
		s.t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		s.t.Fatal(err)
	}
	pane, _ := s.pane("stty raw -echo; exec " + quote(self) + " shell " + out)
	pid, err := strconv.Atoi(strings.TrimSpace(string(waitFile(s.t, pidFile))))
	if err != nil {
		s.t.Fatal(err)
	}
	w := s.window(pane, pid)
	s.runs(pid, "cat")
	for deadline := time.Now().Add(10 * time.Second); (&Holder{}).CheckTakeIn(s.t.Context(), w) != nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatal((&Holder{}).CheckTakeIn(s.t.Context(), w))
		}
	}
	return w, out
}

// shellMain is the shell of agentBelow. It is not a real one: bash has no job
// control in a script on macOS that sees the agent stop.
func shellMain(out string) error {
	signal.Ignore(syscall.SIGTTOU)
	c := exec.Command("bash", out+"-agent")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{Foreground: true}
	if err := c.Start(); err != nil {
		return err
	}
	if _, err := syscall.Wait4(c.Process.Pid, nil, syscall.WUNTRACED, nil); err != nil {
		return err
	}
	if err := unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, syscall.Getpgrp()); err != nil {
		return err
	}
	if err := os.WriteFile(out+"-back", []byte("x"), 0o600); err != nil {
		return err
	}
	f, err := os.Create(out + "-shell")
	if err != nil {
		return err
	}
	_, err = io.Copy(f, os.Stdin)
	return err
}

// The agent of a window hataori did not start may run below the pane's own
// process, a shell, and have any name.
func TestInputToAnAgentBelowThePanesProcess(t *testing.T) {
	s := newServer(t)
	for _, program := range []string{"exec cat", "exec -a 2.1.331 cat", "exec -a 'a,b}c#d{e' cat", "exec -a '#{pid}' cat", "exec -a '%0 #,}' cat", "exec -a 'x日本y it' cat", "exec -a '' cat", "exec -a $'cl\\bx' cat", "exec -a 100% cat"} {
		w, out := s.agentBelow(program)
		if err := (&Holder{}).Input(t.Context(), w, []agent.Part{{Paste: "x"}, {Key: agent.Enter}}); err != nil {
			t.Errorf("%s: %v", program, err)
		} else if got := waitBytes(t, out, 2); got != "x\r" {
			t.Errorf("%s: it read %q", program, got)
		}
	}
}

// A person may suspend or leave the agent that was taken in at any moment,
// also between any two of the holder's commands: their shell is then in the
// foreground, and reads nothing of the input.
func TestInputWhileTheAgentLeavesTheForeground(t *testing.T) {
	s := newServer(t)
	paste := []agent.Part{{Paste: "never"}, {Key: agent.Enter}}
	for _, c := range []struct {
		signal, program string
		parts           []agent.Part
	}{
		{"STOP", "exec cat", paste},
		{"KILL", "exec cat", paste},
		{"STOP", "exec cat", paste[1:]},
		{"KILL", "exec cat", paste[1:]},
		// Nothing of its name is sure to be the same when tmux prints it.
		{"STOP", "exec -a 日本 cat", paste},
	} {
		for n := 1; n <= 6; n++ {
			w, out := s.agentBelow(c.program)
			h := &Holder{Tmux: s.before(n, fmt.Sprintf("kill -%s %d; until [ -s %s-back ]; do sleep 0.02; done", c.signal, w.Process, out))}
			err := h.Input(t.Context(), w, c.parts)
			if _, left := os.Stat(out + "-back"); left != nil {
				if err != nil && !errors.Is(err, errNoName) { // fewer commands than n
					t.Errorf("%s, %q: %v", c.signal, c.parts, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s before command %d: %q was handed over", c.signal, n, c.parts)
			}
			time.Sleep(200 * time.Millisecond)
			if b, _ := os.ReadFile(out + "-shell"); len(b) > 0 {
				t.Errorf("%s before command %d: the shell read %q", c.signal, n, b)
			}
			if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
				t.Errorf("buffers left: %q", b)
			}
		}
	}
}

// A person may give the window's pane another program, also one of the
// agent's name: it reads nothing of the input.
func TestInputWhileThePaneIsRespawned(t *testing.T) {
	s := newServer(t)
	for _, program := range []string{"tee", "cat"} {
		for n := 1; n <= 6; n++ {
			w, _ := s.agentBelow("exec cat")
			r, _ := parseRef(w)
			theirs := filepath.Join(s.dir, fmt.Sprintf("theirs-%s-%d", program, n))
			h := &Holder{Tmux: s.before(n, fmt.Sprintf("%s -S %s respawn-pane -k -t %s 'stty raw -echo; echo x > %s-ready; exec %s > %s'; until [ -s %s-ready ]; do sleep 0.02; done",
				s.tmux, s.sock, r.Pane, theirs, program, theirs, theirs))}
			err := h.Input(t.Context(), w, []agent.Part{{Paste: "never"}, {Key: agent.Enter}})
			if _, left := os.Stat(theirs + "-ready"); left != nil {
				if err != nil { // fewer commands than n
					t.Errorf("%s: %v", program, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s before command %d: handed over", program, n)
			}
			time.Sleep(200 * time.Millisecond)
			if b, _ := os.ReadFile(theirs); len(b) > 0 {
				t.Errorf("%s before command %d: it read %q", program, n, b)
			}
		}
	}
}

// A hook a person set on tmux's own commands may print something, or fail
// after the paste: the input was handed over.
func TestInputBesideAPersonsHooks(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	h := &Holder{}
	s.run("set-hook", "-g", "after-display-message", "display-message -p hooked")
	s.run("set-hook", "-g", "after-paste-buffer", "display-message -p hooked")
	want := ""
	for _, hook := range []string{"", "select-pane -t :.9"} {
		if hook != "" {
			s.run("set-hook", "-gu", "after-display-message")
			s.run("set-hook", "-g", "after-paste-buffer", hook)
		}
		if err := h.Input(t.Context(), w, []agent.Part{{Paste: "ok"}}); err != nil {
			t.Errorf("a paste with the hook %q: %v", hook, err)
		}
		if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}}); err != nil {
			t.Errorf("its Enter: %v", err)
		}
		want += "\x1b[200~ok\x1b[201~\r"
	}
	if got := waitBytes(t, out, len(want)); got != want {
		t.Errorf("the pane read %q, want %q", got, want)
	}
}

// notSent is the *holder.NotSent of err, if any.
func notSent(err error) *holder.NotSent {
	var no *holder.NotSent
	errors.As(err, &no)
	return no
}

// An input's error says whether nothing of it was handed over, and whether
// the window refused it as it is (#741).
func TestWhatAFailedInputHandedOver(t *testing.T) {
	s := newServer(t)
	w, _ := s.reader()
	r, _ := parseRef(w)
	in := []agent.Part{{Paste: "x"}, {Key: agent.Enter}}

	h := &Holder{}
	if no := notSent(h.Input(t.Context(), w, []agent.Part{{Key: "no such key"}})); no == nil {
		t.Errorf("a key with no name: %+v", no)
	}
	// The client killed after it asked the server: not known.
	killed := filepath.Join(s.dir, "killed-tmux")
	syscall.ForkLock.RLock() // as in before
	err := os.WriteFile(killed, []byte(fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *if-shell*) kill -9 $$;; esac\nexec %s \"$@\"\n", s.tmux)), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	h.Tmux = killed
	if err := h.Input(t.Context(), w, in); err == nil || notSent(err) != nil {
		t.Errorf("a client killed at the hand-over: %v", err)
	}
	// The next Enter is refused, the paste not being known handed over.
	if no := notSent(h.Input(t.Context(), w, in[1:])); no == nil || !no.Refused {
		t.Errorf("the Enter after it: %+v", no)
	}
	s.run("kill-pane", "-t", r.Pane)
	if no := notSent((&Holder{}).Input(t.Context(), w, in)); no == nil {
		t.Error("a pane that is gone: not known that nothing was sent")
	}
}

// A codec presses Enter some time after its paste. When the paste was
// refused, that Enter would submit what the person has typed in the window:
// it is refused too, until a paste is handed over. Other keys are not.
func TestNoEnterAfterARefusedPaste(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	other, otherOut := s.reader()
	r, _ := parseRef(w)
	h := &Holder{}
	s.run("copy-mode", "-t", r.Pane)
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: "refused"}}); !errors.Is(err, ErrTakesNoInput) {
		t.Fatalf("a paste into a pane in copy mode: %v", err)
	}
	s.run("copy-mode", "-q", "-t", r.Pane)
	if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}}); err == nil {
		t.Error("the Enter of a refused paste was handed over")
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}, {Paste: "x"}}); err == nil {
		t.Error("the Enter of a refused paste was handed over before another paste")
	}
	if err := h.Input(t.Context(), other, []agent.Part{{Key: agent.Enter}}); err != nil {
		t.Errorf("Enter in another window: %v", err)
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Escape}}); err != nil {
		t.Errorf("Escape: %v", err)
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: "next"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}}); err != nil {
		t.Error(err)
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: "x"}, {Key: "no such key"}}); err == nil {
		t.Error("a key with no name was handed over")
	}
	if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}}); err == nil {
		t.Error("the Enter of a paste that could not be encoded was handed over")
	}
	want := "\x1b" + "\x1b[200~next\x1b[201~" + "\r"
	if got := waitBytes(t, out, len(want)); got != want {
		t.Errorf("the pane read %q, want %q", got, want)
	}
	if got := waitBytes(t, otherOut, 1); got != "\r" {
		t.Errorf("the other pane read %q", got)
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
	ps, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,tty=,comm=").Output()
	if err != nil {
		s.t.Fatalf("ps: %v", err)
	}
	var procs []string
	for _, l := range strings.Split(string(ps), "\n") {
		if f := strings.Fields(l); len(f) >= 3 && f[2] == tty {
			procs = append(procs, strings.Join(f, " "))
		}
	}
	return strings.Join([]string{panes, strings.Join(procs, "\n"),
		s.run("show-options", "-p", "-t", pane), s.run("show-options", "-w", "-t", pane), s.run("show-options", "-s")}, "\n--\n")
}

// runs waits until the process pid runs the program name: a pane's process
// is the tmux server's fork until it has run the pane's command.
func (s *server) runs(pid int, name string) {
	s.t.Helper()
	var comm string
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var out []byte
		out, err = exec.Command("ps", "-o", "ucomm=", "-p", strconv.Itoa(pid)).Output()
		if comm = strings.TrimSpace(string(out)); filepath.Base(comm) == name {
			return
		}
	}
	s.t.Fatalf("process %d runs %q, not %s (ps: %v)", pid, comm, name, err)
}

func TestCapsStopAndNothingChanges(t *testing.T) {
	s := newServer(t)
	pane, pid := s.pane("exec cat > /dev/null")
	s.runs(pid, "cat")
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
	size := s.run("display-message", "-p", "-t", pane, "#{pane_width}x#{pane_height}") // not 100x30 on 3.2a
	if c, ok := <-ch; !ok || !c.Reset || fmt.Sprintf("%dx%d", c.Cols, c.Rows) != size {
		t.Fatalf("snapshot %+v, %t, of a pane of %s", c, ok, size)
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

	// How tmux 3.0a and 3.1c say it (testdata, 3.1c), and another failure.
	says, err := os.ReadFile("testdata/display-message-gone-3.1c.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		says string
		want holder.State
	}{{strings.ReplaceAll(strings.TrimSpace(string(says)), "%0", pane), holder.Gone}, {"lost server", holder.Unknown}, {"can't find pane: " + pane + "0", holder.Unknown}} {
		fails := filepath.Join(s.dir, "fails-tmux")
		syscall.ForkLock.RLock()
		os.WriteFile(fails, []byte("#!/bin/sh\ncase \"$*\" in *display-message*) echo \""+c.says+"\" >&2; exit 1;; esac\nexec "+s.tmux+" \"$@\"\n"), 0o700)
		syscall.ForkLock.RUnlock()
		if st := state2(t, &Holder{Tmux: fails}, w); st != c.want {
			t.Errorf("display-message says %q: %v, not %v", c.says, st, c.want)
		}
	}

	fake := filepath.Join(s.dir, "fake-tmux")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'lost the connection' >&2\nexit 1\n"), 0o700)
	if st := state2(t, &Holder{Tmux: fake}, s.window(other, pid)); st != holder.Unknown {
		t.Fatalf("tmux that cannot be reached: %v", st)
	}
	// tmux that answers with nothing says nothing about the window (#497).
	empty := filepath.Join(s.dir, "empty-tmux")
	os.WriteFile(empty, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	if st := state2(t, &Holder{Tmux: empty}, s.window(other, pid)); st != holder.Unknown {
		t.Fatalf("tmux that prints nothing: %v", st)
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
