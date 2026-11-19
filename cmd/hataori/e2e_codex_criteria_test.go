package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"syscall"

	"encoding/json"
	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/codex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// question_after_turn contains two complete turns on the same captured thread.
func TestCodexFollowUpContinuesTheSameThread(t *testing.T) {
	temp := t.TempDir()
	home := filepath.Join(temp, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, codexLoggedFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
	if err != nil {
		t.Fatal(err)
	}
	wire := filepath.Join(temp, "wire")
	t.Setenv("HATAORI_TEST_CODEX_CAPTURE", capture)
	t.Setenv("HATAORI_TEST_CODEX_RECORD", filepath.Join(temp, "env"))
	t.Setenv("HATAORI_TEST_CODEX_SCENARIO", "question_after_turn")
	t.Setenv("HATAORI_TEST_CODEX_WIRE", wire)
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": os.Args[0], "pass_env": []string{"HATAORI_TEST_CODEX_CAPTURE", "HATAORI_TEST_CODEX_RECORD", "HATAORI_TEST_CODEX_SCENARIO", "HATAORI_TEST_CODEX_WIRE"}}}
	})
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" hello")
	th := p.thread(t, 0)
	p.posted(t, th, "I haven’t received your answer yet. Which colour do you like?")
	eventually(t, "first turn end", 15*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	_, before := p.agentSession(t)
	p.in(th, "blue")
	p.posted(t, th, "Blue.")
	eventually(t, "second turn end", 15*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 2 })
	if n, after := p.agentSession(t); n != 1 || before == "" || after != before {
		t.Fatalf("sessions %d, IDs %q and %q", n, before, after)
	}
	if n := p.count(th, "Blue."); n != 1 {
		t.Fatalf("follow-up reply posted %d times", n)
	}
	starts := codexMethods(t, wire, "turn/start")
	if len(starts) != 2 || starts[0]["params"].(map[string]any)["threadId"] != before || starts[1]["params"].(map[string]any)["threadId"] != before {
		t.Fatalf("turn starts: %v", starts)
	}
}

func codexMethods(t *testing.T, path, method string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if m["method"] == method {
			found = append(found, m)
		}
	}
	return found
}

func TestCodexIdlePeriodSleepsAndTheNextPostResumes(t *testing.T) {
	temp := t.TempDir()
	home := filepath.Join(temp, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, codexLoggedFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
	if err != nil {
		t.Fatal(err)
	}
	wire := filepath.Join(temp, "wire")
	t.Setenv("HATAORI_TEST_CODEX_CAPTURE", capture)
	t.Setenv("HATAORI_TEST_CODEX_RECORD", filepath.Join(temp, "env"))
	t.Setenv("HATAORI_TEST_CODEX_SCENARIO", "start,resume")
	t.Setenv("HATAORI_TEST_CODEX_LAUNCHES", filepath.Join(temp, "launches"))
	t.Setenv("HATAORI_TEST_CODEX_WIRE", wire)
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "1s"
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": os.Args[0], "pass_env": []string{"HATAORI_TEST_CODEX_CAPTURE", "HATAORI_TEST_CODEX_RECORD", "HATAORI_TEST_CODEX_SCENARIO", "HATAORI_TEST_CODEX_LAUNCHES", "HATAORI_TEST_CODEX_WIRE"}}}
	})
	p.start(t)
	p.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" hello")
	th := p.thread(t, 0)
	p.posted(t, th, "I’ll run the requested shell command, then answer the arithmetic question.")
	eventually(t, "first turn end", 15*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	pid := p.agentPID(t)
	_, before := p.agentSession(t)
	eventually(t, "app-server to sleep", 15*time.Second, func() bool { return !alive(pid) })
	_, out, _ := cli(t, "thread", "--root", p.root, numberOf(t, p.root, th))
	if !strings.Contains(out, `"state":"asleep"`) {
		t.Fatalf("thread: %s", out)
	}
	p.in(th, "what was the arithmetic?")
	p.posted(t, th, "2+3")
	if n, after := p.agentSession(t); n != 1 || before == "" || after != before {
		t.Fatalf("sessions %d, IDs %q and %q", n, before, after)
	}
	if starts := codexMethods(t, wire, "thread/start"); len(starts) != 1 {
		t.Fatalf("thread starts: %v", starts)
	}
	resumes := codexMethods(t, wire, "thread/resume")
	if len(resumes) != 1 || resumes[0]["params"].(map[string]any)["threadId"] != before {
		t.Fatalf("resume: %v, session %q", resumes, before)
	}
}

// codexCriteriaRoot runs the captured app-server in a separate daemon session.
func codexCriteriaRoot(t *testing.T, scenario string, extra map[string]string) (*procRoot, string) {
	t.Helper()
	temp := t.TempDir()
	home := filepath.Join(temp, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, codexLoggedFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
	if err != nil {
		t.Fatal(err)
	}
	wire := filepath.Join(temp, "wire")
	env := map[string]string{"HATAORI_TEST_CODEX_CAPTURE": capture, "HATAORI_TEST_CODEX_RECORD": filepath.Join(temp, "env"), "HATAORI_TEST_CODEX_SCENARIO": scenario, "HATAORI_TEST_CODEX_WIRE": wire}
	for k, v := range extra {
		env[k] = v
	}
	var names []string
	for k, v := range env {
		t.Setenv(k, v)
		names = append(names, k)
	}
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": os.Args[0], "pass_env": names}}
	})
	return p, wire
}

func TestCodexInterruptLeavesNoCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "command-pid")
	p, wire := codexCriteriaRoot(t, "interrupt", map[string]string{"HATAORI_TEST_CODEX_COMMAND_PID": pidFile})
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" run a command")
	th := p.thread(t, 0)
	var pid int
	eventually(t, "command started", 15*time.Second, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, _ = strconv.Atoi(string(b))
		return pid > 1 && alive(pid) && len(p.events(t, "tool_started", th)) > 0
	})
	if code, _, out := cli(t, "stop", "--root", p.root, numberOf(t, p.root, th)); code != 0 {
		t.Fatalf("stop: %s", out)
	}
	eventually(t, "interrupted turn and command cleanup", 10*time.Second, func() bool {
		ends := p.events(t, "turn_ended", th)
		return len(ends) == 1 && ends[0]["outcome"] == "failure" && ends[0]["reason"] == "interrupted" && !alive(pid)
	})
	methods := codexMethods(t, wire, "turn/interrupt")
	clean := codexMethods(t, wire, "thread/backgroundTerminals/clean")
	if len(methods) != 1 || len(clean) != 1 {
		t.Fatalf("interrupt %v, clean %v", methods, clean)
	}
	p.in(th, "continue")
	p.posted(t, th, "ok")
	eventually(t, "follow-up end", 15*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 2 })
}

func TestCodexDaemonKilledDuringATurnFinishesIt(t *testing.T) {
	// The captured command starts over three seconds before turn completion.
	// Four times the captured timing leaves a window of at least ten seconds.
	p, wire := codexCriteriaRoot(t, "usage", map[string]string{"HATAORI_TEST_CODEX_PACE": "4"})
	p.start(t)
	prompt := "run a captured command"
	commentary := "I’ll run `echo start-ok`, then answer the arithmetic question."
	answer := "5"
	p.in(c1, "!go repo:"+bareRepo(t)+" "+prompt)
	th := p.thread(t, 0)
	p.posted(t, th, prompt)
	eventually(t, "commentary and tool started", 50*time.Second, func() bool {
		return p.count(th, commentary) > 0 && len(p.events(t, "tool_started", th)) > 0
	})
	if p.count(th, commentary) != 1 || len(p.events(t, "tool_started", th)) != 1 || len(p.events(t, "turn_ended", th)) != 0 {
		t.Fatal("expected one commentary and tool start, and no turn end, before the kill")
	}
	p.kill(t)
	p.start(t)
	eventually(t, "answer after restart", 40*time.Second, func() bool { return p.count(th, answer) > 0 })
	eventually(t, "turn ended", 40*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) > 0 })
	time.Sleep(300 * time.Millisecond)
	if a, b, c := p.count(th, prompt), p.count(th, commentary), p.count(th, answer); a != 1 || b != 1 || c != 1 {
		t.Fatalf("prompt %d, commentary %d, answer %d", a, b, c)
	}
	if n := len(p.events(t, "tool_started", th)); n != 1 {
		t.Fatalf("%d tool start events", n)
	}
	if n := len(p.events(t, "turn_ended", th)); n != 1 {
		t.Fatalf("%d end events", n)
	}
	for _, method := range []string{"initialize", "thread/start", "turn/start"} {
		if messages := codexMethods(t, wire, method); len(messages) != 1 {
			t.Errorf("%s sent %d times: %v", method, len(messages), messages)
		}
	}
}

// No 30-minute capture exists; the duration criterion needs the live check.
func TestCodexApprovalAnsweredThroughAnswerInChat(t *testing.T) {
	for _, scenario := range []string{"approval_accept", "approval_decline"} {
		t.Run(scenario, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("CODEX_HOME", temp)
			if err := os.WriteFile(filepath.Join(temp, codexLoggedFile), nil, 0600); err != nil {
				t.Fatal(err)
			}
			capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"HATAORI_TEST_CODEX_CAPTURE": capture, "HATAORI_TEST_CODEX_RECORD": filepath.Join(temp, "env"), "HATAORI_TEST_CODEX_SCENARIO": scenario}
			var names []string
			for k, v := range env {
				t.Setenv(k, v)
				names = append(names, k)
			}
			settings, _ := json.Marshal(map[string]any{"command": os.Args[0], "pass_env": names, "approval_policy": "untrusted"})
			adapter, err := codex.New(settings)
			if err != nil {
				t.Fatal(err)
			}
			spec := agent.Spec{Dir: temp, Approvals: agent.AskInChat}
			driveCodexApproval(t, adapter, spec, codexApprovalOptions{
				inputs: []agent.Input{{ID: "first", Text: "Run touch approved.txt"}, {ID: "second", Text: "create patch.txt"}},
				allow:  scenario == "approval_accept", requests: 2, exact: true, timeout: 30 * time.Second,
			})
		})
	}
}

type codexApprovalOptions struct {
	inputs   []agent.Input
	allow    bool
	requests int
	exact    bool
	timeout  time.Duration
}

// driveCodexApproval runs the adapter's actual launch and exchanges codec bytes
// over process pipes. Closing stdin lets the replay report response mismatches.
func driveCodexApproval(t *testing.T, adapter agent.Adapter, spec agent.Spec, opts codexApprovalOptions) {
	t.Helper()
	launch, err := adapter.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launch.Argv[0], launch.Argv[1:]...)
	cmd.Dir, cmd.Env = spec.Dir, launch.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	_, started := codexProcess(cmd.Process.Pid)
	defer func() {
		// Linux can identify descendants that Codex placed outside its group.
		var children []processID
		if started != 0 {
			children = codexProcesses(cmd.Process.Pid, spec.Dir, started)
		}
		if !done {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
		for _, child := range children {
			killCodexProcess(child, started)
		}
	}()
	lines := make(chan []byte, 1)
	stopReading := make(chan struct{})
	defer close(stopReading)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), 4<<20)
		for scan.Scan() {
			select {
			case lines <- append([]byte(nil), scan.Bytes()...):
			case <-stopReading:
				return
			}
		}
	}()
	codec := adapter.NewCodec(spec)
	write := func(a agent.Actions) {
		t.Helper()
		for _, b := range a.Write {
			if _, err := stdin.Write(b); err != nil {
				t.Fatalf("write app-server: %v: %s", err, stderr.String())
			}
		}
	}
	write(codec.Open())
	a, err := codec.Input(opts.inputs[0])
	if err != nil {
		t.Fatal(err)
	}
	write(a)
	requests, turns := 0, 0
	timer := time.NewTimer(opts.timeout)
	defer timer.Stop()
	for turns < len(opts.inputs) {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("app-server exited before completion: %s", stderr.String())
			}
			a := codec.Decode(agent.Output, line)
			write(a)
			for _, e := range a.Events {
				if e.Kind == agent.Request {
					if e.Request == nil || !e.Request.Approval {
						t.Fatalf("unexpected request: %+v", e.Request)
					}
					requests++
					reply, err := codec.Answer(agent.Answer{Request: e.Request.ID, Allow: opts.allow})
					if err != nil {
						t.Fatal(err)
					}
					write(reply)
				}
				if e.Kind == agent.TurnEnded {
					if e.Status != agent.Completed {
						t.Fatalf("turn status %v", e.Status)
					}
					turns++
					if turns < len(opts.inputs) {
						next, err := codec.Input(opts.inputs[turns])
						if err != nil {
							t.Fatal(err)
						}
						write(next)
					}
				}
			}
		case <-timer.C:
			t.Fatal("timed out waiting for approval turn")
		}
	}
	if (opts.exact && requests != opts.requests) || (!opts.exact && requests < opts.requests) {
		if opts.exact {
			t.Fatalf("got %d approval requests, want %d", requests, opts.requests)
		}
		t.Fatalf("got %d approval requests, want at least %d", requests, opts.requests)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	// Drain stdout so the child can exit even if it has more notifications.
	exitTimer := time.NewTimer(10 * time.Second)
	defer exitTimer.Stop()
	draining := true
	for draining {
		select {
		case _, ok := <-lines:
			if !ok {
				draining = false
			}
		case <-exitTimer.C:
			t.Fatal("app-server did not exit after stdin closed")
		}
	}
	err = cmd.Wait()
	done = true
	if err != nil {
		t.Fatalf("app-server: %v: %s", err, stderr.String())
	}
}
