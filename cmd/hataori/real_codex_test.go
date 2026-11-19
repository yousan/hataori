package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/surface"
)

func TestRealCodex(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CODEX") != "1" {
		t.Skip("set HATAORI_REAL_CODEX=1 to run")
	}
	if runtime.GOOS != "linux" {
		t.Skip("process checks require Linux")
	}
	wrapper, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	wrapper, err = filepath.Abs(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if exec.Command(wrapper, "login", "status").Run() != nil {
		t.Skip("codex is not signed in")
	}
	const cleanupMargin = 2 * time.Minute
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("the live check requires a go test timeout to reserve time for cleanup")
	}
	workDeadline := deadline.Add(-cleanupMargin)
	minimum := 20 * time.Minute
	if os.Getenv("HATAORI_REAL_CODEX_LONG") == "1" {
		minimum += 35 * time.Minute
	}
	if time.Until(workDeadline) < minimum {
		t.Fatalf("insufficient time for live steps and cleanup: need at least %s plus %s cleanup; use the timeout in docs/testing.md", minimum, cleanupMargin)
	}
	before := codexSettings(t)
	t.Cleanup(func() {
		if after := codexSettings(t); after != before {
			t.Errorf("config.toml changed: %s then %s", before, after)
		}
	})
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "20s"
		cfg["model"] = "gpt-6-luna"
		cfg["effort"] = "low"
		cfg["login_relay"] = false
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": wrapper}}
	})
	p.deadline = workDeadline
	var daemonStart uint64
	defer func() {
		if daemonStart == 0 && p.daemon != nil {
			_, daemonStart = codexProcess(p.daemon.Process.Pid)
		}
		var roots, known []processID
		for _, file := range []string{host.HostPIDFile, host.PIDFile} {
			path := filepath.Join(runner.StateDir(p.root, 1), file)
			b, err := os.ReadFile(path)
			if err == nil {
				pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
				if err == nil && pid > 1 && daemonStart != 0 {
					_, start := codexProcess(pid)
					if start >= daemonStart {
						roots = append(roots, processID{pid, start})
					}
				}
			}
			// The shared cleanup must not act on a stale or reused PID.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove pid file: %v", err)
			}
		}
		if daemonStart != 0 {
			known = append(known, codexProcesses(0, p.root, daemonStart)...)
			for _, root := range roots {
				known = append(known, root)
				known = append(known, codexProcesses(root.pid, p.root, daemonStart)...)
			}
		}
		p.stopDaemon()
		for _, proc := range known {
			killCodexProcess(proc, daemonStart)
		}
		waitSessionsGone(p.root)
		var left []processID
		deadline := time.Now().Add(5 * time.Second)
		for daemonStart != 0 {
			left = codexProcesses(0, p.root, daemonStart)
			for _, root := range roots {
				_, start := codexProcess(root.pid)
				if start == root.start {
					left = append(left, codexProcesses(root.pid, p.root, daemonStart)...)
				}
			}
			for _, proc := range known {
				state, start := codexProcess(proc.pid)
				if state != "" && state != "Z" && start == proc.start && !containsProcess(left, proc) {
					left = append(left, proc)
				}
			}
			if len(left) == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		for _, proc := range left {
			killCodexProcess(proc, daemonStart)
		}
		if len(left) != 0 {
			t.Errorf("processes left after stopping sessions: %v", left)
		}
	}()
	t.Cleanup(func() {
		if t.Failed() {
			p.logMu.Lock()
			defer p.logMu.Unlock()
			t.Logf("daemon log:\n%s", p.log.String())
		}
	})
	p.start(t)
	_, daemonStart = codexProcess(p.daemon.Process.Pid)
	if daemonStart == 0 {
		t.Fatal("cannot read daemon start time")
	}
	var th surface.Conversation
	step := func(name string, f func(*testing.T)) bool {
		start := time.Now()
		return t.Run(name, func(t *testing.T) {
			defer func() {
				if t.Failed() && th.Thread != "" {
					p.dump(t, th)
				}
				status := "ok"
				if t.Failed() {
					status = "FAIL"
				} else if t.Skipped() {
					status = "SKIP"
				}
				t.Logf("%s: %s (%s)", name, status, time.Since(start).Round(time.Millisecond))
			}()
			if name != "30-minute turn" || os.Getenv("HATAORI_REAL_CODEX_LONG") == "1" {
				needed := 3 * time.Minute
				if name == "30-minute turn" {
					needed = 35 * time.Minute
				}
				if time.Until(p.deadline) < needed {
					t.Fatalf("insufficient time for %s: need %s before the cleanup margin", name, needed)
				}
			}
			f(t)
		})
	}
	var session, workspace string
	if !step("start", func(t *testing.T) {
		p.in(c1, "!go repo:"+bareRepo(t)+" Run the shell command `true`, then reply with just the word: ok")
		th = p.thread(t, 0)
		eventually(t, "first answer", p.within(3*time.Minute), func() bool {
			return len(p.turnsEnded(t, th)) == 1 && p.replies(th, "ok") > 0 && len(p.list(t, "tool_started", th)) > 0
		})
		_, session = p.agentSession(t)
		if session == "" {
			t.Fatal("missing agent session")
		}
		if err := p.db(t).QueryRow(`SELECT t.workspace FROM threads t JOIN links l ON l.thread_id = t.id WHERE l.channel = ? AND l.thread = ?`, th.Channel, th.Thread).Scan(&workspace); err != nil {
			t.Fatal(err)
		}
	}) {
		return
	}
	if !step("follow-up", func(t *testing.T) {
		ends := len(p.turnsEnded(t, th))
		p.in(th, "Remember the word marigold and reply with just the word: again")
		eventually(t, "follow-up", p.within(3*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > ends && p.replies(th, "again") > 0 })
		if n, id := p.agentSession(t); n != 1 || id != session {
			t.Fatalf("%d sessions, %q then %q", n, session, id)
		}
	}) {
		return
	}
	if !step("question", func(t *testing.T) {
		before := len(p.list(t, "request", th))
		p.in(th, "Use request_user_input_async to ask the user which colour they like, then wait for the answer and reply with it. Do not do anything else.")
		eventually(t, "question posted", p.within(3*time.Minute), func() bool {
			requests := p.list(t, "request", th)
			if len(requests) <= before {
				return false
			}
			text, _ := requests[before].Data["text"].(string)
			return text != "" && p.count(th, text) > 0
		})
		ends := len(p.turnsEnded(t, th))
		p.in(th, "blue")
		eventually(t, "question answer", p.within(3*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > ends && p.replies(th, "blue") > 0 })
	}) {
		return
	}
	if !step("interrupt", func(t *testing.T) {
		app := p.agentPID(t)
		if _, start := codexProcess(app); start == 0 || start < daemonStart {
			t.Fatal("invalid agent process start time")
		}
		baseline := codexProcesses(app, workspace, daemonStart)
		ends, tools := len(p.turnsEnded(t, th)), len(p.list(t, "tool_started", th))
		p.in(th, "Run exactly this one shell command and wait until it has finished before you reply: sleep 600. Do not do anything else.")
		eventually(t, "command start", p.within(3*time.Minute), func() bool { return len(p.list(t, "tool_started", th)) > tools })
		if code, _, out := cli(t, "stop", "--root", p.root, numberOf(t, p.root, th)); code != 0 {
			t.Fatalf("stop: %d: %s", code, out)
		}
		eventually(t, "interrupted turn and command cleanup", p.within(10*time.Second), func() bool {
			ended := p.turnsEnded(t, th)
			if len(ended) <= ends || ended[ends].Data["outcome"] != "failure" || ended[ends].Data["reason"] != "interrupted" {
				return false
			}
			for _, proc := range codexProcesses(app, workspace, daemonStart) {
				if !containsProcess(baseline, proc) {
					return false
				}
			}
			return true
		})
		ends = len(p.turnsEnded(t, th))
		p.in(th, "Reply with just the word: recovered")
		eventually(t, "follow-up after interrupt", p.within(3*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > ends && p.replies(th, "recovered") > 0 })
		if n, id := p.agentSession(t); n != 1 || id != session {
			t.Fatalf("%d sessions, %q then %q", n, session, id)
		}
	}) {
		return
	}
	if !step("daemon restart during turn", func(t *testing.T) {
		ends, tools := len(p.turnsEnded(t, th)), len(p.list(t, "tool_started", th))
		p.in(th, "Run `sleep 30` in the foreground, wait for it to finish, then reply with just the word: restartdone")
		eventually(t, "restart command start", p.within(3*time.Minute), func() bool { return len(p.list(t, "tool_started", th)) > tools })
		if len(p.turnsEnded(t, th)) != ends {
			t.Fatal("turn ended before daemon kill")
		}
		pid := p.daemon.Process.Pid
		_, start := codexProcess(pid)
		if start != daemonStart || start == 0 {
			t.Fatal("daemon PID changed")
		}
		p.kill(t)
		p.start(t)
		eventually(t, "restart answer", p.within(3*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > ends && p.replies(th, "restartdone") > 0 })
		time.Sleep(500 * time.Millisecond)
		if n := len(p.turnsEnded(t, th)) - ends; n != 1 || p.replies(th, "restartdone") != 1 {
			t.Fatalf("%d end events, %d answers", n, p.replies(th, "restartdone"))
		}
	}) {
		return
	}
	if !step("sleep and wake", func(t *testing.T) {
		pid := p.agentPID(t)
		eventually(t, "app-server sleeps", p.within(90*time.Second), func() bool { state, _ := codexProcess(pid); return state == "" || state == "Z" })
		_, out, _ := cli(t, "thread", "--root", p.root, numberOf(t, p.root, th))
		if !strings.Contains(out, `"state":"asleep"`) {
			t.Fatalf("thread: %s", out)
		}
		ends := len(p.turnsEnded(t, th))
		p.in(th, "What word did I ask you to remember? Reply with only that word.")
		eventually(t, "resumed answer", p.within(3*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > ends && p.replies(th, "marigold") > 0 })
		if n, id := p.agentSession(t); n != 1 || id != session {
			t.Fatalf("%d sessions, %q then %q", n, session, id)
		}
	}) {
		return
	}
	if !step("approval through Answer", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.Command("git", "init", "-q", dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		settings := fmt.Sprintf(`{"command":%q,"approval_policy":"untrusted","sandbox":"workspace-write"}`, wrapper)
		adapter, err := codex.New([]byte(settings))
		if err != nil {
			t.Fatal(err)
		}
		driveCodexApproval(t, adapter, agent.Spec{Dir: dir, Model: "gpt-6-luna", Effort: "low", Approvals: agent.AskInChat}, codexApprovalOptions{
			inputs: []agent.Input{{ID: "first", Text: "Run exactly the shell command `touch approved.txt` in the working directory, wait for it to finish, then reply done."}},
			allow:  true, requests: 1, timeout: p.within(3 * time.Minute),
		})
		if _, err := os.Stat(filepath.Join(dir, "approved.txt")); err != nil {
			t.Fatalf("approved command did not create approved.txt: %v", err)
		}
	}) {
		return
	}
	step("30-minute turn", func(t *testing.T) {
		if os.Getenv("HATAORI_REAL_CODEX_LONG") != "1" {
			t.Skip("set HATAORI_REAL_CODEX_LONG=1 to run")
		}
		ends := len(p.turnsEnded(t, th))
		start := time.Now()
		p.in(th, "Run these four commands sequentially in the foreground, wait for each to finish before starting the next, and wait for all of them before replying: sleep 480; sleep 480; sleep 480; sleep 480. Then reply with just the word: longdone")
		eventually(t, "long turn", p.within(40*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > ends })
		if time.Since(start) < 30*time.Minute || len(p.turnsEnded(t, th)) != ends+1 || p.turnsEnded(t, th)[ends].Data["outcome"] != "completion" || p.replies(th, "longdone") != 1 {
			t.Fatalf("duration %s, end events %d, answers %d", time.Since(start), len(p.turnsEnded(t, th))-ends, p.replies(th, "longdone"))
		}
	})

}

func codexSettings(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".codex")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if os.IsNotExist(err) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

type processID struct {
	pid   int
	start uint64
}

func containsProcess(ids []processID, id processID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// codexProcess reads a process state and start time in clock ticks.
func codexProcess(pid int) (string, uint64) {
	if pid <= 1 {
		return "", 0
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return "", 0
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return "", 0
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", 0
	}
	return fields[0], start
}

func killCodexProcess(proc processID, earliest uint64) {
	state, start := codexProcess(proc.pid)
	if earliest != 0 && state != "" && state != "Z" && start >= earliest && start == proc.start {
		_ = syscall.Kill(proc.pid, syscall.SIGKILL)
	}
}

// codexProcesses lists live processes started since the daemon by parent or cwd.
func codexProcesses(root int, dir string, earliest uint64) []processID {
	entries, _ := os.ReadDir("/proc")
	parents := map[int]int{}
	starts := map[int]uint64{}
	cwds := map[int]string{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		base := filepath.Join("/proc", e.Name())
		stat, err := os.ReadFile(filepath.Join(base, "stat"))
		if err != nil {
			continue
		}
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 20 || fields[0] == "Z" {
			continue
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		start, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil {
			continue
		}
		parents[pid] = parent
		starts[pid] = start
		cwds[pid], _ = os.Readlink(filepath.Join(base, "cwd"))
	}
	var found []processID
	for pid := range parents {
		if earliest == 0 || starts[pid] < earliest {
			continue
		}
		descendant := false
		for p, seen := parents[pid], map[int]bool{}; p > 1 && !seen[p]; p = parents[p] {
			if p == root {
				descendant = true
				break
			}
			seen[p] = true
		}
		rel, err := filepath.Rel(dir, cwds[pid])
		inside := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && cwds[pid] != ""
		if descendant || inside {
			found = append(found, processID{pid, starts[pid]})
		}
	}
	return found
}
