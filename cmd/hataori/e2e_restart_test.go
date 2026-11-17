package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// The end-to-end suite for what goes wrong around a turn (design, section
// 8). The daemon runs as a process of its own, so that a test can kill
// it; the chat service it talks to stays in the test (remote_test.go).

type procRoot struct {
	*chatRoot
	svc    *service
	daemon *exec.Cmd
	log    *bytes.Buffer
	logMu  *sync.Mutex
}

// newProcRoot is newChatRoot, with the chat service behind the bridge.
func newProcRoot(t *testing.T, change func(cfg map[string]any)) *procRoot {
	t.Helper()
	p := &procRoot{log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	var sock string
	p.chatRoot = newChatRoot(t, func(cfg map[string]any) {
		cfg["drivers"] = map[string]any{remoteSurface: map[string]any{"socket": "SOCKET"}}
		cfg["scopes"].([]map[string]any)[0]["client"] = remoteSurface
		if change != nil {
			change(cfg)
		}
	})
	p.svc = startService(t, p.driver)
	sock = p.svc.sock
	// The socket is known only now: put it into the config.
	path := filepath.Join(p.root, instance.ConfigFile)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(b, []byte(`"SOCKET"`), []byte(strconv.Quote(sock)), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.kill(t)
		killSessions(p.root)
	})
	return p
}

// start starts the daemon as a process and waits until it serves and its
// front end is attached to the service.
func (p *procRoot) start(t *testing.T) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "daemon", p.root)
	w := lockedWriter{p.logMu, p.log}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.daemon = cmd
	eventually(t, "the daemon to serve", 15*time.Second, func() bool {
		c, err := wire.Dial(p.root)
		if err == nil {
			c.Close()
		}
		return err == nil && p.svc.isAttached()
	})
	time.Sleep(50 * time.Millisecond) // its first catch-up
}

// kill kills the daemon the test started with SIGKILL, and nothing else.
func (p *procRoot) kill(t *testing.T) {
	t.Helper()
	if p.daemon == nil {
		return
	}
	p.daemon.Process.Signal(syscall.SIGKILL)
	p.daemon.Wait()
	p.daemon = nil
	eventually(t, "the service to see the daemon go", 5*time.Second, func() bool { return !p.svc.isAttached() })
}

func (p *procRoot) restart(t *testing.T) {
	t.Helper()
	p.kill(t)
	p.start(t)
}

// in posts text into conv as the webhook.
func (p *procRoot) in(conv surface.Conversation, text string) {
	p.driver.Inject(conv, fakesurface.Webhook(e2eHook), text)
}

func (p *procRoot) agentSession(t *testing.T) (n int, id string) {
	t.Helper()
	p.db(t).QueryRow(`SELECT count(*), max(agent_session_id) FROM sessions`).Scan(&n, &id)
	return
}

// agentPID is the pid of the session's agent, as its host wrote it.
func (p *procRoot) agentPID(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runner.StateDir(p.root, 1), host.PIDFile))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func alive(pid int) bool { return pid > 1 && syscall.Kill(pid, 0) == nil }

func TestDaemonKilledDuringATurnFinishesIt(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" tool wait 3s\nreply after the kill")
	th := p.thread(t, 0)
	p.posted(t, th, "tool wait 3s\nreply after the kill")
	eventually(t, "the tool", 10*time.Second, func() bool { return len(p.events(t, "tool_started", th)) == 1 })
	p.restart(t)
	p.posted(t, th, "after the kill")
	eventually(t, "the end event", 10*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) > 0 })
	time.Sleep(300 * time.Millisecond)
	if a, b := p.count(th, "tool wait 3s\nreply after the kill"), p.count(th, "after the kill"); a != 1 || b != 1 {
		t.Fatalf("the prompt was posted %d times, the answer %d", a, b)
	}
	if n := len(p.events(t, "turn_ended", th)); n != 1 {
		t.Fatalf("%d end events", n)
	}
}

func TestEventsAfterNOnceTheDaemonWasRestarted(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" reply x")
	th := p.thread(t, 0)
	eventually(t, "the end event", 10*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	_, all, _ := cli(t, "events", "--root", p.root)
	lines := strings.Split(strings.TrimSpace(all), "\n")
	var mid struct{ Pos uint64 }
	json.Unmarshal([]byte(lines[len(lines)/2]), &mid)
	p.restart(t)
	_, after, _ := cli(t, "events", "--root", p.root, "--after", strconv.FormatUint(mid.Pos, 10))
	want := strings.Join(lines[len(lines)/2+1:], "\n")
	if got := strings.TrimSpace(after); got != want {
		t.Fatalf("events --after %d:\n%s\nwant\n%s", mid.Pos, got, want)
	}
}

func TestThreePostsWhileTheDaemonIsDownRunOnceEachInOrder(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" reply first")
	th := p.thread(t, 0)
	p.posted(t, th, "first")
	p.kill(t)
	for _, s := range []string{"one", "two", "three"} {
		p.in(th, "reply "+s)
	}
	p.start(t)
	p.posted(t, th, "three")
	time.Sleep(300 * time.Millisecond)
	var answers []string
	for _, c := range p.driver.Calls(th) {
		if c.Op == fakesurface.OpPost && (c.Message.Text == "one" || c.Message.Text == "two" || c.Message.Text == "three") {
			answers = append(answers, c.Message.Text)
		}
	}
	if got := strings.Join(answers, ","); got != "one,two,three" {
		t.Fatalf("answers %s", got)
	}
}

func TestIdlePeriodEndsTheAgentAndTheNextPostResumes(t *testing.T) {
	conv := filepath.Join(t.TempDir(), "conversations")
	os.MkdirAll(conv, 0o700)
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "1s"
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv}}
	})
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" reply a")
	th := p.thread(t, 0)
	p.posted(t, th, "a")
	pid := p.agentPID(t)
	_, before := p.agentSession(t)
	eventually(t, "the agent to be put to sleep", 15*time.Second, func() bool { return !alive(pid) })
	_, out, _ := cli(t, "thread", "--root", p.root, th.Thread)
	if !strings.Contains(out, `"state":"asleep"`) {
		t.Fatalf("thread after the idle period: %s", out)
	}
	p.in(th, "reply b")
	p.posted(t, th, "b")
	if n, after := p.agentSession(t); n != 1 || after != before || before == "" {
		t.Fatalf("%d sessions, agent session %q then %q; want the same conversation", n, before, after)
	}
}

func TestEverySessionProcessKilledThenTheNextPostResumes(t *testing.T) {
	conv := filepath.Join(t.TempDir(), "conversations")
	os.MkdirAll(conv, 0o700)
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv}}
	})
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" reply a")
	th := p.thread(t, 0)
	p.posted(t, th, "a")
	_, before := p.agentSession(t)
	pid := p.agentPID(t)
	p.kill(t)            // the daemon,
	killSessions(p.root) // and the session's host and agent, as a reboot would
	eventually(t, "the agent to go", 5*time.Second, func() bool { return !alive(pid) })
	p.start(t)
	p.in(th, "reply b")
	p.posted(t, th, "b")
	if n, after := p.agentSession(t); n != 1 || after != before || before == "" {
		t.Fatalf("%d sessions, agent session %q then %q; want the same conversation", n, before, after)
	}
}

func TestStopEndsARunningToolWithinTenSeconds(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
	th := p.thread(t, 0)
	eventually(t, "the tool", 10*time.Second, func() bool { return len(p.events(t, "tool_started", th)) == 1 })
	pgid := p.agentPID(t) // the agent leads the session's process group
	others := func() []int {
		var pids []int
		out, _ := exec.Command("ps", "-A", "-o", "pid=,pgid=").Output()
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 {
				pid, _ := strconv.Atoi(f[0])
				if g, _ := strconv.Atoi(f[1]); g == pgid && pid != pgid {
					pids = append(pids, pid)
				}
			}
		}
		return pids
	}
	if len(others()) == 0 {
		t.Fatal("the tool's process is not in the session's group")
	}
	start := time.Now()
	if code, _, errOut := cli(t, "stop", "--root", p.root, th.Thread); code != 0 {
		t.Fatalf("stop: exit %d, %s", code, errOut)
	}
	eventually(t, "the end of the turn", 10*time.Second-time.Since(start), func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	eventually(t, "the tool's process to go", 10*time.Second-time.Since(start), func() bool { return len(others()) == 0 })
}
