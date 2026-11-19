package window

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// The socket is in a directory only the instance's OS user can open: the
// daemon makes it so and refuses one that is not, and a hook sends nothing
// to a socket in one that is not.
func TestSocketDirectoryIsPrivate(t *testing.T) {
	root := t.TempDir()
	s, err := Listen(root)
	if err != nil {
		t.Fatal(err)
	}
	sock, _ := SocketPath(root)
	st, err := os.Stat(filepath.Dir(sock))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the socket's directory: %v, %v", st.Mode(), err)
	}
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the socket: %v, %v", st.Mode(), err)
	}
	s.Close()

	// Another user could enter the directory: the daemon does not listen,
	// and a hook does not connect to what listens there.
	dir := filepath.Dir(sock)
	// A short path under /tmp went with the socket, and is not under the
	// test's temporary directory.
	t.Cleanup(func() { os.Remove(sock); os.Remove(dir) })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if s, err := Listen(root); err == nil {
		s.Close()
		t.Fatal("the daemon listened in a directory others can enter")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
			accepted <- struct{}{}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Send(ctx, root, report("s1", "SessionStart")); err == nil {
		t.Fatal("a report was sent to a directory others can enter")
	}
	select {
	case <-accepted:
		t.Fatal("the hook connected")
	case <-time.After(100 * time.Millisecond):
	}
}

func report(session, event string) Report {
	return Report{Holder: Tmux, Server: "/tmp/tmux-1000/default,4242", Pane: "%7", Device: "/dev/pts/9", PID: 4300,
		Agent: "claude-code", Session: session, Dir: "/home/alice/src", Event: event, Interactive: true}
}

func pane(id, device string) control.HostTerminal {
	return control.HostTerminal{Device: device, Tmux: &control.TmuxPane{Pane: id}}
}

// A pane gets the report of the session whose process has the pane's
// terminal. Only interactive sessions are listed; the session's end
// removes it; a report whose pane is gone is dropped.
func TestReportsOfPanes(t *testing.T) {
	s := newServer()
	s.alive = func(pid int) bool { return pid == 4300 }
	w := report("", "").Window()

	s.take(report("s1", "SessionStart"))
	list := []control.HostTerminal{pane("%7", "/dev/pts/9"), pane("%8", "/dev/pts/10"), {Device: "/dev/pts/11"}}
	s.Annotate(list, time.Now())
	if r := list[0].Reported; r == nil || r.Window != w || r.Session != "s1" || r.Event != "SessionStart" || r.Agent != "claude-code" ||
		r.Dir != "/home/alice/src" || r.At.IsZero() || list[1].Reported != nil || list[2].Reported != nil {
		t.Fatalf("listed: %+v", list)
	}

	// `claude -p` from the session's own shell sees the same pane.
	p := report("s2", "SessionStart")
	p.Interactive = false
	s.take(p)
	if r, _ := s.Last(w); r.Session != "s1" {
		t.Fatalf("a session that is not interactive replaced the pane's: %+v", r)
	}

	// The same pane id on another terminal is another pane.
	list = []control.HostTerminal{pane("%7", "/dev/pts/12")}
	s.take(report("s1", "UserPromptSubmit"))
	s.Annotate(list, time.Now())
	if list[0].Reported != nil {
		t.Fatalf("a pane with another terminal: %+v", list[0].Reported)
	}
	if _, ok := s.Last(w); ok {
		t.Fatal("a report whose pane is not listed was kept")
	}

	s.take(report("s1", "SessionStart"))
	s.take(report("other", Ends)) // another session's end
	if _, ok := s.Last(w); !ok {
		t.Fatal("another session's end removed the pane's report")
	}
	s.take(report("s1", Ends))
	if _, ok := s.Last(w); ok {
		t.Fatal("the session's end left its report")
	}
	// A hook of the ended session that ran late does not bring it back; the
	// session resumed does.
	s.take(report("s1", "Stop"))
	if _, ok := s.Last(w); ok {
		t.Fatal("a late report of an ended session brought it back")
	}
	s.take(report("s1", "SessionStart"))
	if _, ok := s.Last(w); !ok {
		t.Fatal("the session resumed in the pane is not listed")
	}

	// A report taken while the list was read stays, though its pane is
	// not in that list yet.
	before := time.Now()
	time.Sleep(time.Millisecond)
	s.take(report("s3", "SessionStart"))
	s.Annotate(nil, before)
	if _, ok := s.Last(w); !ok {
		t.Fatal("a report newer than the list was dropped")
	}

	// The session's process is gone, with no SessionEnd: its pane shows
	// nothing.
	s.alive = func(int) bool { return false }
	list = []control.HostTerminal{pane("%7", "/dev/pts/9")}
	s.Annotate(list, time.Now())
	if list[0].Reported != nil {
		t.Fatalf("a report whose process is gone: %+v", list[0].Reported)
	}
}
