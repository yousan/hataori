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

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
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
	before := codexSettings(t)
	t.Cleanup(func() {
		if after := codexSettings(t); after != before {
			t.Errorf("config.toml changed: %s then %s", before, after)
		}
	})
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["model"] = "gpt-6-luna"
		cfg["effort"] = "low"
		cfg["login_relay"] = false
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": wrapper}}
	})
	p.deadline = time.Now().Add(15 * time.Minute)
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
	// The helper starts at the first tool use and lives as long as the app-server (#317 interrupt).
	p.in(c1, "!go repo:"+bareRepo(t)+" Run the shell command `true`, then reply with just the word: ok")
	th := p.thread(t, 0)
	t.Cleanup(func() {
		if t.Failed() {
			p.dump(t, th)
		}
	})
	eventually(t, "first answer", p.within(3*time.Minute), func() bool {
		return len(p.turnsEnded(t, th)) == 1 && p.replies(th, "ok") > 0 && len(p.list(t, "tool_started", th)) > 0
	})
	_, session := p.agentSession(t)
	if session == "" {
		t.Fatal("missing agent session")
	}
	var workspace string
	if err := p.db(t).QueryRow(`SELECT t.workspace FROM threads t JOIN links l ON l.thread_id = t.id WHERE l.channel = ? AND l.thread = ?`, th.Channel, th.Thread).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	app := p.agentPID(t)
	if _, start := codexProcess(app); start == 0 || start < daemonStart {
		t.Fatal("invalid agent process start time")
	}
	baseline := codexProcesses(app, workspace, daemonStart)
	beforeEnds := len(p.turnsEnded(t, th))
	beforeTools := len(p.list(t, "tool_started", th))
	p.in(th, "Run exactly this one shell command and wait until it has finished before you reply: sleep 600. Do not do anything else.")
	eventually(t, "command start", p.within(3*time.Minute), func() bool { return len(p.list(t, "tool_started", th)) > beforeTools })
	if code, _, stderr := cli(t, "stop", "--root", p.root, numberOf(t, p.root, th)); code != 0 {
		t.Fatalf("stop: exit %d: %s", code, stderr)
	}
	eventually(t, "interrupted turn and command cleanup", p.within(10*time.Second), func() bool {
		ended := p.turnsEnded(t, th)
		if len(ended) <= beforeEnds || ended[beforeEnds].Data["outcome"] != "failure" || ended[beforeEnds].Data["reason"] != "interrupted" {
			return false
		}
		for _, proc := range codexProcesses(app, workspace, daemonStart) {
			if !containsProcess(baseline, proc) {
				return false
			}
		}
		return true
	})
	beforeEnds = len(p.turnsEnded(t, th))
	p.in(th, "Reply with just the word: again")
	eventually(t, "follow-up answer", p.within(3*time.Minute), func() bool { return len(p.turnsEnded(t, th)) > beforeEnds && p.replies(th, "again") > 0 })
	if n, id := p.agentSession(t); n != 1 || id != session {
		t.Fatalf("%d sessions, ID %q then %q", n, session, id)
	}

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
