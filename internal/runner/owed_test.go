package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/proctest"
)

// attachOnly attaches a terminal to the thread, which wakes it with no
// input, and gives up at the test's end.
func (f *fixture) attachOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		f.core.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all | control.OpAttach}).AttachTerminal(ctx, thread)
	}()
}

// sleepNow puts the idle thread to sleep and waits for the end of session
// id's agent.
func (f *fixture) sleepNow(t *testing.T, id int64) {
	t.Helper()
	waitFor(t, "the sleep", 15*time.Second, func() bool {
		slept, err := f.run.Sleep(context.Background(), thread)
		return err == nil && slept
	})
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, id) })
}

// idleNoTurn waits until session id is idle, its agent has reported its
// conversation and its first input is done.
func (f *fixture) idleNoTurn(t *testing.T, id int64) {
	t.Helper()
	waitFor(t, "the new session idle", 15*time.Second, func() bool {
		var state, conv, fi string
		f.st.DB.QueryRow(`SELECT state, coalesce(agent_session_id, ''), first_input FROM sessions WHERE id = ?`, id).Scan(&state, &conv, &fi)
		return state == "idle" && conv != "" && fi == ""
	})
}

// inputTexts returns the texts of the thread's input events, in order.
func (f *fixture) inputTexts(t *testing.T) []string {
	t.Helper()
	rows, err := f.st.DB.Query(`SELECT json_extract(data, '$.text') FROM events WHERE kind = 'input' ORDER BY pos`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// lostDelivery is a Runner that a daemon died in before it handed an input
// over: Deliver does nothing.
type lostDelivery struct{ *Runner }

func (lostDelivery) Deliver(context.Context, int64, int64, control.Input) error { return nil }

const helloContext = contextStart + "user: reply hello\nagent: hello\n" + contextEnd

// A session that replaces one woken with no input, as for an attach, starts
// no turn, and the first input that reaches its agent comes with the
// thread's earlier conversation before it, once: a message to the idle
// session, one after it slept, or one that a daemon stopped before handing
// over. The input event records the message alone (#579).
func TestFirstMessageAfterAnAttachTellsTheEarlierConversation(t *testing.T) {
	replaced := func(t *testing.T) (*fixture, *[]*recorder, func() options) {
		conv := t.TempDir()
		recorders := &[]*recorder{}
		opts := func() options {
			rec := &recorder{inputs: map[string][]string{}}
			*recorders = append(*recorders, rec)
			return options{terminal: true, conversations: conv,
				wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }}
		}
		f := newFixture(t, opts())
		f.start("reply hello")
		waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
		f.sleepNow(t, 1)
		files, _ := filepath.Glob(filepath.Join(conv, "*"))
		for _, p := range files {
			os.Remove(p) // the agent's own retention removed it
		}
		f.attachOnly(t)
		waitFor(t, "the new session", 15*time.Second, f.has("session_replaced", 1))
		f.idleNoTurn(t, 2)
		if got := counts(t, f.st); got["turn_started"] != 1 || got["input"] != 1 {
			t.Fatalf("events %v", got)
		}
		return f, recorders, opts
	}
	// want checks what session 2's agent was given, over all its runs, and
	// that the events record the messages alone.
	want := func(t *testing.T, f *fixture, recorders []*recorder, given, recorded []string) {
		t.Helper()
		var got []string
		for _, rec := range recorders {
			got = append(got, rec.of(f.root, 2)...)
		}
		if !slices.Equal(got, given) {
			t.Errorf("the new session's inputs: %q\nwant: %q", got, given)
		}
		if got := f.inputTexts(t); !slices.Equal(got, recorded) {
			t.Errorf("the input events: %q\nwant: %q", got, recorded)
		}
		var left string
		f.st.DB.QueryRow(`SELECT context FROM sessions WHERE id = 2`).Scan(&left)
		if left != "" {
			t.Errorf("still to be told: %q", left)
		}
	}
	restart := func(t *testing.T, f *fixture, opts func() options) {
		t.Helper()
		f.run.Close()
		st, c, r, err := build(f.root, opts(), f.log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); st.Close() })
		f.core, f.run = c, r
		if err := r.Reconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("idle", func(t *testing.T) {
		f, recs, _ := replaced(t)
		send(t, owner(f.core), thread, "m2", "reply more")
		waitFor(t, "its turn", 15*time.Second, f.has("turn_ended", 2))
		send(t, owner(f.core), thread, "m3", "reply last")
		waitFor(t, "the next turn", 15*time.Second, f.has("turn_ended", 3))
		want(t, f, *recs, []string{helloContext + "reply more", "reply last"}, []string{"reply hello", "reply more", "reply last"})
	})
	t.Run("two before a report", func(t *testing.T) {
		f, recs, opts := replaced(t)
		f.core.Runner = lostDelivery{f.run}
		for _, text := range []string{"reply more", "reply last"} {
			if err := owner(f.core).Send(context.Background(), thread, text, control.Input{Text: text}, control.Deliver); err != nil {
				t.Fatal(err)
			}
		}
		restart(t, f, opts)
		waitFor(t, "the agent's answers", 15*time.Second, f.has("text", 3))
		want(t, f, *recs, []string{helloContext + "reply more", "reply last"}, []string{"reply hello", "reply more", "reply last"})
	})
	t.Run("the first never reached the agent", func(t *testing.T) {
		f, recs, _ := replaced(t)
		lost := f.core.Runner
		f.core.Runner = lostDelivery{f.run}
		send(t, owner(f.core), thread, "m2", "reply more")
		f.core.Runner = lost
		f.run.dropPending(1) // as when the codec refuses it
		if err := owner(f.core).Send(context.Background(), thread, "m3", control.Input{Text: "reply last"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "its turn", 15*time.Second, f.has("text", 2))
		want(t, f, *recs, []string{helloContext + "reply last"}, []string{"reply hello", "reply more", "reply last"})
	})
	t.Run("asleep", func(t *testing.T) {
		f, recs, _ := replaced(t)
		f.sleepNow(t, 2)
		send(t, owner(f.core), thread, "m2", "reply more")
		waitFor(t, "its turn", 15*time.Second, f.has("turn_ended", 2))
		send(t, owner(f.core), thread, "m3", "reply last")
		waitFor(t, "the next turn", 15*time.Second, f.has("turn_ended", 3))
		want(t, f, *recs, []string{helloContext + "reply more", "reply last"}, []string{"reply hello", "reply more", "reply last"})
	})
	t.Run("attached to again", func(t *testing.T) {
		f, recs, _ := replaced(t)
		f.sleepNow(t, 2)
		f.attachOnly(t)
		f.idleNoTurn(t, 2)
		send(t, owner(f.core), thread, "m2", "reply more")
		waitFor(t, "its turn", 15*time.Second, f.has("turn_ended", 2))
		want(t, f, *recs, []string{helloContext + "reply more"}, []string{"reply hello", "reply more"})
	})
	// A message queued behind a typed turn, whose agent died before it was
	// given to it. It fails visibly before the next agent starts; a daemon
	// restart must not deliver it later, including into a separate conversation.
	queued := func(t *testing.T) (*fixture, *[]*recorder, func() options) {
		f, recs, opts := replaced(t)
		c, err := host.Dial(StateDir(f.root, 2))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Input([]byte("tool wait 60s\r")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the typed turn's tool", 15*time.Second, f.has("tool_started", 1))
		if err := owner(f.core).Send(context.Background(), thread, "m2", control.Input{Text: "reply more"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		proctest.Kill(filepath.Join(StateDir(f.root, 2), host.PIDFile))
		waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 2) == "asleep" })
		return f, recs, opts
	}
	for _, again := range []bool{false, true} {
		t.Run(fmt.Sprintf("the agent died before it/restart=%t", again), func(t *testing.T) {
			f, recs, opts := queued(t)
			given := len((*recs)[0].of(f.root, 2))
			send(t, owner(f.core), thread, "m3", "reply last")
			waitFor(t, "its turn", 15*time.Second, f.has("text", 2))
			if again {
				restart(t, f, opts)
			}
			waitFor(t, "the queued input's failure", 15*time.Second, f.has("input_failed", 1))
			if got, want := (*recs)[0].of(f.root, 2)[given:], []string{helloContext + "reply last"}; !slices.Equal(got, want) {
				t.Errorf("after the wake: %q, want %q", got, want)
			}
			if again && len((*recs)[1].of(f.root, 2)) != 0 {
				t.Errorf("after the restart: %q, want no old input", (*recs)[1].of(f.root, 2))
			}
			failed := f.eventData("input_failed")
			if len(failed) != 1 || failed[0]["key"] != "m2" || failed[0]["text"] != "reply more" {
				t.Fatalf("failures: %v", failed)
			}
			if n := count(t, f.st, "pending_inputs"); n != 0 {
				t.Fatalf("%d old pending inputs left", n)
			}
		})
		t.Run(fmt.Sprintf("a conversation of one input's own/restart=%t", again), func(t *testing.T) {
			f, recs, opts := queued(t)
			given := len((*recs)[0].of(f.root, 2))
			fresh := f.core.AsFresh(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all})
			if err := fresh.Send(context.Background(), thread, "w1", control.Input{Text: "tool wake 2s\nreply woken"}, control.Deliver); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "its tool", 15*time.Second, f.has("tool_started", 2))
			if again {
				restart(t, f, opts)
			}
			waitFor(t, "asleep again", 30*time.Second, func() bool { return sessionState(t, f.st, 2) == "asleep" })
			if !f.has("input_failed", 1)() || count(t, f.st, "pending_inputs") != 0 {
				t.Fatalf("old input not retired: %v", counts(t, f.st))
			}
			send(t, owner(f.core), thread, "m3", "reply last")
			waitFor(t, "the session's own conversation", 15*time.Second, f.has("text", 3))
			for i, rec := range *recs {
				start := 0
				if i == 0 {
					start = given
				}
				for _, text := range rec.of(f.root, 2)[start:] {
					if text == "reply more" {
						t.Errorf("old input reached a new agent: %q", rec.of(f.root, 2))
					}
				}
			}
		})
	}
	t.Run("restart before the message", func(t *testing.T) {
		f, recs, opts := replaced(t)
		restart(t, f, opts)
		send(t, owner(f.core), thread, "m2", "reply more")
		waitFor(t, "its turn", 15*time.Second, f.has("turn_ended", 2))
		want(t, f, *recs, []string{helloContext + "reply more"}, []string{"reply hello", "reply more"})
	})
	t.Run("restart before the hand-over", func(t *testing.T) {
		f, recs, opts := replaced(t)
		f.core.Runner = lostDelivery{f.run}
		send(t, owner(f.core), thread, "m2", "reply more")
		restart(t, f, opts)
		waitFor(t, "its turn", 15*time.Second, f.has("turn_ended", 2))
		want(t, f, *recs, []string{helloContext + "reply more"}, []string{"reply hello", "reply more"})
	})
}

// A thread that an attach woke, whose start a dialog blocked, and that a
// person started by a choice: the new session starts no turn, and the first
// message comes with the thread's earlier conversation before it, once
// (#579).
func TestFirstMessageAfterAnAttachAndTheChoice(t *testing.T) {
	rec := &recorder{inputs: map[string][]string{}}
	f := newFixture(t, options{terminal: true, conversations: t.TempDir(),
		wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.run.TrustFiles = func(context.Context, string, string) []string { return nil }
	f.start("reply hello")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.sleepNow(t, 1)
	// From now on the fake's start-up dialog names a file.
	cfg, _ := json.Marshal(fake.Config{Command: []string{os.Args[0], "role", "fake"}, Hataori: os.Args[0], Terminal: true,
		Env: []string{"PATH=" + os.Getenv("PATH")}, Dialog: wakeDialog, DialogTrust: []string{".claude/settings.json"}})
	ad, err := fake.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec.Adapter = ad
	f.attachOnly(t)
	waitFor(t, "the blocked start", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "failed" })
	if err := owner(f.core).StartWithoutSettings(context.Background(), thread, 1); err != nil {
		t.Fatal(err)
	}
	f.idleNoTurn(t, 2)
	time.Sleep(time.Second) // a turn for it would start now
	if got := counts(t, f.st); got["turn_started"] != 1 || got["input"] != 1 {
		t.Fatalf("events %v", got)
	}
	send(t, owner(f.core), thread, "m2", "reply more")
	waitFor(t, "its turn", 15*time.Second, f.has("turn_ended", 2))
	send(t, owner(f.core), thread, "m3", "reply last")
	waitFor(t, "the next turn", 15*time.Second, f.has("turn_ended", 3))
	if got, want := rec.of(f.root, 2), []string{helloContext + "reply more", "reply last"}; !slices.Equal(got, want) {
		t.Errorf("the new session's inputs: %q\nwant: %q", got, want)
	}
	if got, want := f.inputTexts(t), []string{"reply hello", "reply more", "reply last"}; !slices.Equal(got, want) {
		t.Errorf("the input events: %q\nwant: %q", got, want)
	}
}

// A turn typed at the terminal as soon as the agent is ready, read with the
// report that it is: a message into that turn comes with the thread's
// earlier conversation too (#579).
func TestMessageIntoATurnTypedRightAfterAnAttachTellsTheEarlierConversation(t *testing.T) {
	conv := t.TempDir()
	rec := &recorder{inputs: map[string][]string{}}
	var typed atomic.Bool
	f := newFixture(t, options{terminal: true, conversations: conv,
		wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = typedAtOnce{a, &typed}; return rec }})
	f.start("reply hello")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.sleepNow(t, 1)
	files, _ := filepath.Glob(filepath.Join(conv, "*"))
	for _, p := range files {
		os.Remove(p)
	}
	typed.Store(true)
	f.attachOnly(t)
	waitFor(t, "the typed turn", 15*time.Second, f.has("turn_started", 2))
	if err := owner(f.core).Send(context.Background(), thread, "m2", control.Input{Text: "reply more"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "its answer", 15*time.Second, f.has("text", 2))
	if got, want := rec.of(f.root, 2), []string{helloContext + "reply more"}; !slices.Equal(got, want) {
		t.Errorf("the new session's inputs: %q\nwant: %q", got, want)
	}
}

// typedAtOnce is an adapter whose codec reports, while typed is set, a turn
// typed at the terminal with the report that the agent is ready.
type typedAtOnce struct {
	agent.Adapter
	typed *atomic.Bool
}

type typedAtOnceCodec struct {
	agent.Codec
	typed *atomic.Bool
}

func (a typedAtOnce) NewCodec(s agent.Spec) agent.Codec {
	return typedAtOnceCodec{a.Adapter.NewCodec(s), a.typed}
}

func (c typedAtOnceCodec) Decode(src agent.Source, line []byte) agent.Actions {
	a := c.Codec.Decode(src, line)
	for _, e := range a.Events {
		if e.Kind == agent.SessionReady && c.typed.Load() {
			a.Events = append(a.Events, agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByTerminal, Text: "typed"})
			break
		}
	}
	return a
}

var _ core.Runner = lostDelivery{}
