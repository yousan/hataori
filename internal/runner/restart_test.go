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
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
)

// daemon is a daemon process a test can kill: `role daemon ROOT TERMINAL
// start|resume|die [ARG...]`. A mode ending in "+idle" puts idle sessions
// to sleep after a short idle period. die (see dieBeforeHandOver) makes one
// call and kills itself when the core hands it to the runner.
func daemon(args []string) int {
	root, terminal, mode := args[0], args[1] == "true", args[2]
	o := options{terminal: terminal}
	for {
		if m, ok := strings.CutSuffix(mode, "+idle"); ok {
			mode, o.idlePeriod = m, 300*time.Millisecond
		} else if m, ok := strings.CutSuffix(mode, "+slow"); ok { // a new agent takes each input after 1.5s
			mode, o.instructions = m, "input-delay 1500ms"
		} else if m, ok := strings.CutSuffix(mode, "+quick"); ok { // written input goes unreported for 1s at most
			mode, o.inputTimeout = m, "1s"
		} else if m, ok := strings.CutSuffix(mode, "+dieplaced"); ok { // killed right after a workspace is in place
			mode, o.dieWhenPlaced = m, true
		} else if m, ok := strings.CutSuffix(mode, "+dialog"); ok { // the fake shows wakeDialog at each start
			mode, o.dialog = m, wakeDialog
		} else if m, ok := strings.CutSuffix(mode, "+forget"); ok { // the fake keeps its conversations in ROOT/conversations
			mode, o.conversations = m, filepath.Join(root, "conversations")
			os.MkdirAll(o.conversations, 0o700)
		} else {
			break
		}
	}
	log, _ := os.OpenFile(filepath.Join(root, "daemon.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	st, c, r, err := build(root, o, log)
	if err != nil {
		fmt.Fprintln(log, err)
		return 1
	}
	defer st.Close()
	if err := r.Reconnect(context.Background()); err != nil {
		fmt.Fprintln(log, err)
		return 1
	}
	if err := c.Recover(context.Background()); err != nil {
		fmt.Fprintln(log, err)
		return 1
	}
	if mode == "die-send" { // send to t1, and kill itself at a point of handing it over
		point := args[3]
		r.point = func(p string) {
			if p == point {
				syscall.Kill(os.Getpid(), syscall.SIGKILL) // this process only
				select {}
			}
		}
		os.Stdout.WriteString("up\n")
		if err := owner(c).Send(context.Background(), control.ThreadID(1), "d1",
			control.Input{Text: args[4]}, control.Deliver); err != nil {
			fmt.Fprintln(log, err)
			return 1
		}
		select {} // the write may come when the running turn ends
	}
	if mode == "die-at" { // kill itself at a point of the launch of a new thread's session
		point := args[3]
		r.point = func(p string) {
			if p == point {
				syscall.Kill(os.Getpid(), syscall.SIGKILL) // this process only
				select {}
			}
		}
		os.Stdout.WriteString("up\n")
		owner(c).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
			Repo: "file:///unused", Input: control.Input{Text: args[4]}})
		return 1 // it should have died
	}
	if mode == "die-wake" { // start t1, let it sleep, and kill itself at a point of the launch that wakes it
		point := args[3]
		os.Stdout.WriteString("up\n")
		if _, err := owner(c).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
			Repo: "file:///unused", Input: control.Input{Text: args[4]}}); err != nil {
			fmt.Fprintln(log, err)
			return 1
		}
		for state := ""; state != "asleep"; time.Sleep(20 * time.Millisecond) {
			st.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&state)
		}
		for !r.alive(StateDir(root, 1)).Ended() {
			time.Sleep(20 * time.Millisecond)
		}
		r.point = func(p string) {
			if p == point {
				syscall.Kill(os.Getpid(), syscall.SIGKILL) // this process only
				select {}
			}
		}
		owner(c).Send(context.Background(), control.ThreadID(1), "m2", control.Input{Text: args[5]}, control.Deliver)
		return 1 // it should have died
	}
	if mode == "die-replace" { // start t1, let it sleep, forget its conversation, and kill itself at a point of replacing it
		point := args[3]
		os.Stdout.WriteString("up\n")
		if _, err := owner(c).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
			Repo: "file:///unused", Input: control.Input{Text: args[4]}}); err != nil {
			fmt.Fprintln(log, err)
			return 1
		}
		for state := ""; state != "asleep"; time.Sleep(20 * time.Millisecond) {
			st.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&state)
		}
		for !r.alive(StateDir(root, 1)).Ended() {
			time.Sleep(20 * time.Millisecond)
		}
		files, _ := filepath.Glob(filepath.Join(o.conversations, "*"))
		for _, p := range files {
			os.Remove(p) // the agent's own retention removed it: it cannot resume
		}
		r.point = func(p string) {
			if p == point {
				syscall.Kill(os.Getpid(), syscall.SIGKILL) // this process only
				select {}
			}
		}
		// A plugin adds text around the input; it comes through the thread's link.
		c.Hooks = aroundHooks{}
		owner(c).Send(context.Background(), control.ThreadID(1), "m2", control.Input{Text: args[5],
			Link: &control.LinkRef{Client: "chat", Channel: "s", Thread: "t1"}}, control.Deliver)
		select {} // the replace comes when the woken agent says it cannot resume
	}
	if mode == "die" {
		c.Runner = dying{r}
		os.Stdout.WriteString("up\n")
		if err := dieBeforeHandOver(st, c, args[3], args[4]); err != nil {
			fmt.Fprintln(log, err)
		}
		return 1 // it should have died
	}
	if mode == "start" { // one thread per script: t1, t2, ...
		for i, script := range args[3:] {
			n := fmt.Sprint(i + 1)
			key := "k" + n
			if _, err := owner(c).Start(context.Background(), control.StartRequest{Key: key, Scope: "s", Thread: "t" + n,
				Repo: "file:///unused", Input: control.Input{Text: script}}); err != nil {
				fmt.Fprintln(log, err)
				return 1
			}
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
				if notDeliveredTurn(t, st) {
					// Killed after the input was marked written and before it
					// was: it failed, visibly, and nothing ran twice.
					if got["turn_ended"] != 1 || got["text"] != 0 || got["tool_started"] != 0 {
						t.Errorf("a turn not delivered, and events %v", got)
					}
					return
				}
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

// The daemon killed around the first input, before the turn has started:
// the input is neither lost nor sent twice.
func TestDaemonKilledAroundTheFirstInput(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		for i := range 8 {
			t.Run(fmt.Sprint("point ", i), func(t *testing.T) {
				root := t.TempDir()
				t.Cleanup(func() { killSessions(root) })
				d := runDaemon(t, root, terminal, "start", "reply once")
				time.Sleep(time.Duration(i) * 40 * time.Millisecond)
				d.kill()
				d = runDaemon(t, root, terminal, "resume")
				st := openStore(t, root)
				deadline := time.Now().Add(20 * time.Second)
				for counts(t, st)["turn_ended"] < 1 {
					if time.Now().After(deadline) {
						var state, fi string
						st.DB.QueryRow(`SELECT state, first_input FROM sessions`).Scan(&state, &fi)
						lg, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
						ev, _ := os.ReadFile(filepath.Join(StateDir(root, 1), host.EventsFile))
						ents, _ := os.ReadDir(StateDir(root, 1))
						var names []string
						for _, e := range ents {
							names = append(names, e.Name())
						}
						tail, _ := os.ReadFile(filepath.Join(StateDir(root, 1), host.TerminalLog))
						t.Fatalf("no end: %v state %s first %s\nlog %s\nevents %s\nfiles %v sock %s (%d)\nterminal %q", counts(t, st), state, fi, lg, ev,
							names, host.SocketPath(StateDir(root, 1)), len(filepath.Join(StateDir(root, 1), host.SocketFile)), tail)
					}
					time.Sleep(20 * time.Millisecond)
				}
				time.Sleep(300 * time.Millisecond)
				d.kill()
				got := counts(t, st)
				if notDeliveredTurn(t, st) && got["turn_started"] == 1 && got["turn_ended"] == 1 && got["text"] == 0 {
					return // killed between marking the input written and writing it: it failed, visibly
				}
				if got["turn_started"] != 1 || got["turn_ended"] != 1 || got["text"] != 1 {
					t.Errorf("events %v", got)
				}
				// The turn names who gave the input, also when the input was
				// handed over by the daemon that came after.
				var principal, from string
				st.DB.QueryRow(`SELECT principal, from_id FROM turns`).Scan(&principal, &from)
				if principal != "owner" || from != ownerFrom {
					t.Errorf("the turn records %q from %q, want owner from %s", principal, from, ownerFrom)
				}
			})
		}
	})
}

// After a host reboot nothing of a session is left and nothing recorded its
// end: the session becomes asleep, its turn interrupted.
func TestHostRebootEndsTheTurnAsInterrupted(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	d := runDaemon(t, root, false, "start", "tool wait 60s")
	st := openStore(t, root)
	until(t, "the tool", st, "tool_started", 1)
	d.kill()
	killSessions(root) // every process gone, no status written: a reboot
	d = runDaemon(t, root, false, "resume")
	waitFor(t, "asleep", 20*time.Second, func() bool {
		var state string
		st.DB.QueryRow(`SELECT state FROM sessions`).Scan(&state)
		return state == "asleep"
	})
	var reason string
	st.DB.QueryRow(`SELECT reason FROM turns`).Scan(&reason)
	if reason != "interrupted" {
		t.Fatalf("turn reason %q", reason)
	}
	d.kill()
}

// notDeliveredTurn reports whether a turn ended as "not delivered".
func notDeliveredTurn(t *testing.T, st *store.Store) bool {
	t.Helper()
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'turn_ended' AND json_extract(data, '$.reason') = 'not delivered'`).Scan(&n)
	return n > 0
}

// aroundHooks puts a line before and after every input at
// before_prompt_build, as a plugin may.
type aroundHooks struct{}

func (aroundHooks) Run(_ context.Context, c control.HookCall) control.HookResult {
	if c.Point == core.PointBeforePromptBuild {
		return control.HookResult{Text: c.Text, Before: "plugin before", After: "plugin after"}
	}
	return control.HookResult{Text: c.Text}
}
