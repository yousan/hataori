package runner

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
)

// dying is a runner whose daemon dies as the core hands it a call: after
// the core committed it, before the agent got it.
type dying struct{ core.Runner }

func die() error {
	syscall.Kill(os.Getpid(), syscall.SIGKILL) // this process only
	select {}
}

func (dying) Start(context.Context, core.Launch) error                   { return die() }
func (dying) Deliver(context.Context, int64, int64, control.Input) error { return die() }
func (dying) Answer(context.Context, int64, int64, string, control.Answer) error {
	return die()
}
func (dying) Resume(context.Context, int64, control.Principal, string, string, control.Input) error {
	return die()
}

// dieBeforeHandOver makes one call on thread t1: start (a new thread),
// send (input), answer (the open request, through Answer) or send-answer
// (the open request, through Send).
func dieBeforeHandOver(st *store.Store, c *core.Core, call, text string) error {
	ctx := context.Background()
	th := control.ThreadID(1)
	in := control.Input{Text: text}
	switch call {
	case "start":
		_, err := owner(c).Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: "file:///unused", Input: in})
		return err
	case "send", "send-answer":
		return owner(c).Send(ctx, th, "d1", in, control.Deliver)
	case "answer":
		var h string
		if err := st.DB.QueryRow(`SELECT handle FROM requests WHERE state = 'open'`).Scan(&h); err != nil {
			return err
		}
		return owner(c).Answer(ctx, control.RequestID(h), control.Answer{Text: text})
	}
	return nil
}

// waitKilled waits for a die daemon to kill itself.
func (p *proc) waitKilled(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		p.cmd.Process.Signal(syscall.SIGKILL)
		<-done
		t.Fatal("the daemon did not die at the hand-over")
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the daemon ended with %v, not at the hand-over", p.cmd.ProcessState)
	}
}

// withText counts the events of kind whose text is text.
func withText(t *testing.T, st *store.Store, kind, text string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = ? AND json_extract(data, '$.text') = ?`, kind, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A daemon killed after the core committed an input and before it handed
// it over delivers it exactly once after it starts again, and not again
// after a further restart.
func TestInputPendingAtADaemonKill(t *testing.T) {
	type check struct {
		kind, text string
		n          int
	}
	cases := []struct {
		name   string
		setup  string // the first daemon's script, if any
		ready  func(t *testing.T, root string, st *store.Store)
		call   string
		text   string
		turns  int
		checks []check
		idle   bool
	}{
		{name: "start", call: "start", text: "reply hello", turns: 1,
			checks: []check{{"input", "reply hello", 1}, {"text", "hello", 1}}},
		{name: "send to a running turn", setup: "tool wait 2s\nreply one", call: "send", text: "reply two", turns: 2,
			ready:  func(t *testing.T, root string, st *store.Store) { until(t, "the tool", st, "tool_started", 1) },
			checks: []check{{"input", "reply two", 1}, {"text", "one", 1}, {"text", "two", 1}}},
		{name: "answer", setup: "ask which colour?", call: "answer", text: "blue", turns: 2,
			ready:  func(t *testing.T, root string, st *store.Store) { until(t, "the question", st, "request", 1) },
			checks: []check{{"answer", "blue", 1}, {"text", "answer: blue", 1}}},
		{name: "send as the answer", setup: "ask which colour?", call: "send-answer", text: "blue", turns: 2,
			ready:  func(t *testing.T, root string, st *store.Store) { until(t, "the question", st, "request", 1) },
			checks: []check{{"answer", "blue", 1}, {"input", "blue", 0}, {"text", "answer: blue", 1}}},
		{name: "send to an asleep thread", setup: "reply hi", call: "send", text: "reply again", turns: 2, idle: true,
			ready: func(t *testing.T, root string, st *store.Store) {
				waitFor(t, "asleep", 20*time.Second, func() bool {
					return sessionState(t, st, 1) == "asleep" && !agentAlive(t, root, 1)
				})
			},
			checks: []check{{"input", "reply again", 1}, {"text", "hi", 1}, {"text", "again", 1}}},
	}
	forms(t, func(t *testing.T, terminal bool) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				t.Cleanup(func() { killSessions(root) })
				if tc.setup != "" {
					mode := "start"
					if tc.idle {
						mode += "+idle"
					}
					d := runDaemon(t, root, terminal, mode, tc.setup)
					tc.ready(t, root, openStore(t, root))
					d.kill()
				}
				runDaemon(t, root, terminal, "die", tc.call, tc.text).waitKilled(t)
				st := openStore(t, root)
				d := runDaemon(t, root, terminal, "resume")
				until(t, "the turns' ends", st, "turn_ended", tc.turns)
				time.Sleep(300 * time.Millisecond) // anything late would show now
				d.kill()
				got := counts(t, st)
				verify := func(when string) {
					t.Helper()
					if got["turn_started"] != tc.turns || got["turn_ended"] != tc.turns {
						t.Errorf("%s: %d turns started, %d ended, want %d (all: %v)", when, got["turn_started"], got["turn_ended"], tc.turns, got)
					}
					for _, c := range tc.checks {
						if n := withText(t, st, c.kind, c.text); n != c.n {
							t.Errorf("%s: %d %s events %q, want %d (all: %v)", when, n, c.kind, c.text, c.n, got)
						}
					}
				}
				verify("after the restart")
				if n := count(t, st, "pending_inputs"); n != 0 {
					t.Errorf("%d pending inputs left", n)
				}

				// Delivered and reported: a further restart delivers nothing.
				d = runDaemon(t, root, terminal, "resume")
				time.Sleep(time.Second)
				d.kill()
				if again := counts(t, st); again["turn_started"] != got["turn_started"] || again["input"] != got["input"] ||
					again["answer"] != got["answer"] || again["text"] != got["text"] {
					t.Errorf("after a further restart: %v, before %v", again, got)
				}
				verify("after a further restart")
			})
		}
	})
}
