package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
)

// daemon is a daemon process a test can kill: `role daemon ROOT TERMINAL
// start|resume [SCRIPT]`.
func daemon(args []string) int {
	root, terminal, mode := args[0], args[1] == "true", args[2]
	log, _ := os.OpenFile(filepath.Join(root, "daemon.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	st, c, r, err := build(root, options{terminal: terminal}, log)
	if err != nil {
		fmt.Fprintln(log, err)
		return 1
	}
	defer st.Close()
	if err := r.Reconnect(context.Background()); err != nil {
		fmt.Fprintln(log, err)
		return 1
	}
	if mode == "start" {
		if _, err := owner(c).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
			Repo: "file:///unused", Input: control.Input{Text: args[3]}}); err != nil {
			fmt.Fprintln(log, err)
			return 1
		}
	}
	os.Stdout.WriteString("up\n")
	select {}
}

// proc is a daemon process the test started.
type proc struct{ cmd *exec.Cmd }

func runDaemon(t *testing.T, root string, terminal bool, mode string, script ...string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"role", "daemon", root, fmt.Sprint(terminal), mode}, script...)...)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd}
	t.Cleanup(p.kill)
	buf := make([]byte, 3)
	if n, _ := out.Read(buf); string(buf[:n]) != "up\n" {
		p.kill()
		b, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
		t.Fatalf("daemon did not come up: %s", b)
	}
	return p
}

// kill ends the daemon with SIGKILL; only this process, which the test
// started. The session hosts it started live on, as they would.
func (p *proc) kill() {
	if p.cmd.ProcessState == nil {
		p.cmd.Process.Signal(syscall.SIGKILL)
		p.cmd.Wait()
	}
}

func openStore(t *testing.T, root string) *store.Store {
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func until(t *testing.T, what string, st *store.Store, kind string, n int) {
	t.Helper()
	waitFor(t, what, 20*time.Second, func() bool { return counts(t, st)[kind] >= n })
}

// The turn of this script, and every event it must leave exactly once.
const killScript = "tool step 400ms\nreply first\nreply second"

var once = map[string]int{"turn_started": 1, "tool_started": 1, "tool_ended": 1, "text": 2, "turn_ended": 1, "input": 1}

func TestDaemonKilledDuringATurn(t *testing.T) {
	const points = 20
	forms(t, func(t *testing.T, terminal bool) {
		for i := range points {
			t.Run(fmt.Sprint("point ", i), func(t *testing.T) {
				root := t.TempDir()
				t.Cleanup(func() { killSessions(root) })
				d := runDaemon(t, root, terminal, "start", killScript)
				st := openStore(t, root)
				until(t, "the turn's start", st, "turn_started", 1)
				// A point spread over the turn (about 400ms of tool, then the replies).
				time.Sleep(time.Duration(i) * 500 * time.Millisecond / points)
				d.kill()
				d = runDaemon(t, root, terminal, "resume")
				until(t, "the turn's end", st, "turn_ended", 1)
				time.Sleep(150 * time.Millisecond) // anything late would show now
				d.kill()
				got := counts(t, st)
				for kind, n := range once {
					if got[kind] != n {
						t.Errorf("%d %s events, want %d (all: %v)", got[kind], kind, n, got)
					}
				}
			})
		}
	})
}

func TestRestartKeepsTheRunningAgent(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		root := t.TempDir()
		t.Cleanup(func() { killSessions(root) })
		d := runDaemon(t, root, terminal, "start", "tool wait 1s\nreply done")
		st := openStore(t, root)
		until(t, "the tool", st, "tool_started", 1)
		pidFile := filepath.Join(StateDir(root, 1), host.PIDFile)
		before := strings.TrimSpace(string(readFile(t, pidFile)))
		d.kill()
		d = runDaemon(t, root, terminal, "resume")
		until(t, "the turn's end", st, "turn_ended", 1)
		after := strings.TrimSpace(string(readFile(t, pidFile)))
		pid, _ := strconv.Atoi(after)
		if before != after || syscall.Kill(pid, 0) != nil {
			t.Fatalf("agent process %s before, %s after (alive: %v)", before, after, syscall.Kill(pid, 0) == nil)
		}
		d.kill()
	})
}

// A question asked before the daemon was killed is answered after.
func TestWaitingCallAnsweredAfterRestart(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		root := t.TempDir()
		t.Cleanup(func() { killSessions(root) })
		d := runDaemon(t, root, terminal, "start", "ask which colour?")
		st := openStore(t, root)
		until(t, "the question", st, "request", 1)
		d.kill()

		// The new daemon is this test process: it reconnects, and the
		// owner answers through its core.
		log := &syncBuffer{}
		st2, c, r, err := build(root, options{terminal: terminal}, log)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { r.Close(); st2.Close() }()
		if err := r.Reconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		var req string
		st.DB.QueryRow(`SELECT json_extract(data, '$.request') FROM events WHERE kind = 'request'`).Scan(&req)
		waitFor(t, "the reconnect", 10*time.Second, func() bool {
			_, err := r.get(1)
			return err == nil
		})
		if err := owner(c).Answer(context.Background(), control.RequestID(req), control.Answer{Text: "blue"}); err != nil {
			t.Fatalf("answer: %v\n%s", err, log.String())
		}
		until(t, "the turn's end", st, "turn_ended", 2) // blocked, then the answered turn
		var final string
		st.DB.QueryRow(`SELECT json_extract(data, '$.text') FROM events WHERE kind = 'text' ORDER BY pos DESC LIMIT 1`).Scan(&final)
		if final != "answer: blue" {
			t.Fatalf("final answer %q", final)
		}
	})
}
