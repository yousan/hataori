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
	svc        *service
	daemon     *exec.Cmd
	log        *bytes.Buffer
	logMu      *sync.Mutex
	afterStart func(*exec.Cmd)
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
		p.stopDaemon()
		killSessions(p.root)
		waitSessionsGone(p.root)
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
	if p.afterStart != nil {
		p.afterStart(cmd)
	}
	eventually(t, "the daemon to serve", 15*time.Second, func() bool {
		c, err := wire.Dial(p.root)
		if err == nil {
			c.Close()
		}
		return err == nil && p.svc.isAttached()
	})
	// Its front end has caught up once: the channel is read from now on.
	eventually(t, "the first catch-up", 10*time.Second, func() bool {
		var n int
		p.db(t).QueryRow(`SELECT count(*) FROM chat_inbound WHERE channel = 'c1' AND since > 0`).Scan(&n)
		return n == 1
	})
}

// stopDaemon kills the daemon the test started, if it runs, and waits for
// it; it does not fail the test, so that cleanup goes on.
func (p *procRoot) stopDaemon() {
	if p.daemon != nil {
		p.daemon.Process.Signal(syscall.SIGKILL)
		p.daemon.Wait()
		p.daemon = nil
	}
}

// waitSessionsGone waits, up to five seconds, until no host or agent whose
// pid is in a session directory under root runs.
func waitSessionsGone(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		left := false
		for _, d := range dirs {
			for _, f := range []string{host.PIDFile, host.HostPIDFile} {
				if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
					if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); alive(pid) && !zombie(pid) {
						left = true
					}
				}
			}
		}
		if !left {
			return
		}
	}
}

// zombie reports whether pid has ended and waits only to be reaped.
func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	return len(f) > 2 && f[2] == "Z"
}

// kill kills the daemon the test started with SIGKILL, and nothing else.
func (p *procRoot) kill(t *testing.T) {
	t.Helper()
	if p.daemon == nil {
		return
	}
	p.stopDaemon()
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
	p.in(c1, "!go repo:"+bareRepo(t)+" tool wait 4s\nreply after the kill")
	th := p.thread(t, 0)
	p.posted(t, th, "tool wait 4s\nreply after the kill")
	eventually(t, "the tool", 10*time.Second, func() bool { return len(p.events(t, "tool_started", th)) == 1 })
	p.kill(t)
	if len(p.events(t, "tool_ended", th)) != 0 {
		t.Fatal("the tool ended before the daemon was killed; the kill was not during the turn")
	}
	p.start(t)
	p.posted(t, th, "after the kill")
	eventually(t, "the end event", 10*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) > 0 })
	time.Sleep(300 * time.Millisecond)
	if a, b := p.count(th, "tool wait 4s\nreply after the kill"), p.count(th, "after the kill"); a != 1 || b != 1 {
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
	// What the agent got: each post once, in order, and four turns in all.
	eventually(t, "four turns", 10*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 4 })
	time.Sleep(300 * time.Millisecond)
	var inputs []string
	for _, e := range p.events(t, "input", th) {
		inputs = append(inputs, e["text"].(string))
	}
	if got := strings.Join(inputs, ","); got != "reply first,reply one,reply two,reply three" {
		t.Fatalf("inputs %s", got)
	}
	if n := len(p.events(t, "turn_ended", th)); n != 4 {
		t.Fatalf("%d turns", n)
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
	_, out, _ := cli(t, "thread", "--root", p.root, numberOf(t, p.root, th))
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
	eventually(t, "the tool's process in the session's group", 10*time.Second, func() bool { return len(others()) > 0 })
	start := time.Now()
	if code, _, errOut := cli(t, "stop", "--root", p.root, numberOf(t, p.root, th)); code != 0 {
		t.Fatalf("stop: exit %d, %s", code, errOut)
	}
	eventually(t, "the end of the turn", 10*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	eventually(t, "the tool's process to go", 10*time.Second, func() bool { return len(others()) == 0 })
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the tool ended %v after the stop", took)
	}
	// The agent and its host stay, for the next message; the tool is gone.
}

func TestAThreadKeepsItsAgentAcrossSleepRestartAndAConfigChange(t *testing.T) {
	conv := filepath.Join(t.TempDir(), "conversations")
	if err := os.MkdirAll(conv, 0o700); err != nil {
		t.Fatal(err)
	}
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "1s"
		cfg["agent"] = fake.Name
		cfg["scopes"].([]map[string]any)[0]["agent"] = secondAgent
		cfg["adapters"] = map[string]any{
			fake.Name:   map[string]any{"command": []string{os.Args[0], "fake"}, "env": []string{"PATH=" + os.Getenv("PATH")}},
			secondAgent: map[string]any{"command": []string{os.Args[0], "fake"}, "env": []string{"PATH=" + os.Getenv("PATH")}, "conversations": conv, "model": "second-model", "effort": "low", "report_effort": "max"},
		}
	})
	files := func(want int) {
		t.Helper()
		entries, err := os.ReadDir(conv)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != want {
			t.Fatalf("conversation files: %d, want %d", len(entries), want)
		}
	}
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" reply a")
	th := p.thread(t, 0)
	p.posted(t, th, "a")
	files(1)
	initialFiles, err := os.ReadDir(conv)
	if err != nil {
		t.Fatal(err)
	}
	initialFile := initialFiles[0].Name()
	var threadID, sessionID int64
	var name, model, effort, before string
	if err := p.db(t).QueryRow(`SELECT id FROM threads ORDER BY id LIMIT 1`).Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if err := p.db(t).QueryRow(`SELECT id, agent, model, effort, agent_session_id FROM sessions WHERE thread_id = ?`, threadID).Scan(&sessionID, &name, &model, &effort, &before); err != nil {
		t.Fatal(err)
	}
	if name != secondAgent || model != "second-model" || effort != "max" || before == "" {
		t.Fatalf("agent %q model %q effort %q conversation %q", name, model, effort, before)
	}
	check := func() {
		t.Helper()
		var count int
		var agentName, id, reportedEffort string
		if err := p.db(t).QueryRow(`SELECT count(*), max(agent), max(agent_session_id), max(effort) FROM sessions WHERE thread_id = ?`, threadID).Scan(&count, &agentName, &id, &reportedEffort); err != nil {
			t.Fatal(err)
		}
		if count != 1 || agentName != secondAgent || id != before {
			t.Fatalf("sessions %d, agent %q, conversation %q; want 1, %q, %q", count, agentName, id, secondAgent, before)
		}
		if reportedEffort != "max" {
			t.Fatalf("effort %q, want max; a wake by another adapter reports low", reportedEffort)
		}
		files(1)
	}
	sleep := func() {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(runner.StateDir(p.root, sessionID), host.PIDFile))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the old thread's agent to sleep", 15*time.Second, func() bool { return !alive(pid) })
	}
	sleep()
	p.in(th, "reply b")
	p.posted(t, th, "b")
	check()
	sleep()
	p.kill(t)
	path := filepath.Join(p.root, instance.ConfigFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg["scopes"].([]any)[0].(map[string]any), "agent")
	b, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	p.start(t)
	p.in(th, "reply c")
	p.posted(t, th, "c")
	check()
	p.in(c1, "!go repo:"+bareRepo(t)+" reply d")
	newThread := p.thread(t, 1)
	p.posted(t, newThread, "d")
	var newAgent string
	if err := p.db(t).QueryRow(`SELECT s.agent FROM sessions s JOIN threads t ON t.id = s.thread_id WHERE t.id != ? ORDER BY s.id DESC LIMIT 1`, threadID).Scan(&newAgent); err != nil || newAgent != fake.Name {
		t.Fatalf("new thread agent %q: %v", newAgent, err)
	}
	files(1)
	sleep()
	entries, err := os.ReadDir(conv)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(conv, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	p.in(th, "reply e")
	p.posted(t, th, "e")
	var count int
	if err := p.db(t).QueryRow(`SELECT count(*) FROM sessions WHERE thread_id = ?`, threadID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("old thread has %d sessions: %v", count, err)
	}
	var replacementAgent string
	if err := p.db(t).QueryRow(`SELECT agent FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, threadID).Scan(&replacementAgent); err != nil || replacementAgent != secondAgent {
		t.Fatalf("replacement agent %q: %v", replacementAgent, err)
	}
	files(1)
	newFiles, err := os.ReadDir(conv)
	if err != nil {
		t.Fatal(err)
	}
	if newFiles[0].Name() == initialFile {
		t.Fatal("replacement reused the removed conversation file")
	}
}

// A shutdown during a turn: the daemon goes, then every process gets
// SIGTERM, the agent's first. The host stays to record its agent's end and
// that it got SIGTERM, so the turn ends as interrupted, as in a reboot
// (design 4.5). An agent killed on its own while its host runs is a crash.
func TestShutdownDuringATurnEndsItAsInterruptedAndACrashAsCrashed(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
	}{{"shutdown", "interrupted"}, {"crash", "crashed"}} {
		t.Run(c.name, func(t *testing.T) {
			p := newProcRoot(t, nil)
			p.start(t)
			p.in(c1, "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
			th := p.thread(t, 0)
			eventually(t, "the tool", 10*time.Second, func() bool { return len(p.events(t, "tool_started", th)) == 1 })
			dir := runner.StateDir(p.root, 1)
			b, err := os.ReadFile(filepath.Join(dir, host.HostPIDFile))
			if err != nil {
				t.Fatal(err)
			}
			hostPID, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			agent := p.agentPID(t)
			p.kill(t)
			if c.name == "shutdown" {
				syscall.Kill(-agent, syscall.SIGTERM)
				syscall.Kill(-hostPID, syscall.SIGTERM)
			} else {
				syscall.Kill(-agent, syscall.SIGKILL)
			}
			eventually(t, "the host to record the end and go", 10*time.Second, func() bool {
				_, ok, _ := host.ReadStatus(dir)
				return ok && !alive(hostPID) && !alive(agent)
			})
			p.start(t)
			eventually(t, "the turn's end", 10*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) > 0 })
			if e := p.events(t, "turn_ended", th)[0]; e["outcome"] != "failure" || e["reason"] != c.reason {
				st, _, _ := host.ReadStatus(dir)
				t.Fatalf("the turn ended %v; want a failure, %s (the host's status: %+v)", e, c.reason, st)
			}
		})
	}
}
