package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/surface"
)

// syncBuffer is a bytes.Buffer safe for one writer and readers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// attached is one `hataori attach` the test runs, with its keys written
// into keys.
type attached struct {
	keys        *io.PipeWriter
	out, errOut *syncBuffer
	code        chan int
}

func attach(t *testing.T, root, key string) *attached {
	t.Helper()
	in, keys := io.Pipe()
	a := &attached{keys: keys, out: &syncBuffer{}, errOut: &syncBuffer{}, code: make(chan int, 1)}
	attachStdin = in
	t.Cleanup(func() { keys.Close(); attachStdin = os.Stdin })
	go func() { a.code <- run([]string{"attach", "--root", root, key}, a.out, a.errOut) }()
	return a
}

// detach presses the detach key and waits for the command to end.
func (a *attached) detach(t *testing.T) {
	t.Helper()
	a.keys.Write([]byte{detachKey})
	select {
	case code := <-a.code:
		if code != 0 || !strings.Contains(a.errOut.String(), "detached") {
			t.Fatalf("attach ended with %d: %s", code, a.errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the detach key did not end hataori attach")
	}
}

func (a *attached) running(t *testing.T) {
	t.Helper()
	select {
	case code := <-a.code:
		t.Fatalf("hataori attach ended with %d: %s", code, a.errOut.String())
	default:
	}
}

func terminalProcRoot(t *testing.T, idle string) *procRoot {
	t.Helper()
	conv := filepath.Join(t.TempDir(), "conversations")
	os.MkdirAll(conv, 0o700)
	return newProcRoot(t, func(cfg map[string]any) {
		cfg["idle"] = idle
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv, Terminal: true, Hataori: os.Args[0]}}
	})
}

func (p *procRoot) threadState(t *testing.T, th surface.Conversation) string {
	t.Helper()
	var s string
	p.db(t).QueryRow(`SELECT state FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&s)
	return s
}

// The acceptance criteria of #253 with a fake agent in a terminal:
// attaching shows the screen, a line typed there is a turn of the
// terminal with one start and one end, which chat shows marked as typed at
// the terminal; a daemon restart ends neither the agent nor the attached
// terminal; while attached the thread is not put to sleep, and after a
// detach it is; the next message from chat continues the same session.
func TestAttachTypeDetachAndTheSessionGoesOn(t *testing.T) {
	p := terminalProcRoot(t, "2s")
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" screen on-the-screen")
	th := p.thread(t, 0)
	eventually(t, "the first turn to end", 15*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	pid := p.agentPID(t)
	_, session := p.agentSession(t)

	a := attach(t, p.root, th.Thread)
	eventually(t, "the screen", 15*time.Second, func() bool { return strings.Contains(a.out.String(), "on-the-screen") })

	// Attached for longer than the idle period: not put to sleep.
	time.Sleep(4 * time.Second)
	if s := p.threadState(t, th); s != "idle" || !alive(pid) {
		t.Fatalf("attached past the idle period: state %s, agent alive %v", s, alive(pid))
	}

	// A daemon restart ends neither the agent nor the attached terminal.
	p.restart(t)
	a.running(t)
	if !alive(pid) {
		t.Fatal("the daemon restart ended the agent")
	}

	a.keys.Write([]byte("reply typed here\r"))
	eventually(t, "the typed line on the screen", 10*time.Second, func() bool { return strings.Contains(a.out.String(), "reply typed here") })
	p.posted(t, th, "(typed at the terminal)\ntyped here")
	var started, ended []map[string]any
	eventually(t, "the terminal turn to end", 15*time.Second, func() bool {
		started, ended = p.events(t, "turn_started", th), p.events(t, "turn_ended", th)
		return len(ended) == 2
	})
	if len(started) != 2 || started[1]["by"] != "terminal" || ended[1]["by"] != "terminal" || ended[1]["outcome"] != "completion" {
		t.Fatalf("turn_started %v, turn_ended %v; want one more turn, by the terminal", started, ended)
	}

	a.detach(t)
	if s := p.threadState(t, th); s != "idle" || !alive(pid) {
		t.Fatalf("after the detach: state %s, agent alive %v; want it idle and running", s, alive(pid))
	}
	p.in(th, "reply from chat")
	p.posted(t, th, "from chat")
	if n, now := p.agentSession(t); n != 1 || now != session || session == "" {
		t.Fatalf("%d sessions, agent session %q then %q; want the same conversation", n, session, now)
	}

	// Detached, the idle period puts it to sleep.
	eventually(t, "the agent to be put to sleep", 20*time.Second, func() bool { return !alive(pid) })
	if s := p.threadState(t, th); s != "asleep" {
		t.Fatalf("after the idle period: state %s", s)
	}

	// Attaching to an asleep thread resumes it first, with no turn.
	turns := len(p.events(t, "turn_started", th))
	a = attach(t, p.root, th.Thread)
	eventually(t, "the attach to an asleep thread", 15*time.Second, func() bool {
		return strings.Contains(a.errOut.String(), "attached to thread")
	})
	eventually(t, "the woken thread to be idle", 15*time.Second, func() bool { return p.threadState(t, th) == "idle" })
	a.keys.Write([]byte("reply after the wake\r"))
	p.posted(t, th, "(typed at the terminal)\nafter the wake")
	a.detach(t)
	if got := len(p.events(t, "turn_started", th)); got != turns+1 {
		t.Fatalf("%d turns started after the wake and one typed line; want %d", got, turns+1)
	}
	if n, now := p.agentSession(t); n != 1 || now != session {
		t.Fatalf("%d sessions, agent session %q then %q; want the same conversation", n, session, now)
	}
}

// A thread whose agent runs on pipes has no terminal: attaching is
// refused with a message, and nothing changes.
func TestAttachToAnAgentOnPipesIsRefused(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" reply a")
	th := p.thread(t, 0)
	p.posted(t, th, "a")
	code, _, errOut := cli(t, "attach", "--root", p.root, th.Thread)
	if code != exitError || !strings.Contains(errOut, "no terminal") {
		t.Fatalf("attach to an agent on pipes: exit %d, %q", code, errOut)
	}
	if s := p.threadState(t, th); s != "idle" {
		t.Fatalf("state %s after a refused attach", s)
	}
}
