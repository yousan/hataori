package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// secondAgent is a second name the fake agent is registered under, for an instance with two adapters.
const secondAgent = "fake-second"

// The test binary is also `hataori host`, `hataori hook`, `hataori mcp`,
// `hataori statusline`, the login guard and the fake agent: every command
// that a session's agent or host runs as the hataori binary, which in a
// test is this one. One it does not run here runs the whole test suite.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "host", "hook", "login-guard", "mcp", "statusline", "signin", "threads": // signin: as hataori on the host, run over SSH by hataori connect; threads: run by a test plugin
			os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
		case "--version", "login":
			os.Exit(codexLogin(os.Args[1:]))
		case "app-server":
			os.Exit(codexReplay(os.Args[2:]))
		case "fake":
			os.Exit(fake.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		case "codex-standin":
			parent, err := strconv.Atoi(os.Args[2])
			if err != nil || parent <= 1 {
				os.Exit(2)
			}
			for os.Getppid() == parent {
				time.Sleep(50 * time.Millisecond)
			}
			os.Exit(0)
		case "test-plugin":
			os.Exit(testPlugin())
		case "orphan-terminal":
			os.Exit(orphanTerminal(os.Args[2]))
		}
	}
	if err := agent.Register(fake.Name, fake.New); err != nil {
		panic(err)
	}
	if err := agent.Register(secondAgent, fake.New); err != nil {
		panic(err)
	}
	if err := surface.Register(fakeSurface, fakeDriver); err != nil {
		panic(err)
	}
	if err := surface.Register(secondSurface, fakeDriver); err != nil {
		panic(err)
	}
	if err := surface.Register(remoteSurface, remoteDriver); err != nil {
		panic(err)
	}
	// `hataori run` as a process of its own, which a test can kill.
	if len(os.Args) > 2 && os.Args[1] == "daemon" {
		os.Exit(run([]string{"run", "--root", os.Args[2]}, os.Stdout, os.Stderr))
	}
	// The status line command of TestStatusLineCommandRunsHere: a command
	// not run above would run the whole suite here, sessions and all.
	if os.Getenv(statusLineChildEnv) == "1" {
		fmt.Fprintln(os.Stdout, "the test binary ran its tests instead of the command:", os.Args[1:])
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func bareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work, bare := filepath.Join(dir, "work"), filepath.Join(dir, "repo.git")
	git := func(cwd string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.Mkdir(work, 0o700)
	git(work, "init", "-q")
	os.WriteFile(filepath.Join(work, "README"), []byte("hello\n"), 0o600)
	git(work, "add", "README")
	git(work, "commit", "-q", "-m", "first")
	git(dir, "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

// newRoot makes an instance root whose config uses the fake agent.
func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfg := map[string]any{
		"owner": "owner", "trigger": "!go", "idle": "1h",
		"scopes":   []map[string]any{{"key": "s", "client": "chat", "enabled": true}},
		"adapters": map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")}}},
	}
	b, _ := json.Marshal(cfg)
	os.MkdirAll(filepath.Join(root, "config"), 0o700)
	if err := os.WriteFile(filepath.Join(root, instance.ConfigFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// killSessions kills the hosts and agents of the sessions under root, and
// only those, and removes the socket directories they left outside root.
func killSessions(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	for _, d := range dirs {
		for _, f := range []string{host.PIDFile, host.HostPIDFile} {
			if pid, ok := sessionPID(filepath.Join(d, f)); ok {
				syscall.Kill(-pid, syscall.SIGKILL) // each leads its own group
			}
		}
		if sock := filepath.Dir(host.SocketPath(d)); strings.HasPrefix(filepath.Base(sock), "hataori-host-") {
			os.RemoveAll(sock) // left outside root by a killed host
		}
	}
}

// pidFileSlack is how much later than its pid file a process may seem to
// have started: a file's time is a few milliseconds coarse, and Linux
// gives a start in ticks of 10. A pid given to another process within it
// would still be signalled.
const pidFileSlack = 20 * time.Millisecond

// sessionPID returns the pid that the pid file at path names, and whether
// its group may be killed: only while the pid is still the process the
// file was written for, or no process has it any more. A pid that ended
// early can be given to another process, of another test or program,
// which must not be signalled.
func sessionPID(path string) (int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		// Gone. A new process never gets the id of a process group that
		// still has members, so a group it left behind is still its own.
		return pid, true
	}
	start, ok := processStart(pid)
	if !ok {
		return 0, false // cannot tell: nothing is sent
	}
	// The file is written once the process runs: the host writes its own,
	// and the agent's right after starting it. A process that started
	// after that only has the same number.
	return pid, !start.After(st.ModTime().Add(pidFileSlack))
}

// startDaemon runs `hataori run` in this process until the test ends.
func startDaemon(t *testing.T, root string) { t.Helper(); startDaemonLog(t, root) }

// startDaemonLog is startDaemon, and returns what the daemon has logged
// so far, whenever it is called.
func startDaemonLog(t *testing.T, root string) func() string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var log bytes.Buffer
	var mu sync.Mutex
	logged := func() string { mu.Lock(); defer mu.Unlock(); return log.String() }
	go func() { done <- runCtx(ctx, []string{"run", "--root", root}, io.Discard, lockedWriter{&mu, &log}) }()
	t.Cleanup(func() {
		cancel()
		<-done
		killSessions(root)
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := wire.Dial(root); err == nil {
			c.Close()
			return logged
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("the daemon did not come up: %s", log.String())
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunFailsWithOneLine(t *testing.T) {
	bad := t.TempDir()
	os.MkdirAll(filepath.Join(bad, "config"), 0o700)
	os.WriteFile(filepath.Join(bad, instance.ConfigFile), []byte(`{"owner": 1}`), 0o600)

	locked := newRoot(t)
	in, err := instance.Open(locked)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()

	badDB := newRoot(t)
	os.MkdirAll(filepath.Join(badDB, "state", "hataori.db"), 0o700) // a directory where the database goes

	for name, root := range map[string]string{"config": bad, "lock": locked, "database": badDB} {
		code, out, errOut := cli(t, "run", "--root", root)
		if code == 0 || out != "" || strings.Count(strings.TrimSpace(errOut), "\n") != 0 || errOut == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
}

func TestNoDaemon(t *testing.T) {
	root := newRoot(t)
	for _, args := range [][]string{{"threads"}, {"thread", "1"}, {"events"}, {"stop", "1"}} {
		code, out, errOut := cli(t, append(args, "--root", root)...)
		if code != exitError || out != "" || !strings.Contains(errOut, "no hataori daemon runs for "+root) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func lines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not one JSON object per line: %q", l)
		}
		got = append(got, m)
	}
	return got
}

func TestCommands(t *testing.T) {
	root := newRoot(t)
	startDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "tool wait 30s"}}); err != nil {
		t.Fatal(err)
	}
	waitState := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			_, out, _ := cli(t, "thread", "--root", root, "1")
			if strings.Contains(out, `"state":"`+want+`"`) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("thread never %s: %s", want, out)
			}
		}
	}
	waitState("running")

	code, out, _ := cli(t, "threads", "--root", root)
	ts := lines(t, out)
	if code != 0 || len(ts) != 1 || ts[0]["scope"] != "s" || ts[0]["thread"] != 1.0 || ts[0]["state"] != "running" ||
		!reflect.DeepEqual(ts[0]["links"], []any{map[string]any{"client": "chat", "channel": "s", "thread": "t1"}}) {
		t.Fatalf("threads: exit %d, %v", code, ts)
	}
	if code, out, _ := cli(t, "thread", "--root", root, "99"); code != exitNoThread || out != "" {
		t.Fatalf("thread 99: exit %d, stdout %q", code, out)
	}
	if code, out, _ := cli(t, "thread", "--root", root, "#1"); code != 0 || !strings.Contains(out, `"thread":1,`) {
		t.Fatalf("thread #1: exit %d, stdout %q", code, out)
	}
	if code, out, _ := cli(t, "thread", "--root", root, "t1"); code != exitUsage || out != "" {
		t.Fatalf("thread t1, not a number: exit %d, stdout %q", code, out)
	}

	// events --after n: exactly the events above n, and then it exits.
	// The fake agent may still write events between the two reads, so the
	// later read must start with the earlier one's tail and may hold more.
	_, out, _ = cli(t, "events", "--root", root)
	all := lines(t, out)
	if len(all) < 3 {
		t.Fatalf("events: %v", all)
	}
	_, out, _ = cli(t, "events", "--root", root, "--after", "2")
	got := lines(t, out)
	if len(got) < len(all)-2 {
		t.Fatalf("events --after 2: %v of %v", got, all)
	}
	for i, e := range got {
		if e["pos"] != float64(3+i) || (i < len(all)-2 && !reflect.DeepEqual(e, all[i+2])) {
			t.Fatalf("events --after 2: %v of %v", got, all)
		}
	}

	// --follow goes on with new events.
	pr, pw := io.Pipe()
	fctx, fcancel := context.WithCancel(ctx)
	fdone := make(chan int, 1)
	go func() {
		fdone <- runCtx(fctx, []string{"events", "--root", root, "--after", fmt.Sprint(len(all)), "--follow"}, pw, io.Discard)
		pw.Close()
	}()
	defer func() { fcancel(); pr.Close(); <-fdone }() // a write blocked on the pipe ends too

	code, out, _ = cli(t, "stop", "--root", root, "1")
	if m := lines(t, out); code != 0 || len(m) != 1 || m[0]["running"] != true {
		t.Fatalf("stop: exit %d, %q", code, out)
	}
	sc := bufio.NewScanner(pr)
	seen := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"kind":"turn_ended"`) {
				seen <- sc.Text()
				return
			}
		}
	}()
	select {
	case <-seen:
	case <-time.After(15 * time.Second):
		t.Fatal("events --follow did not print the end of the stopped turn")
	}
	waitState("idle")
	code, out, _ = cli(t, "stop", "--root", root, "1")
	if m := lines(t, out); code != 0 || m[0]["running"] != false {
		t.Fatalf("stop when idle: exit %d, %q", code, out)
	}
}

// Production code outside cmd/ and adapter, driver or client packages must
// depend only on interfaces and must not name particular surfaces or agents.
func TestAdaptersOnlyUnderCmd(t *testing.T) {
	const module = "github.com/yousan/hataori/"
	// The trailing slashes keep the interface packages themselves on the
	// core side: internal/agent is core, internal/agent/fake an adapter.
	// Add future adapter, driver and client package prefixes here.
	packagePrefixes := []string{"internal/agent/", "internal/surface/"}
	isAdapter := func(path string) bool {
		for _, prefix := range packagePrefixes {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	// Keep all surface and agent names in this one list.
	names := []string{"Discord", "Slack", "Microsoft Teams", "Teams", "Claude Code", "Claude", "Codex"}
	patterns := make([]string, len(names))
	for i, name := range names {
		patterns[i] = regexp.QuoteMeta(name)
	}
	words := regexp.MustCompile(`(?i)\b(?:` + strings.Join(patterns, "|") + `)\b`)

	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != root || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if pkg == "cmd" || strings.HasPrefix(pkg, "cmd/") || isAdapter(pkg) {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, rel, source, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(importPath, module) && isAdapter(strings.TrimPrefix(importPath, module)) {
				t.Errorf("%s: imports adapter, driver or client %s", fset.Position(imp.Pos()), importPath)
			}
		}
		if strings.HasPrefix(pkg, "internal/") {
			for i, line := range strings.Split(string(source), "\n") {
				for _, word := range words.FindAllString(line, -1) {
					t.Errorf("%s:%d: core source names surface or agent %q", rel, i+1, word)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// hataori events prints each event exactly as the protocol's golden files
// encode it.
func TestEventsPrintTheDocumentedSchema(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "internal", "wire", "testdata", "protocol", wire.Version, "event-*.json"))
	if len(files) == 0 {
		t.Fatal("no golden events")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var msg struct {
			Event json.RawMessage `json:"event"`
		}
		json.Unmarshal(b, &msg)
		var e control.Event
		if err := json.Unmarshal(msg.Event, &e); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		printEvent(json.NewEncoder(&out), e)
		if got := bytes.TrimSpace(out.Bytes()); !bytes.Equal(got, msg.Event) {
			t.Errorf("%s:\n printed %s\n golden  %s", filepath.Base(f), got, msg.Event)
		}
	}
}

// killSessions signals a pid of a session only while it is the process
// its pid file was written for: one that ended early leaves its pid file,
// and the pid may name another process by the time the test ends.
func TestKillSessionsOnlyTheSessionsProcesses(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state", "sessions", "1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sleeper := func() (*exec.Cmd, <-chan error) {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // leads its group, as a host or agent does
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, done
	}
	// The agent's pid file names it, written after it started.
	agent, agentDone := sleeper()
	if err := os.WriteFile(filepath.Join(dir, host.PIDFile), []byte(strconv.Itoa(agent.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	// The host's pid file is stale: written 50 ms before its pid went to a
	// process the test did not start as the session's.
	other, otherDone := sleeper()
	stale := filepath.Join(dir, host.HostPIDFile)
	if err := os.WriteFile(stale, []byte(strconv.Itoa(other.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	start, ok := processStart(other.Process.Pid)
	if !ok {
		t.Fatal("the other process's start cannot be read")
	}
	if err := os.Chtimes(stale, start.Add(-50*time.Millisecond), start.Add(-50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	killSessions(root)
	select {
	case <-agentDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the session's agent was not killed")
	}
	select {
	case err := <-otherDone:
		t.Fatalf("the process a stale pid file names was signalled: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}
