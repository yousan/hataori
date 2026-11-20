package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// fakeTokens issues t1, t2, ... and remembers which are bound.
type fakeTokens struct {
	mu     sync.Mutex
	n      int
	live   map[string]bool
	issued []string
}

func (f *fakeTokens) Issue(control.API) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	t := fmt.Sprint("t", f.n)
	f.live[t] = true
	f.issued = append(f.issued, t)
	return t, nil
}

func (f *fakeTokens) Restore(t string, _ control.API) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[t] = true
	return nil
}

func (f *fakeTokens) Revoke(t string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, t)
}

func (f *fakeTokens) isLive(t string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[t]
}

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

type told struct {
	mu   sync.Mutex
	keys []string
}

func (t *told) tell(_ context.Context, key, _ string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys = append(t.keys, key)
	return nil
}

func (t *told) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.keys)
}

// script writes a shell script plugin into a new directory and returns its
// path and hash.
func script(t *testing.T, body string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin")
	data := []byte("#!/bin/sh\n" + body + "\n")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

type runnerFixture struct {
	r      *Runner
	tokens *fakeTokens
	log    *syncBuffer
	told   *told
	state  string
	cancel context.CancelFunc
}

// newRunner is a Runner of the grant for the plugin at path, with small
// limits, started. Its context ends, and it is waited for, when the test
// ends.
func newRunner(t *testing.T, g Grant, change func(*Runner)) *runnerFixture {
	t.Helper()
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if g.ID == "" {
		g.ID = "scan"
	}
	if g.Protocol == (Protocol{}) {
		g.Protocol = Protocol{3, 0}
	}
	if g.Settings == nil {
		g.Settings = []byte(`{"words": ["x"]}`)
	}
	f := &runnerFixture{tokens: &fakeTokens{live: map[string]bool{}}, log: &syncBuffer{}, told: &told{}, state: filepath.Join(root, "state")}
	f.r = &Runner{Grants: []Grant{g}, State: f.state, Socket: "/the/socket", Speaks: "3.12",
		Tokens: f.tokens, As: func(control.Caller) control.API { return nil }, Outages: st,
		Log: slog.New(slog.NewTextHandler(f.log, nil)), Tell: f.told.tell,
		Backoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond, Failures: 3, Window: time.Minute,
		Grace:   300 * time.Millisecond,
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/x", "SECRET=s3"} }}
	if change != nil {
		change(f.r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	if err := f.r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); f.r.Wait() })
	return f
}

// file waits for the plugin's state directory to hold name, and returns
// what it holds.
func (f *runnerFixture) file(t *testing.T, name string) string {
	t.Helper()
	var b []byte
	eventually(t, name, 10*time.Second, func() bool {
		var err error
		b, err = os.ReadFile(filepath.Join(f.state, "plugins", "scan", name))
		return err == nil && len(b) > 0 && b[len(b)-1] == '\n'
	})
	return strings.TrimSpace(string(b))
}

// gone reports whether process pid is gone within a few seconds.
func gone(t *testing.T, pid int) bool {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A plugin gets the socket, a token, its id, its state directory (mode
// 0700), its settings in a file and the protocol, and of the daemon's
// environment only the fixed list. It runs in its executable's directory.
func TestPluginEnvironment(t *testing.T) {
	path, sum := script(t, `env > "$HATAORI_PLUGIN_STATE/env.tmp"; pwd >> "$HATAORI_PLUGIN_STATE/env.tmp"
mv "$HATAORI_PLUGIN_STATE/env.tmp" "$HATAORI_PLUGIN_STATE/env"; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	env := strings.Split(f.file(t, "env"), "\n")
	pwd := env[len(env)-1]
	env = env[:len(env)-1]
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	state := filepath.Join(f.state, "plugins", "scan")
	settings := filepath.Join(f.state, "plugin-run", "scan", "settings.json")
	want := map[string]string{EnvSocket: "/the/socket", EnvToken: "t1", EnvID: "scan", EnvState: state,
		EnvSettings: settings, EnvProtocol: "3.12", "HOME": "/home/x"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["SECRET"]; ok {
		t.Error("a variable outside the fixed list reached the plugin")
	}
	for k := range got {
		if !slices.Contains(Env, k) && !strings.HasPrefix(k, "HATAORI_") && k != "PWD" && k != "SHLVL" && k != "_" && k != "OLDPWD" {
			t.Errorf("the plugin got %s, which is not documented", k)
		}
	}
	if real, _ := filepath.EvalSymlinks(filepath.Dir(path)); pwd != filepath.Dir(path) && pwd != real {
		t.Errorf("working directory %q, want %q", pwd, filepath.Dir(path))
	}
	if b, err := os.ReadFile(settings); err != nil || string(b) != `{"words": ["x"]}` {
		t.Errorf("settings: %q, %v", b, err)
	}
	for _, d := range []string{state, filepath.Dir(settings)} {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v", d, st.Mode(), err)
		}
	}
}

// A changed executable is not started, and the log line names both hashes;
// a manifest's protocol of another major version, or of a minor version
// the daemon lacks, is not started, and the line names both versions. Each
// stays stopped, and the owner is told.
func TestNotStarted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol Protocol
		sum      func(string) string
		line     []string
	}{
		{"changed executable", Protocol{3, 0}, func(string) string { return strings.Repeat("ab", 32) },
			[]string{"expected=" + strings.Repeat("ab", 32), "actual="}},
		{"another major version", Protocol{4, 0}, nil, []string{"needs protocol 4.0", "speaks 3.12"}},
		{"a minor version the daemon lacks", Protocol{3, 13}, nil, []string{"needs protocol 3.13", "speaks 3.12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "ran")
			path, sum := script(t, `touch '`+marker+`'`)
			if tc.sum != nil {
				sum = tc.sum(sum)
			}
			f := newRunner(t, Grant{Path: path, SHA256: sum, Protocol: tc.protocol}, nil)
			eventually(t, "the owner told", 5*time.Second, func() bool { return len(f.told.all()) == 1 })
			f.cancel()
			f.r.Wait()
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the plugin ran")
			}
			if len(f.tokens.issued) != 0 {
				t.Fatalf("tokens issued: %v", f.tokens.issued)
			}
			var line string
			for _, l := range strings.Split(f.log.String(), "\n") {
				if strings.Contains(l, "not started") {
					line = l
				}
			}
			for _, w := range tc.line {
				if !strings.Contains(line, w) {
					t.Fatalf("the log line %q does not name %q:\n%s", line, w, f.log.String())
				}
			}
			if tc.sum != nil {
				_, real := script(t, `touch '`+marker+`'`)
				if !strings.Contains(line, "actual="+real) {
					t.Fatalf("the line %q does not name the actual hash %s", line, real)
				}
			}
		})
	}
}

// The executable replaced between its hash and its start is not the one
// that runs: what runs is the bytes that were hashed.
func TestReplacedExecutableDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	original, replaced := filepath.Join(dir, "original"), filepath.Join(dir, "replaced")
	path, sum := script(t, `touch '`+original+`'; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) {
		r.afterRead = func(Grant) {
			// Another file in its place, by rename, as an update would do.
			other := path + ".other"
			os.WriteFile(other, []byte("#!/bin/sh\ntouch '"+replaced+"'; exec sleep 600\n"), 0o700)
			if err := os.Rename(other, path); err != nil {
				panic(err)
			}
		}
	})
	eventually(t, "a start", 10*time.Second, func() bool {
		_, err1 := os.Stat(original)
		_, err2 := os.Stat(replaced)
		return err1 == nil || err2 == nil
	})
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(replaced); err == nil {
		t.Fatal("the file put in the executable's place ran")
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("the hashed executable did not run: %v", err)
	}
	f.cancel()
}

// A plugin killed with SIGKILL is started again with a new token, and the
// old one is revoked. One that keeps failing stays stopped after the set
// number of exits, and the owner is told; its outage is kept, so the next
// runner tells with the same key.
func TestRestartsAndStaysStopped(t *testing.T) {
	path, sum := script(t, `[ -e "$HATAORI_PLUGIN_STATE/spawned" ] || { touch "$HATAORI_PLUGIN_STATE/spawned"; sh -c 'echo $$ > "$HATAORI_PLUGIN_STATE/child.tmp"; mv "$HATAORI_PLUGIN_STATE/child.tmp" "$HATAORI_PLUGIN_STATE/child"; exec sleep 600' & }
echo "$$ $HATAORI_TOKEN" > "$HATAORI_PLUGIN_STATE/run.tmp"; mv "$HATAORI_PLUGIN_STATE/run.tmp" "$HATAORI_PLUGIN_STATE/run"; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	pid, token, _ := strings.Cut(f.file(t, "run"), " ")
	child, _ := strconv.Atoi(f.file(t, "child")) // the first run's child, before the kill
	os.Remove(filepath.Join(f.state, "plugins", "scan", "run"))
	n, _ := strconv.Atoi(pid)
	if err := syscall.Kill(n, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pid2, token2, _ := strings.Cut(f.file(t, "run"), " ")
	if pid2 == pid || token2 == token || token2 == "" {
		t.Fatalf("after SIGKILL: pid %s token %s, before %s %s", pid2, token2, pid, token)
	}
	if child == 0 || !gone(t, child) {
		t.Fatalf("the child %d of the killed plugin is left", child)
	}
	if f.tokens.isLive(token) || !f.tokens.isLive(token2) {
		t.Fatalf("the old token is live %v, the new one %v", f.tokens.isLive(token), f.tokens.isLive(token2))
	}
	if len(f.told.all()) != 0 {
		t.Fatalf("told after one exit: %v", f.told.all())
	}
	f.cancel()
	f.r.Wait()
	if f.tokens.isLive(token2) {
		t.Fatal("the token of a stopped plugin is live")
	}

	// One that exits at once, again and again.
	path, sum = script(t, `echo "$HATAORI_TOKEN" >> "$HATAORI_PLUGIN_STATE/starts"; exit 3`)
	f = newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	eventually(t, "the owner told", 10*time.Second, func() bool { return len(f.told.all()) == 1 })
	time.Sleep(200 * time.Millisecond)
	if starts := strings.Fields(f.file(t, "starts")); len(starts) != 3 {
		t.Fatalf("started %d times, want 3: %v", len(starts), starts)
	}
	if n := strings.Count(f.log.String(), "plugin stays stopped"); n != 1 {
		t.Fatalf("%d lines say it stays stopped: %s", n, f.log.String())
	}
	key := f.told.all()[0]
	f.cancel()
	f.r.Wait()

	// The same outage, after a restart of the daemon: the same key.
	again := &Runner{}
	*again = Runner{Grants: f.r.Grants, State: f.r.State, Socket: f.r.Socket, Speaks: f.r.Speaks, Tokens: f.tokens, As: f.r.As,
		Outages: f.r.Outages, Log: f.r.Log, Tell: f.told.tell, Backoff: f.r.Backoff, Failures: f.r.Failures, Window: f.r.Window}
	ctx, cancel := context.WithCancel(context.Background())
	again.Start(ctx)
	eventually(t, "told again", 10*time.Second, func() bool { return len(f.told.all()) == 2 })
	cancel()
	again.Wait()
	if k := f.told.all(); k[1] != key {
		t.Fatalf("the same outage told with keys %q and %q", k[0], k[1])
	}
}

// A run as long as the window ends the outage, while it runs: a later one
// is told anew.
func TestALongRunEndsTheOutage(t *testing.T) {
	path, sum := script(t, `exit 3`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	eventually(t, "the owner told", 10*time.Second, func() bool { return len(f.told.all()) == 1 })
	f.cancel()
	f.r.Wait()
	var n int
	f.r.Outages.DB.QueryRow(`SELECT count(*) FROM plugin_outages`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d outages", n)
	}
	path, sum = script(t, `exec sleep 600`) // it runs, and runs on
	f2 := &Runner{Grants: []Grant{{ID: "scan", Path: path, SHA256: sum, Protocol: Protocol{3, 0}, Settings: []byte(`{}`)}},
		State: f.state, Speaks: "3.12", Tokens: f.tokens, As: f.r.As, Outages: f.r.Outages, Log: f.r.Log, Tell: f.told.tell,
		Backoff: 10 * time.Millisecond, Failures: 100, Window: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	f2.Start(ctx)
	eventually(t, "the outage ended", 10*time.Second, func() bool {
		f.r.Outages.DB.QueryRow(`SELECT count(*) FROM plugin_outages`).Scan(&n)
		return n == 0
	})
	cancel()
	f2.Wait()
}

// Two external plugins whose token files hold the same token are refused
// with one line naming both keys: either would get the other's grant.
func TestExternalTokensDiffer(t *testing.T) {
	a := Grant{ID: "a", External: true, TokenFile: "a-token", Scopes: []string{"c1"}}
	b := Grant{ID: "b", External: true, TokenFile: "b-token", Scopes: []string{"c2"}}
	r := &Runner{Grants: []Grant{a, b}, Tokens: &fakeTokens{live: map[string]bool{}}, As: func(control.Caller) control.API { return nil },
		Log: slog.New(slog.DiscardHandler), ReadToken: func(string) ([]byte, error) { return []byte("same\n"), nil }}
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"plugins[0].token_file"`) || !strings.Contains(err.Error(), `"plugins[1].token_file"`) {
		t.Fatalf("one token in two files: %v", err)
	}
}

// A plugin that prints its token does not put it in the log, and a line
// with no end is logged, cut, while the plugin runs.
func TestPluginOutputHidesItsTokenAndCutsAtOnce(t *testing.T) {
	path, sum := script(t, `echo "mine is $HATAORI_TOKEN"; printf '%0300d' 0; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) { r.LineMax = 100 })
	eventually(t, "the cut line", 10*time.Second, func() bool { return strings.Contains(f.log.String(), "cut=true") })
	l := f.log.String()
	if !strings.Contains(l, "line=\"mine is [token]\"") || strings.Contains(l, "is t1") {
		t.Fatalf("log: %s", l)
	}
}

// A token the cut goes through is not logged in part either.
func TestPluginOutputHidesACutToken(t *testing.T) {
	// The token is t1001: the cut goes after four of its five bytes, then
	// after two of them.
	path, sum := script(t, `printf '%096d%s\n' 0 "$HATAORI_TOKEN"; printf '%098d%s\n' 0 "$HATAORI_TOKEN"; echo after; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) {
		r.LineMax = 100
		r.Tokens = &fakeTokens{live: map[string]bool{}, n: 1000}
	})
	eventually(t, "the output", 10*time.Second, func() bool { return strings.Contains(f.log.String(), "line=after") })
	l := f.log.String()
	if strings.Count(l, "0[token] cut=true") != 2 || strings.Contains(l, "0t1") {
		t.Fatalf("a cut token is logged in part:\n%s", l)
	}
}

// A stopping notice that does not return in the grace period does not
// keep the plugin from getting SIGTERM, and then SIGKILL.
func TestStoppingThatHangs(t *testing.T) {
	path, sum := script(t, `echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"; exec sleep 600`)
	hang := make(chan struct{})
	defer close(hang)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) {
		r.Stopping = func(context.Context, Grant) { <-hang }
	})
	pid, _ := strconv.Atoi(f.file(t, "pid"))
	done := make(chan struct{})
	go func() { f.cancel(); f.r.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stopping notice that hangs held the stop")
	}
	if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatal("the plugin still runs")
	}
}

// The stopping notice comes while the plugin still runs, before SIGTERM.
func TestStoppingComesFirst(t *testing.T) {
	path, sum := script(t, `echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"; exec sleep 600`)
	var pid int
	var alive bool
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) {
		r.Stopping = func(ctx context.Context, g Grant) {
			_, hasDeadline := ctx.Deadline()
			alive = g.ID == "scan" && hasDeadline && syscall.Kill(pid, 0) == nil
		}
	})
	pid, _ = strconv.Atoi(f.file(t, "pid"))
	f.cancel()
	f.r.Wait()
	if !alive {
		t.Fatal("the stopping notice did not come while the plugin ran, with a deadline")
	}
}

// When the owner cannot be told, it is tried again until it is.
func TestTellIsTriedAgain(t *testing.T) {
	path, sum := script(t, `exit 3`)
	var mu sync.Mutex
	tries := 0
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) {
		r.Tell = func(context.Context, string, string) error {
			mu.Lock()
			defer mu.Unlock()
			tries++
			if tries < 3 {
				return errors.New("the chat service is down")
			}
			return nil
		}
	})
	eventually(t, "three tries", 10*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return tries == 3 })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if tries != 3 {
		t.Fatalf("%d tries after it succeeded", tries)
	}
	if n := strings.Count(f.log.String(), "could not be told"); n != 2 {
		t.Fatalf("%d lines say the owner could not be told", n)
	}
}

// When the runner stops, no process of a plugin is left: not one that
// ignores SIGTERM, nor what it started.
func TestStopLeavesNoProcess(t *testing.T) {
	path, sum := script(t, `trap '' TERM
sh -c 'trap "" TERM; echo $$ > "$HATAORI_PLUGIN_STATE/child.tmp"; mv "$HATAORI_PLUGIN_STATE/child.tmp" "$HATAORI_PLUGIN_STATE/child"; while :; do sleep 1; done' &
echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"
while :; do sleep 1; done`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	var pids []int
	for _, name := range []string{"pid", "child"} {
		n, err := strconv.Atoi(f.file(t, name))
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, n)
	}
	start := time.Now()
	f.cancel()
	f.r.Wait()
	if took := time.Since(start); took < f.r.Grace {
		t.Fatalf("stopped in %v, before the grace of %v: it did not ignore SIGTERM", took, f.r.Grace)
	}
	for _, pid := range pids {
		eventually(t, fmt.Sprint("process ", pid, " gone"), 5*time.Second, func() bool {
			return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		})
	}
	if !strings.Contains(f.log.String(), "did not stop on SIGTERM") {
		t.Fatalf("log: %s", f.log.String())
	}
}

// A plugin's output goes to the log, a line at a time, marked with its id
// and cut at the set size.
func TestPluginOutputIsLogged(t *testing.T) {
	path, sum := script(t, `echo hello; echo oops >&2; printf '%0300d\n' 0; echo after; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) { r.LineMax = 100 })
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the log:\n%s", f.log.String())
		}
	})
	// Each line is waited for: the two streams are read apart, so a line of
	// one may be logged after a later line of the other.
	for _, w := range []string{"plugin=scan stream=stdout line=hello", "plugin=scan stream=stderr line=oops",
		"plugin=scan stream=stdout line=" + strings.Repeat("0", 100) + " cut=true", "plugin=scan stream=stdout line=after"} {
		eventually(t, w, 10*time.Second, func() bool { return strings.Contains(f.log.String(), w) })
	}
	if l := f.log.String(); strings.Contains(l, strings.Repeat("0", 101)) {
		t.Errorf("a line longer than the set size was logged whole:\n%s", l)
	}
}

// An external plugin's token, from its file, is bound to its caller; hataori
// starts nothing for it. A token file that cannot be read stops the start
// with a line naming the key.
func TestExternalToken(t *testing.T) {
	g := Grant{ID: "ext", External: true, TokenFile: "plugins/ext-token", Scopes: []string{"c1"}}
	var asked []control.Caller
	f := newRunner(t, g, func(r *Runner) {
		r.ReadToken = func(file string) ([]byte, error) {
			if file != "plugins/ext-token" {
				return nil, os.ErrNotExist
			}
			return []byte(" secret-token \n"), nil
		}
		r.As = func(c control.Caller) control.API { asked = append(asked, c); return nil }
	})
	if !f.tokens.isLive("secret-token") || len(f.tokens.issued) != 0 {
		t.Fatalf("tokens: %+v", f.tokens)
	}
	if len(asked) != 1 || asked[0].From != "plugin:ext" || asked[0].Limit == nil {
		t.Fatalf("bound to %+v", asked)
	}
	if strings.Contains(f.log.String(), "secret-token") {
		t.Fatal("the log has the token")
	}

	r := &Runner{Grants: []Grant{g}, Tokens: f.tokens, As: f.r.As, Log: f.r.Log,
		ReadToken: func(string) ([]byte, error) { return nil, os.ErrNotExist }}
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"plugins[0].token_file"`) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("an unreadable token file: %v", err)
	}
}

// PluginOf names a running plugin for its own process and one it started
// in its group, and nothing for another process or once it has stopped.
func TestPluginOf(t *testing.T) {
	path, sum := script(t, `sh -c 'echo $$ > "$HATAORI_PLUGIN_STATE/child.tmp"; mv "$HATAORI_PLUGIN_STATE/child.tmp" "$HATAORI_PLUGIN_STATE/child"; exec sleep 600' &
echo $$ > "$HATAORI_PLUGIN_STATE/pid.tmp"; mv "$HATAORI_PLUGIN_STATE/pid.tmp" "$HATAORI_PLUGIN_STATE/pid"; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	pid, _ := strconv.Atoi(f.file(t, "pid"))
	child, _ := strconv.Atoi(f.file(t, "child"))
	if got := f.r.PluginOf(pid); got != "scan" {
		t.Errorf("the plugin's own process: %q", got)
	}
	if got := f.r.PluginOf(child); got != "scan" {
		t.Errorf("a process it started: %q", got)
	}
	if got := f.r.PluginOf(os.Getpid()); got != "" {
		t.Errorf("another process: %q", got)
	}
	f.cancel()
	f.r.Wait()
	if got := f.r.PluginOf(pid); got != "" {
		t.Errorf("after the stop: %q", got)
	}
	// Forgotten, not only gone: a group id the system gives again later is
	// no plugin's.
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	if len(f.r.groups) != 0 {
		t.Errorf("groups kept after the stop: %v", f.r.groups)
	}
}

// A plugin that connects while it is being started, before its group is
// recorded, is not taken for the owner: PluginOf waits for the start.
func TestPluginOfWhileStarting(t *testing.T) {
	path, sum := script(t, `exec sleep 600`)
	got := make(chan string, 1)
	newRunner(t, Grant{Path: path, SHA256: sum}, func(r *Runner) {
		r.afterStart = func(pid int) {
			go func() { got <- r.PluginOf(pid) }()
			time.Sleep(100 * time.Millisecond) // the plugin connects now
		}
	})
	select {
	case id := <-got:
		if id != "scan" {
			t.Fatalf("a plugin connecting while it starts: %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PluginOf did not answer")
	}
}

// group starts a process group as a plugin's start would leave it: a
// leader and a child, each running until killed. env is put in their
// environment and arg among the leader's arguments, each when not empty.
// It returns the leader's and the child's pids, and a channel closed when
// the leader is gone. The test ends both.
func group(t *testing.T, env, arg string) (int, int, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-c", `sh -c 'echo $$ > "$1/child.tmp"; mv "$1/child.tmp" "$1/child"; exec sleep 600' sh "$0"; while :; do sleep 1; done`, dir}
	if arg != "" {
		args = append(args, arg)
	}
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if env != "" {
		cmd.Env = append(cmd.Env, env)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	go func() { cmd.Wait(); close(gone) }()
	var child int
	eventually(t, "the child", 10*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "child"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && child > 0
	})
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-gone })
	return cmd.Process.Pid, child, gone
}

// What an earlier run left is killed before the plugin starts again, and
// only that: processes of another run, of none, with the run's value in an
// argument instead of the environment, and of a run recorded in another
// boot, are not signalled.
func TestLeftovers(t *testing.T) {
	const run = "0123456789abcdef0123456789abcdef"
	boot, err := bootID()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		env, arg  string
		boot      string // the record's
		signalled bool
	}{
		{"the earlier run", EnvRun + "=" + run, "", boot, true},
		{"another run", EnvRun + "=fedcba9876543210fedcba9876543210", "", boot, false},
		{"no run", "", "", boot, false},
		{"the run's value in an argument", "", EnvRun + "=" + run, boot, false},
		{"another boot", EnvRun + "=" + run, "", "an earlier boot", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, child, ended := group(t, tc.env, tc.arg)
			r := &Runner{State: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
			g := Grant{ID: "scan"}
			os.MkdirAll(filepath.Dir(r.recordPath(g, run)), 0o700)
			b, _ := json.Marshal(runRecord{Run: run, Boot: tc.boot})
			if err := os.WriteFile(r.recordPath(g, run), b, 0o600); err != nil {
				t.Fatal(err)
			}
			r.leftovers(context.Background(), g)
			if tc.signalled {
				select {
				case <-ended:
				case <-time.After(5 * time.Second):
					t.Fatal("the earlier run's leader is left")
				}
				if !gone(t, child) {
					t.Fatal("the earlier run's child is left")
				}
			} else {
				time.Sleep(200 * time.Millisecond)
				select {
				case <-ended:
					t.Fatal("a process of no earlier run was signalled")
				default:
				}
				if !running(child) {
					t.Fatal("a child of no earlier run was signalled")
				}
			}
			if _, err := os.Stat(r.recordPath(g, run)); err == nil {
				t.Fatal("the record is kept")
			}
		})
	}
}

// A run is recorded before anything of it runs: the plugin, as it starts,
// finds its own record.
func TestRunIsRecordedBeforeItStarts(t *testing.T) {
	path, sum := script(t, `if [ -e "$HATAORI_PLUGIN_STATE/../../plugin-run/$HATAORI_PLUGIN_ID/run-$HATAORI_PLUGIN_RUN.json" ]; then echo yes; else echo no; fi > "$HATAORI_PLUGIN_STATE/seen.tmp"
mv "$HATAORI_PLUGIN_STATE/seen.tmp" "$HATAORI_PLUGIN_STATE/seen"; exec sleep 600`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	if got := f.file(t, "seen"); got != "yes" {
		t.Fatalf("the plugin, as it started, found its record: %s", got)
	}
}

// When a run ends, what it started that left its group, and kept its
// environment, goes too.
func TestRunEndsWithWhatLeftItsGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to leave a process group with")
	}
	path, sum := script(t, `[ -e "$HATAORI_PLUGIN_STATE/away" ] && exec sleep 600
python3 -c 'import os, time; os.setpgid(0, 0); open(os.environ["HATAORI_PLUGIN_STATE"] + "/away.tmp", "w").write("%d\n" % os.getpid()); os.rename(os.environ["HATAORI_PLUGIN_STATE"] + "/away.tmp", os.environ["HATAORI_PLUGIN_STATE"] + "/away"); time.sleep(600)' &
while [ ! -e "$HATAORI_PLUGIN_STATE/away" ]; do sleep 0.05; done; exit 3`)
	f := newRunner(t, Grant{Path: path, SHA256: sum}, nil)
	away, _ := strconv.Atoi(f.file(t, "away"))
	t.Cleanup(func() { syscall.Kill(away, syscall.SIGKILL) })
	if pg, err := syscall.Getpgid(away); err == nil && pg != away {
		t.Fatalf("the process did not leave the plugin's group: %d", pg)
	}
	if !gone(t, away) {
		t.Fatal("what the plugin started outside its group is left after it exited")
	}
}

// A record whose processes could not be looked for is kept, and done at
// the next start.
func TestLeftoversTriedAgain(t *testing.T) {
	const run = "0123456789abcdef0123456789abcdef"
	boot, _ := bootID()
	_, _, ended := group(t, EnvRun+"="+run, "")
	r := &Runner{State: t.TempDir(), Log: slog.New(slog.DiscardHandler),
		processes: func() ([]int, error) { return nil, errors.New("cannot list") }}
	g := Grant{ID: "scan"}
	os.MkdirAll(filepath.Dir(r.recordPath(g, run)), 0o700)
	b, _ := json.Marshal(runRecord{Run: run, Boot: boot})
	os.WriteFile(r.recordPath(g, run), b, 0o600)
	r.leftovers(context.Background(), g)
	if _, err := os.Stat(r.recordPath(g, run)); err != nil {
		t.Fatal("a record that could not be done is dropped")
	}
	r.processes = nil
	r.leftovers(context.Background(), g)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the earlier run is left after the second try")
	}
	if _, err := os.Stat(r.recordPath(g, run)); err == nil {
		t.Fatal("the record is kept once done")
	}
}

// What a plugin that is no longer in the config left is killed too, and
// so is what one that is external now left of when hataori started it.
func TestLeftoversOfAPluginNoLongerConfigured(t *testing.T) {
	t.Run("removed", func(t *testing.T) { leftoversOf(t, nil) })
	t.Run("external now", func(t *testing.T) {
		leftoversOf(t, []Grant{{ID: "removed", External: true, TokenFile: "t", Scopes: []string{"c1"}}})
	})
}

func leftoversOf(t *testing.T, grants []Grant) {
	const run = "0123456789abcdef0123456789abcdef"
	boot, _ := bootID()
	_, _, ended := group(t, EnvRun+"="+run, "")
	r := &Runner{State: t.TempDir(), Log: slog.New(slog.DiscardHandler), Grants: grants,
		Tokens: &fakeTokens{live: map[string]bool{}}, As: func(control.Caller) control.API { return nil },
		ReadToken: func(string) ([]byte, error) { return []byte("tok"), nil }}
	old := Grant{ID: "removed"}
	os.MkdirAll(filepath.Dir(r.recordPath(old, run)), 0o700)
	b, _ := json.Marshal(runRecord{Run: run, Boot: boot})
	os.WriteFile(r.recordPath(old, run), b, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("what a plugin no longer configured left is left")
	}
	cancel()
	r.Wait()
}

// A look that the daemon's stop ends keeps its record.
func TestLeftoversStopWithTheDaemon(t *testing.T) {
	const run = "0123456789abcdef0123456789abcdef"
	boot, _ := bootID()
	r := &Runner{State: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	g := Grant{ID: "scan"}
	os.MkdirAll(filepath.Dir(r.recordPath(g, run)), 0o700)
	b, _ := json.Marshal(runRecord{Run: run, Boot: boot})
	os.WriteFile(r.recordPath(g, run), b, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.leftovers(ctx, g)
	if _, err := os.Stat(r.recordPath(g, run)); err != nil {
		t.Fatal("a look that did not happen dropped its record")
	}
}

// A process of the run that the first listing did not show, as one forked
// meanwhile, is found by the next pass.
func TestLeftoversLookAgain(t *testing.T) {
	const run = "0123456789abcdef0123456789abcdef"
	boot, _ := bootID()
	leader, child, ended := group(t, EnvRun+"="+run, "")
	calls := 0
	r := &Runner{State: t.TempDir(), Log: slog.New(slog.DiscardHandler), processes: func() ([]int, error) {
		calls++
		if calls == 1 {
			return []int{leader}, nil // the child came after this listing
		}
		return []int{leader, child}, nil
	}}
	g := Grant{ID: "scan"}
	os.MkdirAll(filepath.Dir(r.recordPath(g, run)), 0o700)
	b, _ := json.Marshal(runRecord{Run: run, Boot: boot})
	os.WriteFile(r.recordPath(g, run), b, 0o600)
	r.leftovers(context.Background(), g)
	<-ended
	if !gone(t, child) {
		t.Fatal("a process the first listing did not show is left")
	}
}
