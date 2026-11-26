package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/window"
)

// windowEnv is the environment of a hook in a tmux pane of an interactive
// Claude Code session.
func windowEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "TMUX=/tmp/tmux-1000/default,4242,0", "TMUX_PANE=%7",
		"CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CONFIG_DIR=/home/alice/.claude-work"}, extra...)
}

// hookResult is how one run of `hataori hook --window` went.
type hookResult struct {
	code           int
	stdout, stderr []byte
	took           time.Duration
}

// runWindowHook runs `hataori hook --window` as Claude Code does: under a
// shell of its own that has no terminal, below a process that has one. Here
// that process is a shell on a pseudo-terminal of the test's own.
func runWindowHook(t *testing.T, env []string, payload []byte, args ...string) hookResult {
	t.Helper()
	dir := t.TempDir()
	in, out, errf, code := filepath.Join(dir, "in"), filepath.Join(dir, "out"), filepath.Join(dir, "err"), filepath.Join(dir, "code")
	if err := os.WriteFile(in, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `"$0" role windowhook "$@" <"$HOOK_IN" >"$HOOK_OUT" 2>"$HOOK_ERR"; echo $? >"$HOOK_CODE"`
	cmd := exec.Command("sh", append([]string{"-c", script, os.Args[0]}, args...)...)
	cmd.Env = append(env, "HOOK_IN="+in, "HOOK_OUT="+out, "HOOK_ERR="+errf, "HOOK_CODE="+code)
	start := time.Now()
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, tty)
	err = cmd.Wait()
	took := time.Since(start)
	tty.Close()
	if err != nil {
		t.Fatalf("the shell around the hook: %v", err)
	}
	var r hookResult
	r.took = took
	b, _ := os.ReadFile(code)
	if r.code, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
		t.Fatalf("no exit status: %q", b)
	}
	r.stdout, _ = os.ReadFile(out)
	r.stderr, _ = os.ReadFile(errf)
	return r
}

// capturedPayload reads a hook payload Claude Code sent, from the captures.
func capturedPayload(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawListener listens where the daemon of root takes reports, accepts and
// never answers. It returns how many connections it accepted and what they
// sent, once the test is done with it.
func rawListener(t *testing.T, root string) (accepted *atomic.Int32, got func() []byte) {
	t.Helper()
	sock, err := window.SocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	accepted = &atomic.Int32{}
	var mu sync.Mutex
	var all bytes.Buffer
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			held = append(held, c) // never answered, never closed by this side
			mu.Unlock()
			go func() {
				b, _ := io.ReadAll(c)
				mu.Lock()
				all.Write(b)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
		os.Remove(filepath.Dir(sock)) // only when it is empty: a short path under /tmp
	})
	return accepted, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Clone(all.Bytes())
	}
}

// Every hook of the plugin exits 0 within a second and prints nothing,
// the waiting ones included: while the daemon takes reports, while no daemon
// runs, and while something accepts the connection and never answers.
func TestWindowHookNeverHolds(t *testing.T) {
	start := capturedPayload(t, "2.1.328/paste_tabs/hooks/01-SessionStart.json")
	run := func(t *testing.T, root string) (exhausted bool) {
		t.Helper()
		for _, args := range [][]string{
			{"--window", "--root", root},
			{"--window", "--root", root, "--wait", "--timeout", "86400s"}, // PermissionRequest, AskUserQuestion
		} {
			r := runWindowHook(t, windowEnv(), start, args...)
			if r.code != 0 || len(r.stdout) != 0 || len(r.stderr) != 0 {
				t.Fatalf("hook %q: exit %d after %v, printed %q and %q", args, r.code, r.took, r.stdout, r.stderr)
			}
			if r.took >= time.Second {
				t.Skipf("hook took %v including process startup; cannot check a one-second bound on this host", r.took)
			}
			exhausted = exhausted || r.took >= windowHookBudget
		}
		return exhausted
	}

	t.Run("daemon", func(t *testing.T) {
		root := t.TempDir()
		s, err := window.Listen(root)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		exhausted := run(t, root)
		// The report arrived, from the agent's terminal: the shell on the
		// pseudo-terminal, not the hook's own process.
		w := "tmux:/tmp/tmux-1000/default,4242,%7"
		deadline := time.Now().Add(5 * time.Second)
		for {
			r, ok := s.Last(w)
			if ok && r.Session == "00000000-0000-4000-8000-000000000001" && r.Interactive && r.Event == "SessionStart" &&
				r.Model == "claude-haiku-5-5" && r.ConfigDir == "/home/alice/.claude-work" && strings.HasPrefix(r.Device, "/dev/") && r.PID > 1 {
				break
			}
			if time.Now().After(deadline) {
				if !ok && exhausted {
					t.Skip("hook used its reporting budget before a report arrived; connection could not be checked")
				}
				t.Fatalf("no report for %s: %+v %v", w, r, ok)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("no daemon", func(t *testing.T) {
		root := t.TempDir()
		s, err := window.Listen(root) // its directory stays; the socket goes
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		run(t, root)
		run(t, t.TempDir()) // no directory either
	})
	t.Run("never answers", func(t *testing.T) {
		root := t.TempDir()
		accepted, _ := rawListener(t, root)
		exhausted := run(t, root)
		if accepted.Load() == 0 {
			if exhausted {
				t.Skip("hook used its reporting budget before connecting; an unanswered connection was not tried")
			}
			t.Fatal("the hook never connected: the case was not tried")
		}
	})
}

// In a session that hataori runs, the plugin's hook does nothing: it exits
// 0, prints nothing and connects to nothing.
func TestWindowHookInHataorisSession(t *testing.T) {
	root := t.TempDir()
	accepted, _ := rawListener(t, root)
	short, err := os.MkdirTemp("/tmp", "hwh") // a socket path short enough on macOS too
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	own := filepath.Join(short, "host.sock")
	ln, err := net.Listen("unix", own)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var ownAccepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ownAccepted.Add(1)
			c.Close()
		}
	}()
	r := runWindowHook(t, windowEnv(host.HookEnvVar+"="+own), capturedPayload(t, "2.1.328/paste_tabs/hooks/01-SessionStart.json"),
		"--window", "--root", root, "--wait", "--timeout", "86400s")
	if r.code != 0 || len(r.stdout) != 0 || len(r.stderr) != 0 {
		t.Fatalf("exit %d, printed %q and %q", r.code, r.stdout, r.stderr)
	}
	time.Sleep(200 * time.Millisecond)
	if n, m := accepted.Load(), ownAccepted.Load(); n != 0 || m != 0 {
		t.Fatalf("the hook connected: %d times to the daemon, %d to the session host", n, m)
	}
}

// The report holds nothing of a payload's prompt, the last message, a
// tool's input or its result.
func TestWindowHookSendsNoText(t *testing.T) {
	const marker = "MARKER-7f3a-do-not-send"
	captures := []string{
		"2.1.327/confine_files_settings_flag/hooks/02-UserPromptSubmit.json",
		"2.1.327/confine_files_settings_flag/hooks/03-Stop.json",
		"2.1.327/usage/hooks/05-PostToolUse.json",
	}
	for _, c := range captures {
		var p map[string]any
		if err := json.Unmarshal(capturedPayload(t, c), &p); err != nil {
			t.Fatal(err)
		}
		// The marker in every field that holds the session's text, whether
		// the event has it or not.
		p["prompt"] = "please print " + marker
		p["last_assistant_message"] = "Done: " + marker
		p["tool_input"] = map[string]any{"command": "echo " + marker, "description": marker}
		p["tool_response"] = map[string]any{"stdout": marker + "\n", "stderr": "", "interrupted": false}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		_, got := rawListener(t, root)
		env := map[string]string{"TMUX": "/tmp/tmux-1000/default,4242,0", "TMUX_PANE": "%7", "CLAUDE_CODE_ENTRYPOINT": "cli"}
		h := windowHook{getenv: func(k string) string { return env[k] }, ppid: 4243,
			terminal: func(context.Context, int) (int, string, bool, error) { return 4242, "/dev/pts/9", true, nil },
			send:     window.Send}
		if code := h.main([]string{"--window", "--root", root}, bytes.NewReader(b), io.Discard); code != 0 {
			t.Fatalf("%s: exit %d", c, code)
		}
		var sent []byte
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if sent = got(); bytes.HasSuffix(sent, []byte("\n")) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: nothing sent", c)
			}
		}
		if bytes.Contains(sent, []byte(marker)) {
			t.Fatalf("%s: the report holds the session's text: %s", c, sent)
		}
		var r window.Report
		if err := json.Unmarshal(sent, &r); err != nil || r.Session != p["session_id"] || r.Event != p["hook_event_name"] || r.Dir != p["cwd"] {
			t.Fatalf("%s: report %s, %v", c, sent, err)
		}
	}
}

// A hook outside tmux names no window and sends nothing; without --window
// it is a usage error.
func TestWindowHookOutsideTmux(t *testing.T) {
	sent := 0
	h := windowHook{getenv: func(k string) string { return "" }, ppid: 4243,
		terminal: func(context.Context, int) (int, string, bool, error) { return 0, "", false, errors.New("not asked") },
		send:     func(context.Context, string, window.Report) error { sent++; return nil }}
	if code := h.main([]string{"--window", "--root", t.TempDir()}, strings.NewReader(`{"session_id":"s","hook_event_name":"SessionStart"}`), io.Discard); code != 0 || sent != 0 {
		t.Fatalf("exit %d, %d sent", code, sent)
	}
	if code := h.main([]string{"--root", "x"}, strings.NewReader(""), io.Discard); code != host.HookUsage {
		t.Fatalf("without --window: exit %d", code)
	}
}

// Check the budget without subprocess startup or terminal discovery. The
// integration test above may skip when those consume the reporting budget.
func TestWindowHookBudget(t *testing.T) {
	for _, stage := range []string{"terminal", "send"} {
		t.Run(stage, func(t *testing.T) {
			wait := func(ctx context.Context) {
				t.Helper()
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > windowHookBudget {
					t.Fatal("hook did not bound its work by the reporting budget")
				}
				<-ctx.Done()
				if ctx.Err() != context.DeadlineExceeded {
					t.Fatalf("budget ended: %v", ctx.Err())
				}
			}
			var reached bool
			env := map[string]string{"TMUX": "/tmp/tmux-1000/default,4242,0", "TMUX_PANE": "%7"}
			h := windowHook{getenv: func(k string) string { return env[k] }, ppid: 4243,
				terminal: func(ctx context.Context, _ int) (int, string, bool, error) {
					if stage == "terminal" {
						reached = true
						wait(ctx)
						return 0, "", false, ctx.Err()
					}
					return 4242, "/dev/pts/9", true, nil
				},
				send: func(ctx context.Context, _ string, _ window.Report) error {
					if stage != "send" {
						t.Fatal("hook sent after terminal discovery used its budget")
					}
					reached = true
					wait(ctx)
					return ctx.Err()
				},
			}
			started := time.Now()
			code := h.main([]string{"--window", "--root", t.TempDir()},
				bytes.NewReader(capturedPayload(t, "2.1.328/paste_tabs/hooks/01-SessionStart.json")), io.Discard)
			if code == 0 && !reached && time.Since(started) >= windowHookBudget {
				t.Skip("reporting budget expired before the injected stage was reached")
			}
			if code != 0 || !reached {
				t.Fatalf("exit %d, reached %s: %v", code, stage, reached)
			}
		})
	}
}
