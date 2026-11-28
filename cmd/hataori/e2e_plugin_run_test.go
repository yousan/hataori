package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// pluginRun is what the test plugin writes to its state directory each
// time it runs: who it was and what Threads answered.
type pluginRun struct {
	PID     int                  `json:"pid"`
	Token   string               `json:"token"`
	Env     []string             `json:"env"`
	Threads []control.ThreadInfo `json:"threads"`
	Error   string               `json:"error,omitempty"`
}

// testPlugin is the test plugin: as a program in another language would,
// it reads the documented environment, speaks the protocol on the socket
// with its token, calls Threads, writes what it got to run.json in its
// state directory and stays connected until it is stopped.
func testPlugin() int {
	run := pluginRun{PID: os.Getpid(), Token: os.Getenv(plugin.EnvToken), Env: os.Environ()}
	write := func() {
		b, _ := json.Marshal(run)
		state := os.Getenv(plugin.EnvState)
		os.WriteFile(filepath.Join(state, "run.tmp"), b, 0o600)
		os.Rename(filepath.Join(state, "run.tmp"), filepath.Join(state, "run.json"))
	}
	conn, err := net.Dial("unix", os.Getenv(plugin.EnvSocket))
	if err != nil {
		run.Error = err.Error()
		write()
		return 1
	}
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	var hello struct{ Version, Error string }
	enc.Encode(map[string]string{"version": os.Getenv(plugin.EnvProtocol), "token": run.Token})
	if err := dec.Decode(&hello); err != nil || hello.Error != "" {
		run.Error = fmt.Sprint("hello: ", hello.Error, err)
		write()
		return 1
	}
	// Requests the daemon sends come on this connection too:
	// plugin_started and plugin_stopping are answered with no result, any
	// other with not_supported, as docs/protocol.md says.
	type message struct {
		ID     uint64
		Op     string
		Result json.RawMessage
		Error  *struct{ Code, Message string }
	}
	answer := func(m message) {
		if f, err := os.OpenFile(filepath.Join(os.Getenv(plugin.EnvState), "ops"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			fmt.Fprintln(f, m.Op)
			f.Close()
		}
		switch m.Op {
		case "plugin_started", "plugin_stopping":
			enc.Encode(map[string]any{"id": m.ID, "result": nil})
		default: // it handles no hook point
			enc.Encode(map[string]any{"id": m.ID, "error": map[string]string{"code": "not_supported", "message": m.Op}})
		}
	}
	enc.Encode(map[string]any{"id": 1, "op": "Threads", "args": map[string]any{"filter": map[string]any{}}})
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			run.Error = fmt.Sprint("Threads: ", err)
			break
		}
		if m.Op != "" {
			answer(m)
			continue
		}
		if m.ID == 1 {
			if m.Error != nil {
				run.Error = fmt.Sprint("Threads: ", m.Error)
			} else {
				json.Unmarshal(m.Result, &run.Threads)
			}
			break
		}
	}
	write()
	for { // until the daemon closes the connection
		var m message
		if dec.Decode(&m) != nil {
			break
		}
		if m.Op != "" {
			answer(m)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}

// writePlugin writes a plugin into a new directory: a shell script that
// runs body, with a manifest asking for Threads, and returns its path and
// SHA-256.
func writePlugin(t *testing.T, id, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	manifest := `{"id": "` + id + `", "version": "1", "protocol": "` + wire.Version + `", "operations": ["Threads", "Events"], "events": ["turn_ended"]}`
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id)
	data := []byte("#!/bin/sh\n" + body + "\n")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

// testPluginScript is the body of a plugin script that runs testPlugin.
func testPluginScript(t *testing.T) string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return "exec '" + exe + "' test-plugin"
}

// readRun waits for the plugin's run.json, other than one with the token
// before, and returns it.
func readRun(t *testing.T, root, id, before string) pluginRun {
	t.Helper()
	var run pluginRun
	eventually(t, "the plugin's run", 20*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(root, "state", "plugins", id, "run.json"))
		return err == nil && json.Unmarshal(b, &run) == nil && run.Token != before
	})
	return run
}

// pluginDaemon runs the daemon of root until stop is called, and returns what it
// has logged.
func pluginDaemon(t *testing.T, root string) (logged func() string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var mu sync.Mutex
	var log strings.Builder
	go func() { done <- runCtx(ctx, []string{"run", "--root", root}, io.Discard, lockedWriter{&mu, &log}) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done; killSessions(root) }) }
	t.Cleanup(stop)
	logged = func() string { mu.Lock(); defer mu.Unlock(); return log.String() }
	eventually(t, "the daemon", 10*time.Second, func() bool {
		c, err := wire.Dial(root)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return logged, stop
}

func pluginEntry(id, path, sum string) map[string]any {
	return map[string]any{"id": id, "path": path, "sha256": sum, "scopes": []string{"c1"}}
}

// A plugin is started with the documented environment, connects with its
// token and calls Threads, which answers what its grant shows. Killed with
// SIGKILL, it is started again with a new token, and the old token is
// refused on the socket. It is sent plugin_started when it connects, and
// plugin_stopping when the daemon stops.
func TestPluginStartsAndStartsAgain(t *testing.T) {
	path, sum := writePlugin(t, "scan", testPluginScript(t))
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["plugins"] = []map[string]any{pluginEntry("scan", path, sum)}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "posters": []string{e2eHook}},
			{"key": "c2", "client": fakeSurface, "enabled": true, "posters": []string{e2eHook}},
		}
	})
	runnerHook = func(pr *plugin.Runner) { pr.Backoff = 10 * time.Millisecond }
	t.Cleanup(func() { runnerHook = nil })
	logged, stop := pluginDaemon(t, r.root)
	ctx := context.Background()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	repo := bareRepo(t)
	a, err := owner.Start(ctx, control.StartRequest{Key: "k-a", Scope: "c1", Thread: "ta", Repo: repo, Input: control.Input{Text: "reply a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Start(ctx, control.StartRequest{Key: "k-b", Scope: "c2", Thread: "tb", Repo: repo, Input: control.Input{Text: "reply b"}}); err != nil {
		t.Fatal(err)
	}

	// The plugin started before the threads did: it is killed once, so
	// that its next run sees them.
	run := readRun(t, r.root, "scan", "")
	if run.Error != "" {
		t.Fatalf("the plugin's first run: %s", run.Error)
	}
	env := map[string]string{}
	for _, kv := range run.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	socket, _ := wire.SocketPath(r.root)
	for k, want := range map[string]string{
		plugin.EnvSocket: socket, plugin.EnvID: "scan", plugin.EnvProtocol: wire.Version,
		plugin.EnvState:    filepath.Join(r.root, "state", "plugins", "scan"),
		plugin.EnvSettings: filepath.Join(r.root, "state", "plugin-run", "scan", "settings.json"),
	} {
		if abs, _ := filepath.Abs(want); env[k] != want && env[k] != abs {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if env[plugin.EnvToken] == "" {
		t.Error("no token")
	}
	if err := syscall.Kill(run.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	again := readRun(t, r.root, "scan", run.Token)
	if again.Error != "" || again.PID == run.PID {
		t.Fatalf("after SIGKILL: %+v", again)
	}
	if len(again.Threads) != 1 || again.Threads[0].ID != a {
		t.Fatalf("Threads answered %+v, want only thread %v of c1", again.Threads, a)
	}
	if _, err := wire.DialToken(r.root, run.Token); err == nil || !strings.Contains(err.Error(), "unknown token") {
		t.Fatalf("the old token: %v, want refused", err)
	}
	if strings.Contains(logged(), run.Token) || strings.Contains(logged(), again.Token) {
		t.Fatal("the log has a token")
	}

	// It was told it started, each time, and it is told it is stopping
	// before the daemon stops it.
	owner.Close()
	stop()
	b, _ := os.ReadFile(filepath.Join(r.root, "state", "plugins", "scan", "ops"))
	if ops := strings.Join(strings.Fields(string(b)), " "); ops != "plugin_started plugin_started plugin_stopping" {
		t.Fatalf("the plugin was sent %v", ops)
	}
}

// warningDuringShutdown models a successful service post returning just
// as the daemon stops. The fake has recorded the message before the test
// observes it; the outbox has not yet recorded the successful reply.
type warningDuringShutdown struct {
	*fakesurface.Driver
	once sync.Once
}

func (d *warningDuringShutdown) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	ref, err := d.Driver.Post(ctx, conv, key, m)
	if err == nil && strings.Contains(m.Text, "plugin flaky is stopped") {
		d.once.Do(func() { <-ctx.Done() })
	}
	return ref, err
}

// A plugin that keeps failing stays stopped after the set number of exits,
// and the owner is told once in the direct conversation of the login
// alert: not again when the daemon restarts within the same outage.
func TestPluginThatKeepsFailingIsToldOnce(t *testing.T) {
	for _, duringPost := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown_during_post=%v", duringPost), func(t *testing.T) {
			path, sum := writePlugin(t, "flaky", `echo started >> "$HATAORI_PLUGIN_STATE/starts"; exit 3`)
			r := newChatRoot(t, func(cfg map[string]any) { cfg["plugins"] = []map[string]any{pluginEntry("flaky", path, sum)} })
			runnerHook = func(pr *plugin.Runner) { pr.Backoff, pr.Failures = 10*time.Millisecond, 3 }
			t.Cleanup(func() { runnerHook = nil })
			if duringPost {
				drivers.Store(r.root, &warningDuringShutdown{Driver: r.driver})
			}
			dm, _ := r.driver.OpenDirect(nil, e2eOwner)
			toldIn := func() int {
				n := 0
				for _, c := range r.driver.Calls(dm) {
					if strings.Contains(c.Message.Text, "plugin flaky is stopped") {
						n++
					}
				}
				return n
			}
			starts := func() int {
				b, _ := os.ReadFile(filepath.Join(r.root, "state", "plugins", "flaky", "starts"))
				return strings.Count(string(b), "started")
			}
			logged, stop := pluginDaemon(t, r.root)
			r.connected(t)
			eventually(t, "the owner told", 20*time.Second, func() bool { return toldIn() == 1 })
			if n := starts(); n != 3 {
				t.Fatalf("started %d times, want 3", n)
			}
			if !strings.Contains(logged(), "plugin stays stopped") {
				t.Fatal("no log line")
			}
			stop()

			logged, stop = pluginDaemon(t, r.root)
			r.connected(t)
			eventually(t, "the plugin stopped again", 20*time.Second, func() bool { return strings.Contains(logged(), "plugin stays stopped") })
			time.Sleep(500 * time.Millisecond) // a second message would be on its way
			if n := toldIn(); n != 1 {
				t.Fatalf("the owner was told %d times for one outage; calls: %+v", n, r.driver.Calls(dm))
			}
			if n := starts(); n != 6 {
				t.Fatalf("started %d times over two runs of the daemon, want 6", n)
			}
			stop()
		})
	}
}

// A daemon stop leaves no process of any plugin, also of one that ignores
// SIGTERM, nor what it started.
func TestDaemonStopLeavesNoPluginProcess(t *testing.T) {
	stubborn, sum1 := writePlugin(t, "stubborn", `trap '' TERM
sh -c 'trap "" TERM; echo $$ > "$HATAORI_PLUGIN_STATE/child.tmp"; mv "$HATAORI_PLUGIN_STATE/child.tmp" "$HATAORI_PLUGIN_STATE/child"; while :; do sleep 1; done' &
echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"
while :; do sleep 1; done`)
	polite, sum2 := writePlugin(t, "polite", `echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"; exec sleep 600`)
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["plugins"] = []map[string]any{pluginEntry("stubborn", stubborn, sum1), pluginEntry("polite", polite, sum2)}
	})
	runnerHook = func(pr *plugin.Runner) { pr.Grace = 300 * time.Millisecond }
	t.Cleanup(func() { runnerHook = nil })
	_, stop := pluginDaemon(t, r.root)
	var pids []int
	for _, f := range []string{"stubborn/pid", "stubborn/child", "polite/pid"} {
		var pid int
		eventually(t, f, 20*time.Second, func() bool {
			b, err := os.ReadFile(filepath.Join(r.root, "state", "plugins", f))
			return err == nil && strings.HasSuffix(string(b), "\n") && json.Unmarshal(b[:len(b)-1], &pid) == nil
		})
		pids = append(pids, pid)
	}
	stop()
	for _, pid := range pids {
		eventually(t, fmt.Sprint("process ", pid, " gone"), 5*time.Second, func() bool {
			return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		})
	}
}

// An external plugin connects with the token from its file and gets its
// grant; a wrong token is refused and logged without the token.
func TestExternalPluginConnects(t *testing.T) {
	path, _ := writePlugin(t, "ext", "exit 1")
	const token = "ext-token-Zr7q2"
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["plugins"] = []map[string]any{{"id": "ext", "path": path, "external": true, "token_file": "plugins/ext-token", "scopes": []string{"c1"}}}
	})
	os.MkdirAll(filepath.Join(r.root, "config", "plugins"), 0o700)
	if err := os.WriteFile(filepath.Join(r.root, "config", "plugins", "ext-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logged, _ := pluginDaemon(t, r.root)
	p, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	if _, err := p.Threads(ctx, control.Filter{}); err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if _, err := p.Threads(ctx, control.Filter{Scope: "c2"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Threads outside its scopes: %v", err)
	}
	if _, err := p.Limits(ctx); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Limits, not granted: %v", err)
	}
	const wrong = "wrong-token-Qx81"
	if _, err := wire.DialToken(r.root, wrong); err == nil || !strings.Contains(err.Error(), "unknown token") {
		t.Fatalf("a wrong token: %v", err)
	}
	eventually(t, "the refusal logged", 5*time.Second, func() bool { return strings.Contains(logged(), "unknown token") })
	if l := logged(); strings.Contains(l, wrong) || strings.Contains(l, token) {
		t.Fatalf("the log has a token: %s", l)
	}
	if _, err := os.Stat(filepath.Join(r.root, "state", "plugins", "ext")); err == nil {
		t.Fatal("hataori started the external plugin")
	}
}

// A hataori command that a plugin starts, and so the plugin itself when it
// connects without its token, is refused instead of getting the owner's
// connection, and the log line names the plugin. The owner's own commands
// work as before while the plugin runs.
func TestPluginWithoutItsTokenIsNotTheOwner(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The instance's root is three levels above the plugin's state directory.
	path, sum := writePlugin(t, "forgetful", `'`+exe+`' threads --root "$HATAORI_PLUGIN_STATE/../../.." > "$HATAORI_PLUGIN_STATE/out" 2>&1
echo $? > "$HATAORI_PLUGIN_STATE/code.tmp"; mv "$HATAORI_PLUGIN_STATE/code.tmp" "$HATAORI_PLUGIN_STATE/code"
exec sleep 600`)
	r := newChatRoot(t, func(cfg map[string]any) { cfg["plugins"] = []map[string]any{pluginEntry("forgetful", path, sum)} })
	logged, _ := pluginDaemon(t, r.root)
	state := filepath.Join(r.root, "state", "plugins", "forgetful")
	var code []byte
	eventually(t, "the plugin's command to end", 20*time.Second, func() bool {
		code, err = os.ReadFile(filepath.Join(state, "code"))
		return err == nil
	})
	out, _ := os.ReadFile(filepath.Join(state, "out"))
	if strings.TrimSpace(string(code)) == "0" || !strings.Contains(string(out), "a plugin connects with its token") {
		t.Fatalf("hataori threads from a plugin: exit %s, %s", code, out)
	}
	eventually(t, "the refusal logged", 5*time.Second, func() bool {
		for _, l := range strings.Split(logged(), "\n") {
			if strings.Contains(l, "control connection refused") && strings.Contains(l, "plugin=forgetful") {
				return true
			}
		}
		return false
	})

	// The owner, while the plugin runs.
	if code, out, errOut := cli(t, "threads", "--root", r.root); code != 0 {
		t.Fatalf("the owner's hataori threads: %d %s %s", code, out, errOut)
	}
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatalf("the owner's connection: %v", err)
	}
	defer owner.Close()
	if _, err := owner.Threads(context.Background(), control.Filter{}); err != nil {
		t.Fatal(err)
	}
}

// After the daemon is killed with SIGKILL while a plugin and a child it
// started run, the next start of the daemon leaves no process of that
// earlier run, and starts the plugin again.
func TestPluginLeftoversOfADaemonThatDied(t *testing.T) {
	path, sum := writePlugin(t, "keeper", `sh -c 'echo $$ > "$HATAORI_PLUGIN_STATE/child.tmp"; mv "$HATAORI_PLUGIN_STATE/child.tmp" "$HATAORI_PLUGIN_STATE/child"; exec sleep 600' &
echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"; exec sleep 600`)
	root := newRoot(t)
	cfgPath := filepath.Join(root, instance.ConfigFile)
	b, _ := os.ReadFile(cfgPath)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["plugins"] = []map[string]any{{"id": "keeper", "path": path, "sha256": sum, "scopes": []string{"s"}}}
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state", "plugins", "keeper")
	read := func(name string) int {
		var pid int
		eventually(t, name, 20*time.Second, func() bool {
			b, err := os.ReadFile(filepath.Join(state, name))
			return err == nil && strings.HasSuffix(string(b), "\n") && json.Unmarshal(b[:len(b)-1], &pid) == nil
		})
		os.Remove(filepath.Join(state, name))
		return pid
	}

	// A daemon of its own process, killed with SIGKILL.
	cmd := exec.Command(os.Args[0], "daemon", root)
	var log strings.Builder
	var mu sync.Mutex
	cmd.Stdout, cmd.Stderr = lockedWriter{&mu, &log}, lockedWriter{&mu, &log}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	leader, child := read("pid"), read("child")
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	earlier := []int{leader, child}
	t.Cleanup(func() {
		for _, pid := range earlier {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	time.Sleep(300 * time.Millisecond)
	if !alive(child) || zombie(child) {
		t.Fatalf("the earlier run's child %d did not outlive the daemon: the test shows nothing", child)
	}

	logged, _ := pluginDaemon(t, root)
	again := read("pid")
	for _, pid := range earlier {
		eventually(t, fmt.Sprint("process ", pid, " of the earlier run gone"), 10*time.Second, func() bool {
			return !alive(pid) || zombie(pid)
		})
	}
	if again == leader || !alive(again) {
		t.Fatalf("the plugin was not started again: %d, before %d", again, leader)
	}
	if !strings.Contains(logged(), "killed what an earlier run left") {
		t.Fatalf("no log line: %s", logged())
	}
}
