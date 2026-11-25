package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// talks are conversations of scope s, of which a number may run a turn at a
// time.
type talks struct {
	*kinds
	seq uint64 // of the agents' events
}

func newTalks(t *testing.T, atOnce int) *talks {
	t.Helper()
	k := &talks{newKinds(t), 100}
	k.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:U1"}}}, "owner", nil)
	k.limit(atOnce)
	return k
}

// limit changes how many may run, as a restart with another config does.
func (k *talks) limit(atOnce int) {
	s := k.core.Scopes["s"]
	s.ConversationsAtOnce = atOnce
	k.core.Scopes["s"] = s
}

// call starts conversation key with the input key.
func (k *talks) call(t *testing.T, key string) control.ThreadID {
	t.Helper()
	return k.kind(t, control.KindConversation, key)
}

func (k *talks) kind(t *testing.T, kind control.ThreadKind, key string) control.ThreadID {
	t.Helper()
	id, err := k.core.StartConfined(context.Background(), control.Caller{Principal: "owner", From: "chat:U1", Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: key, Scope: "s", Thread: key, Input: control.Input{Text: key}}, kind, InDir(k.dir))
	if err != nil {
		t.Fatalf("starting a %s thread: %v", kind, err)
	}
	return id
}

// say gives thread id the input text, as a person does in its chat thread.
func (k *talks) say(t *testing.T, id control.ThreadID, text string) {
	t.Helper()
	if err := k.sayAs(control.Caller{}, id, text, control.Deliver); err != nil {
		t.Fatalf("thread %d, %s: %v", id, text, err)
	}
}

// sayAs is say as a caller with what c sets.
func (k *talks) sayAs(c control.Caller, id control.ThreadID, text string, busy control.Busy) error {
	c.Principal, c.From, c.Scopes, c.May = "owner", "chat:U1", []string{"s"}, all
	return k.core.As(c).Send(context.Background(), id, text, control.Input{Text: text}, busy)
}

// wake gives thread id the input text, which nobody wrote.
func (k *talks) wake(t *testing.T, id control.ThreadID, text string) {
	t.Helper()
	if err := k.sayAs(control.Caller{NoAnswer: true}, id, text, control.Refuse); err != nil {
		t.Fatalf("thread %d, wake %s: %v", id, text, err)
	}
}

func (k *talks) event(t *testing.T, id control.ThreadID, e agent.Event) {
	t.Helper()
	k.seq++
	k.agentEvent(t, k.session(t, id), k.seq, e)
}

// ready is that the agent of thread id started: it is given its first input,
// in the transaction that makes it ready, as the runner does.
func (k *talks) ready(t *testing.T, id control.ThreadID) {
	t.Helper()
	ctx, sid := context.Background(), k.session(t, id)
	k.seq++
	err := k.core.Store.Update(ctx, func(tx *store.Tx) error {
		var first string
		if err := tx.QueryRowContext(ctx, `SELECT first_input FROM sessions WHERE id = ?`, sid).Scan(&first); err != nil {
			return err
		}
		var f FirstInput
		if err := json.Unmarshal([]byte(first), &f); err != nil {
			return err
		}
		if err := k.core.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.AgentEvent,
			Agent: agent.Event{Seq: k.seq, Kind: agent.SessionReady, SessionID: fmt.Sprint("conv-", id)}}); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sessions SET spec = '{}', first_input = '', launch = 0 WHERE id = ?`, sid); err != nil {
			return err
		}
		if f.NoInput {
			return nil
		}
		return k.core.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Send, Principal: f.Principal, From: f.From, Key: f.Key, Text: f.Text})
	})
	if err != nil {
		t.Fatalf("thread %d is ready: %v", id, err)
	}
}

// runs starts conversation key and brings it to its first turn.
func (k *talks) runs(t *testing.T, key string) control.ThreadID {
	t.Helper()
	id := k.call(t, key)
	k.ready(t, id)
	return id
}

// idles starts conversation key, which ran its first turn.
func (k *talks) idles(t *testing.T, key string) control.ThreadID {
	t.Helper()
	id := k.runs(t, key)
	k.ends(t, id)
	return id
}

func (k *talks) ends(t *testing.T, id control.ThreadID) {
	t.Helper()
	k.event(t, id, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
}

func (k *talks) sleeps(t *testing.T, id control.ThreadID) {
	t.Helper()
	if err := k.core.Machine.Apply(context.Background(), k.session(t, id), session.Input{Kind: session.IdleTimeout}); err != nil {
		t.Fatal(err)
	}
}

// pass makes one pass of GiveWaiting and returns what the runner got in it.
func (k *talks) pass(t *testing.T) string {
	t.Helper()
	return k.got(t, func() {
		if failed, err := k.tryPass(); failed || err != nil {
			t.Fatalf("a pass: failed %v, %v\n%s", failed, err, k.log)
		}
	})
}

func (k *talks) tryPass() (failed bool, err error) {
	var hands sync.WaitGroup
	failed, err = k.core.givePass(context.Background(), &hands)
	hands.Wait()
	return failed, err
}

// got is what the runner got while f ran.
func (k *talks) got(t *testing.T, f func()) string {
	t.Helper()
	before := k.runner.Calls()
	f()
	return strings.TrimPrefix(strings.TrimPrefix(k.runner.Calls(), before), "; ")
}

func (k *talks) waits(t *testing.T) int {
	t.Helper()
	return count(t, k.core.Store, "waiting_inputs")
}

// lines are the numbers each input_waits event of thread id names.
func (k *talks) lines(t *testing.T, id control.ThreadID) string {
	t.Helper()
	var out []string
	for _, e := range k.eventsOf(t, id, KindInputWaits) {
		if int64(e["session"].(float64)) != k.session(t, id) {
			t.Errorf("thread %d: input_waits %v is not of its session", id, e)
		}
		out = append(out, fmt.Sprint(e["running"]))
	}
	return strings.Join(out, " ")
}

// inputs are the texts of thread id's input events.
func (k *talks) inputs(t *testing.T, id control.ThreadID) string {
	t.Helper()
	var out []string
	for _, e := range k.eventsOf(t, id, session.KindInput) {
		out = append(out, e["text"].(string))
	}
	return strings.Join(out, " ")
}

func want(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: %q, want %q", what, got, want)
	}
}

// One call more than may run starts nothing: it is told so once, naming those
// that run, and starts as any start when one of them ended its turn, with the
// input, the person and the directory of its start, once (#875).
func TestACallOverTheLimitStartsWhenAPlaceIsFree(t *testing.T) {
	ctx := context.Background()
	k := newTalks(t, 2)
	a, b := k.runs(t, "a"), k.runs(t, "b")
	var c control.ThreadID
	want(t, "a call over the limit", k.got(t, func() { c = k.call(t, "c") }), "")
	if again := k.call(t, "c"); again != c {
		t.Fatalf("the key started thread %d, then %d", c, again)
	}
	want(t, "a pass with no place", k.pass(t), "")
	want(t, "the line", k.lines(t, c), fmt.Sprintf("[%d %d]", a, b))
	want(t, "another pass", k.pass(t), "")
	want(t, "the line, once", k.lines(t, c), fmt.Sprintf("[%d %d]", a, b))
	if st := k.threadState(t, c); st != control.Asleep {
		t.Fatalf("it is %s while it waits", st)
	}
	if n := len(k.eventsOf(t, c, "thread_created")); n != 1 {
		t.Fatalf("%d thread_created events", n)
	}

	k.ends(t, b)
	want(t, "a pass with a place", k.pass(t), "start c")
	want(t, "another pass", k.pass(t), "")
	var first string
	if err := k.core.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = ? AND launch = 1 AND state = 'creating'`, k.session(t, c)).Scan(&first); err != nil {
		t.Fatal(err)
	}
	var f FirstInput
	json.Unmarshal([]byte(first), &f)
	if f.Principal != "owner" || f.From != "chat:U1" || f.Key != "c" || f.Text != "c" || f.Dir != k.dir || f.Wake || f.NoInput {
		t.Fatalf("it starts with %+v", f)
	}
	k.ready(t, c)
	want(t, "its input", k.inputs(t, c), "c")
	if k.waits(t) != 0 {
		t.Fatal("it still waits")
	}
	// A repeat of the call's key is not input.
	if again := k.call(t, "c"); again != c || k.waits(t) != 0 {
		t.Fatalf("the key, again: thread %d, %d waiting", again, k.waits(t))
	}
	if _, err := k.core.StartConfined(ctx, control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "c", Scope: "s", Thread: "c", Input: control.Input{Text: "c"}}, control.KindConversation, InDir(k.dir)); err != nil {
		t.Fatal(err)
	}
	want(t, "its input, still", k.inputs(t, c), "c")
}

// recordingRunner keeps what it is asked to start and to resume.
type recordingRunner struct {
	*fakeRunner
	started []Launch
	resumed []FirstInput
}

func (r *recordingRunner) Start(ctx context.Context, l Launch) error {
	r.started = append(r.started, l)
	return r.fakeRunner.Start(ctx, l)
}

func (r *recordingRunner) Resume(ctx context.Context, sid int64, f FirstInput) error {
	r.resumed = append(r.resumed, f)
	return r.fakeRunner.Resume(ctx, sid, f)
}

// What waited is started and woken as it would have been without a wait.
func TestWhatWaitedIsLaunchedAsWithoutAWait(t *testing.T) {
	k := newTalks(t, 1)
	k.core.Preface = func(context.Context, *store.Tx, control.ThreadID) (string, error) { return "preface ", nil }
	r := &recordingRunner{fakeRunner: k.runner}
	k.core.Runner = r
	sleeper := k.idles(t, "sleeper")
	k.sleeps(t, sleeper)

	direct := k.call(t, "direct")
	waited := k.call(t, "waited")
	k.ready(t, direct)
	k.ends(t, direct)
	want(t, "the start", k.pass(t), "start waited")
	if len(r.started) != 3 {
		t.Fatalf("%d starts", len(r.started))
	}
	a, b := r.started[1], r.started[2]
	for _, l := range []struct {
		Launch
		id  control.ThreadID
		key string
	}{{a, direct, "direct"}, {b, waited, "waited"}} {
		if l.Session != k.session(t, l.id) || l.Thread != l.id || l.Key != l.key || l.Input.Text != l.key ||
			l.Input.Link == nil || l.Input.Link.Thread != l.key {
			t.Fatalf("%s is started as %+v", l.key, l.Launch)
		}
	}
	a.Session, a.Thread, a.Key, a.Input.Text, a.Input.Link = b.Session, b.Thread, b.Key, b.Input.Text, b.Input.Link
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("with no wait %+v, after one %+v", a, b)
	}

	// Asleep, with the place taken.
	want(t, "a message", k.got(t, func() { k.say(t, sleeper, "up") }), "")
	k.ready(t, waited)
	k.ends(t, waited)
	want(t, "the wake", k.pass(t), "resume up")
	if f := r.resumed[0]; f.Principal != "owner" || f.From != "chat:U1" || f.Key != "up" || !f.Wake || f.Context != "preface " || f.NoInput {
		t.Fatalf("woken with %+v", f)
	}
}

// A message for a conversation that is idle or asleep waits as a call does,
// is accepted once, and is given with its place.
func TestAMessageOverTheLimitWaits(t *testing.T) {
	for _, asleep := range []bool{false, true} {
		t.Run(fmt.Sprintf("asleep %v", asleep), func(t *testing.T) {
			k := newTalks(t, 1)
			quiet := k.idles(t, "quiet")
			if asleep {
				k.sleeps(t, quiet)
			}
			busy := k.runs(t, "busy")
			want(t, "a message over the limit", k.got(t, func() { k.say(t, quiet, "m"); k.say(t, quiet, "m") }), "")
			want(t, "a pass with no place", k.pass(t), "")
			want(t, "the line", k.lines(t, quiet), fmt.Sprintf("[%d]", busy))
			want(t, "its inputs", k.inputs(t, quiet), "quiet")
			k.ends(t, busy)
			if asleep {
				want(t, "a pass with a place", k.pass(t), "resume m")
				k.ready(t, quiet)
			} else {
				want(t, "a pass with a place", k.pass(t), "deliver m")
			}
			want(t, "its inputs", k.inputs(t, quiet), "quiet m")
			if k.waits(t) != 0 {
				t.Fatal("it waits still")
			}
			if st := k.threadState(t, quiet); st != control.Running {
				t.Fatalf("it is %s", st)
			}
			k.say(t, quiet, "m") // the key again
			want(t, "its inputs", k.inputs(t, quiet), "quiet m")
			if k.waits(t) != 0 {
				t.Fatal("a key that was given waits")
			}
		})
	}
}

// A conversation that waits is told once per wait, not once per message, and
// again when it waits again.
func TestOneLinePerWait(t *testing.T) {
	k := newTalks(t, 1)
	quiet := k.idles(t, "quiet")
	busy := k.runs(t, "busy")
	for _, m := range []string{"m1", "m2", "m3"} {
		k.say(t, quiet, m)
		k.pass(t)
	}
	line := fmt.Sprintf("[%d]", busy)
	want(t, "the line", k.lines(t, quiet), line)
	k.ends(t, busy)
	want(t, "a place", k.pass(t), "deliver m1")
	// The other one takes the place when its turn ends: m2 waits again.
	k.say(t, busy, "n1")
	want(t, "while it runs", k.pass(t), "")
	want(t, "no line while its own turn runs", k.lines(t, quiet), line)
	k.ends(t, quiet)
	want(t, "the next place", k.pass(t), "deliver n1")
	want(t, "the second line", k.lines(t, quiet), line+" "+line)
	k.pass(t)
	want(t, "the second line, once", k.lines(t, quiet), line+" "+line)
	k.ends(t, busy)
	want(t, "m2", k.pass(t), "deliver m2")
	k.ends(t, quiet)
	want(t, "m3", k.pass(t), "deliver m3")
	want(t, "no line when it did not wait for others", k.lines(t, quiet), line+" "+line)
	want(t, "in order", k.inputs(t, quiet), "quiet m1 m2 m3")
}

// Two that wait get places in the order their messages came, and one with
// many messages does not hold the other's first back.
func TestPlacesGoToWhatWaitsLongest(t *testing.T) {
	k := newTalks(t, 1)
	a, b, c := k.idles(t, "a"), k.idles(t, "b"), k.idles(t, "c")
	busy := k.runs(t, "busy")
	k.say(t, b, "b1")
	k.say(t, a, "a1")
	k.say(t, a, "a2")
	k.say(t, a, "a3")
	k.say(t, c, "c1")
	d := k.call(t, "d")
	k.say(t, a, "a4")
	k.ends(t, busy)
	var got []string
	for range 8 {
		g := k.pass(t)
		got = append(got, g)
		want(t, "while it runs", k.pass(t), "")
		switch {
		case strings.HasPrefix(g, "deliver a"):
			k.ends(t, a)
		case strings.HasPrefix(g, "deliver b"):
			k.ends(t, b)
		case strings.HasPrefix(g, "deliver c"):
			k.ends(t, c)
		case g == "start d":
			k.ready(t, d)
			k.ends(t, d)
		}
	}
	want(t, "the order", strings.Join(got, ", "),
		"deliver b1, deliver a1, deliver c1, start d, deliver a2, deliver a3, deliver a4, ")
}

// A person's message gets its place before a wake that waits longer, and only
// the message gets the line.
func TestAPersonsMessageGoesBeforeAWake(t *testing.T) {
	k := newTalks(t, 1)
	a, b := k.idles(t, "a"), k.idles(t, "b")
	busy := k.runs(t, "busy")
	k.wake(t, a, "wake")
	k.pass(t)
	want(t, "the line of a wake", k.lines(t, a), "")
	k.say(t, b, "b1")
	k.pass(t)
	want(t, "the line of a message", k.lines(t, b), fmt.Sprintf("[%d]", busy))
	k.ends(t, busy)
	want(t, "first", k.pass(t), "deliver b1")
	k.ends(t, b)
	want(t, "then", k.pass(t), "deliver wake")
}

// A person's message behind a wake that waits gets the line, and goes first.
func TestAPersonsMessageBehindAWakeGetsTheLine(t *testing.T) {
	k := newTalks(t, 1)
	a := k.idles(t, "a")
	busy := k.runs(t, "busy")
	k.wake(t, a, "wake")
	k.pass(t)
	k.say(t, a, "a1")
	k.pass(t)
	want(t, "the line", k.lines(t, a), fmt.Sprintf("[%d]", busy))
	k.ends(t, busy)
	want(t, "first", k.pass(t), "deliver a1")
	k.ends(t, a)
	want(t, "then", k.pass(t), "deliver wake")
}

// A place that is free while others wait for it is not taken by a newcomer,
// which waits behind them.
func TestANewcomerWaitsBehindThoseThatWait(t *testing.T) {
	k := newTalks(t, 1)
	a, b := k.idles(t, "a"), k.idles(t, "b")
	busy := k.runs(t, "busy")
	k.say(t, a, "a1")
	k.ends(t, busy)
	want(t, "a message", k.got(t, func() { k.say(t, b, "b1") }), "")
	want(t, "a call", k.got(t, func() { k.call(t, "c") }), "")
	want(t, "the longest", k.pass(t), "deliver a1")
	k.ends(t, a)
	want(t, "the next", k.pass(t), "deliver b1")
	k.ends(t, b)
	want(t, "the last", k.pass(t), "start c")
}

// A conversation that waits for something else than a place (its question is
// open, it failed) holds no place of others back.
func TestWhatCannotBeGivenHoldsNoPlaceBack(t *testing.T) {
	k := newTalks(t, 1)
	blocked, b := k.idles(t, "a"), k.idles(t, "b")
	busy := k.runs(t, "busy")
	k.say(t, blocked, "a1")
	k.say(t, blocked, "a2")
	k.ends(t, busy)
	want(t, "the first", k.pass(t), "deliver a1")
	k.event(t, blocked, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r", Text: "which?"}})
	want(t, "a2 is no answer", k.pass(t), "")
	want(t, "another's message", k.got(t, func() { k.say(t, b, "b1") }), "deliver b1")
}

// A message to a conversation whose turn runs waits for its end, and is not
// given to the agent before, which would start a turn with it; with no limit
// it is given at once, as to any thread.
func TestAMessageToARunningConversation(t *testing.T) {
	k := newTalks(t, 2)
	a := k.runs(t, "a")
	want(t, "a message to one that runs", k.got(t, func() { k.say(t, a, "m1"); k.say(t, a, "m2") }), "")
	want(t, "while it runs", k.pass(t), "")
	if l := k.lines(t, a); l != "" {
		t.Fatalf("it is told that it waits: %s", l)
	}
	k.ends(t, a)
	want(t, "after it", k.pass(t), "deliver m1")
	k.ends(t, a)
	want(t, "idle, with one that waits", k.got(t, func() { k.say(t, a, "m3") }), "")
	want(t, "in order", k.pass(t), "deliver m2")
	k.ends(t, a)
	want(t, "in order", k.pass(t), "deliver m3")
	if l := k.lines(t, a); l != "" {
		t.Fatalf("it is told that it waits: %s", l)
	}

	k.limit(0)
	want(t, "without a limit", k.got(t, func() { k.say(t, a, "m4") }), "deliver m4")
}

// An interrupt with a message that waits stops the turn that runs, and never
// the one that message starts.
func TestAnInterruptWithAMessageThatWaits(t *testing.T) {
	k := newTalks(t, 1)
	a := k.runs(t, "a")
	var turn int64
	if err := k.core.Store.DB.QueryRow(`SELECT id FROM turns WHERE session_id = ?`, k.session(t, a)).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	send := func(text string) string {
		return k.got(t, func() {
			if err := k.sayAs(control.Caller{}, a, text, control.Interrupt); err != nil {
				t.Fatal(err)
			}
		})
	}
	want(t, "while its turn runs", send("m1"), fmt.Sprintf("interrupt turn %d", turn))
	k.event(t, a, agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted})
	want(t, "then", k.pass(t), "deliver m1")
	// Its turn ends, and the message is given, before the interrupt is made.
	k.runner.beforeInterrupt = func() {
		k.ends(t, a)
		k.pass(t)
	}
	want(t, "once it ended", send("m2"), "deliver m2")
	want(t, "in order", k.inputs(t, a), "a m1 m2")

	// With another one's turn in its way, and none of its own.
	k.ends(t, a)
	k.runs(t, "b")
	want(t, "with nothing to interrupt", send("m3"), "")
	if k.waits(t) != 1 {
		t.Fatal("the message is lost")
	}
}

// A turn that ends in whatever way gives its place up.
func TestAPlaceIsGivenUp(t *testing.T) {
	for how, end := range map[string]func(k *talks, id control.ThreadID){
		"ended": func(k *talks, id control.ThreadID) { k.ends(t, id) },
		"stopped": func(k *talks, id control.ThreadID) {
			k.event(t, id, agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted})
		},
		"failed turn": func(k *talks, id control.ThreadID) {
			k.event(t, id, agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.APIError})
		},
		"blocked on a question": func(k *talks, id control.ThreadID) {
			k.event(t, id, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r", Text: "which?"}})
		},
		"crashed": func(k *talks, id control.ThreadID) {
			if err := k.core.Machine.Apply(context.Background(), k.session(t, id), session.Input{Kind: session.Exit}); err != nil {
				t.Fatal(err)
			}
		},
		"closed": func(k *talks, id control.ThreadID) {
			k.ends(t, id)
			k.close(t, id)
		},
	} {
		t.Run(how, func(t *testing.T) {
			k := newTalks(t, 1)
			busy := k.runs(t, "busy")
			k.call(t, "next")
			want(t, "while it runs", k.pass(t), "")
			end(k, busy)
			want(t, "after it", k.pass(t), "start next")
		})
	}
	t.Run("failed at its start", func(t *testing.T) {
		k := newTalks(t, 1)
		busy := k.call(t, "busy")
		next := k.call(t, "next")
		want(t, "while it starts", k.pass(t), "")
		want(t, "the line names one that starts", k.lines(t, next), fmt.Sprintf("[%d]", busy))
		if err := k.core.Machine.Apply(context.Background(), k.session(t, busy), session.Input{Kind: session.StartTimeout}); err != nil {
			t.Fatal(err)
		}
		if st := k.threadState(t, busy); st != control.Failed {
			t.Fatalf("it is %s", st)
		}
		want(t, "after it", k.pass(t), "start next")
	})
}

// An answered question's turn goes on at once, though no place is free.
func TestAnAnsweredTurnGoesOnAtOnce(t *testing.T) {
	k := newTalks(t, 1)
	asks := k.runs(t, "asks")
	k.event(t, asks, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r", Text: "which?"}})
	busy := k.runs(t, "busy")
	k.call(t, "waits")
	want(t, "the answer", k.got(t, func() { k.say(t, asks, "blue") }), "answer r")
	if st := k.threadState(t, asks); st != control.Running {
		t.Fatalf("it is %s", st)
	}
	k.ends(t, busy)
	want(t, "no place is free yet", k.pass(t), "")
	k.ends(t, asks)
	want(t, "now one is", k.pass(t), "start waits")
}

// Threads of the other kinds take no place and wait for none, and neither
// does another scope's conversation.
func TestOthersTakeNoPlace(t *testing.T) {
	ctx := context.Background()
	k := newTalks(t, 1)
	start := func(kind control.ThreadKind, key string) control.ThreadID {
		if kind != control.KindOrdinary {
			return k.kind(t, kind, key)
		}
		id, err := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all}).Start(ctx,
			control.StartRequest{Key: key, Scope: "s", Thread: key, Input: control.Input{Text: key}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	kinds := []control.ThreadKind{control.KindOrdinary, control.KindSupervisor, control.KindLookup}
	var own control.ThreadID
	for _, kind := range kinds {
		id := start(kind, "early "+string(kind))
		k.ready(t, id)
		if kind == control.KindSupervisor {
			own = id
		}
	}
	var busy control.ThreadID
	want(t, "a call beside turns of the other kinds", k.got(t, func() { busy = k.call(t, "busy") }), "start busy")
	k.ready(t, busy)
	k.call(t, "waits")

	k.ends(t, own)
	want(t, "the own thread's wake", k.got(t, func() { k.wake(t, own, "wake") }), "deliver wake")
	want(t, "a message to it as it runs", k.got(t, func() { k.say(t, own, "m") }), "deliver m")
	k.ends(t, own)
	k.sleeps(t, own)
	want(t, "a wake of it asleep", k.got(t, func() { k.wake(t, own, "wake2") }), "resume wake2")
	for _, kind := range []control.ThreadKind{control.KindOrdinary, control.KindLookup} {
		var id control.ThreadID
		key := "late " + string(kind)
		want(t, key, k.got(t, func() { id = start(kind, key) }), "start "+key)
		k.ready(t, id)
		want(t, key+" in its turn", k.got(t, func() { k.say(t, id, key+" m") }), "deliver "+key+" m")
		k.ends(t, id)
		want(t, key+" idle", k.got(t, func() { k.say(t, id, key+" n") }), "deliver "+key+" n")
	}
	k.core.Scopes["other"] = Scope{Enabled: true, Owner: "chat", ConversationsAtOnce: 1}
	want(t, "another scope's", k.got(t, func() {
		if _, err := k.core.StartConfined(ctx, control.Caller{Principal: "owner", Scopes: []string{"other"}, May: all},
			control.StartRequest{Key: "o", Scope: "other", Thread: "o", Input: control.Input{Text: "o"}}, control.KindConversation, InDir(k.dir)); err != nil {
			t.Fatal(err)
		}
	}), "start o")
	want(t, "the one that waits", k.pass(t), "")
	k.ends(t, busy)
	want(t, "with its place", k.pass(t), "start waits")
}

// A conversation that is only woken, as for a person's terminal, runs no turn
// and takes no place.
func TestAConversationOnlyWokenTakesNoPlace(t *testing.T) {
	k := newTalks(t, 1)
	a := k.idles(t, "a")
	k.sleeps(t, a)
	ctx, sid := context.Background(), k.session(t, a)
	if err := k.core.Store.Update(ctx, func(tx *store.Tx) error {
		fi, _ := json.Marshal(FirstInput{Wake: true, NoInput: true})
		if _, err := tx.Exec(`UPDATE sessions SET first_input = ? WHERE id = ?`, string(fi), sid); err != nil {
			return err
		}
		return k.core.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Wake})
	}); err != nil {
		t.Fatal(err)
	}
	want(t, "a call", k.got(t, func() { k.call(t, "b") }), "start b")
}

// What waits for a thread that is closed is dropped, with a line in the log,
// and is not given once it is reopened.
func TestAClosedConversationsInputIsDropped(t *testing.T) {
	ctx := context.Background()
	k := newTalks(t, 1)
	a, b := k.idles(t, "a"), k.idles(t, "b")
	busy := k.runs(t, "busy")
	k.say(t, a, "a1")
	k.say(t, a, "a2")
	k.say(t, b, "b1")
	k.close(t, a)
	if n := k.waits(t); n != 1 {
		t.Fatalf("%d inputs wait", n)
	}
	if !strings.Contains(k.log.String(), fmt.Sprintf("thread=%d inputs=2", a)) {
		t.Fatalf("log:\n%s", k.log)
	}
	if _, err := k.owner.ReopenThread(ctx, a); err != nil {
		t.Fatal(err)
	}
	k.ends(t, busy)
	want(t, "b's", k.pass(t), "deliver b1")
	k.ends(t, b)
	want(t, "nothing else", k.pass(t), "")
	// A close that is refused drops nothing.
	k.runs(t, "busy2")
	k.say(t, b, "b2")
	k.runner.shutErr = errors.New("not now")
	if err := k.owner.CloseThread(ctx, b); err == nil {
		t.Fatal("closed")
	}
	if n := k.waits(t); n != 1 {
		t.Fatalf("%d inputs wait after a refused close", n)
	}
}

// One whose input cannot be given is logged and tried again, and holds no
// other back.
func TestOneThatCannotBeGivenHoldsNobodyBack(t *testing.T) {
	k := newTalks(t, 1)
	a, b := k.idles(t, "a"), k.idles(t, "b")
	busy := k.runs(t, "busy")
	k.say(t, a, "a1")
	k.say(t, b, "b1")
	if _, err := k.core.Store.DB.Exec(`UPDATE waiting_inputs SET input = '{' || input WHERE thread_id = ?`, int64(a)); err != nil {
		t.Fatal(err)
	}
	k.ends(t, busy)
	want(t, "b's", k.got(t, func() {
		if failed, err := k.tryPass(); !failed || err != nil {
			t.Fatalf("a pass: failed %v, %v", failed, err)
		}
	}), "deliver b1")
	if !strings.Contains(k.log.String(), fmt.Sprintf("input that waits is not given\" thread=%d", a)) {
		t.Fatalf("log:\n%s", k.log)
	}
	if n := k.waits(t); n != 1 {
		t.Fatalf("%d inputs wait", n)
	}
	k.ends(t, b)
	if _, err := k.core.Store.DB.Exec(`UPDATE waiting_inputs SET input = substr(input, 2)`); err != nil {
		t.Fatal(err)
	}
	want(t, "a's", k.pass(t), "deliver a1")
}

// A wake that asks for a conversation of its own is refused while no place is
// free, and its key is not taken.
func TestAFreshWakeOfAConversationIsRefusedWithoutAPlace(t *testing.T) {
	k := newTalks(t, 1)
	a := k.idles(t, "a")
	k.sleeps(t, a)
	busy := k.runs(t, "busy")
	fresh := k.core.AsFresh(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, NoAnswer: true})
	send := func() error {
		return fresh.Send(context.Background(), a, "w", control.Input{Text: "w"}, control.Refuse)
	}
	if err := send(); !errors.Is(err, ErrBusy) {
		t.Fatalf("without a place: %v", err)
	}
	if k.waits(t) != 0 {
		t.Fatal("it waits")
	}
	k.ends(t, busy)
	want(t, "with one", k.got(t, func() {
		if err := send(); err != nil {
			t.Fatal(err)
		}
	}), "resume w")
}

// A limit taken out of the config, or made larger, gives what waited under
// it; a smaller one takes nobody's place.
func TestAnotherLimit(t *testing.T) {
	k := newTalks(t, 1)
	a, b := k.idles(t, "a"), k.idles(t, "b")
	busy := k.runs(t, "busy")
	k.say(t, a, "a1")
	k.say(t, b, "b1")
	k.say(t, busy, "m")
	want(t, "with one", k.pass(t), "")
	k.limit(2)
	want(t, "with two", k.pass(t), "deliver a1")
	k.limit(1)
	k.ends(t, a)
	want(t, "with one again", k.pass(t), "")
	k.limit(0)
	want(t, "with none", k.pass(t), "deliver b1")
	want(t, "while its turn runs", k.pass(t), "")
	k.ends(t, busy)
	want(t, "after it", k.pass(t), "deliver m")
}

// GiveWaiting gives what waits when a place becomes free and when input
// comes for a free one, and what waited when the daemon started, once. It
// ends with its context, and at once when nothing can ever wait.
func TestGiveWaiting(t *testing.T) {
	k := newTalks(t, 1)
	a := k.idles(t, "a")
	busy := k.runs(t, "busy")
	k.say(t, a, "a1")
	calls := k.runner.Calls()
	gives := func() (end func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			k.core.GiveWaiting(ctx)
		}()
		return func() {
			t.Helper()
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("GiveWaiting does not end")
			}
		}
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%s: not so; the runner got %q since\n%s", what, strings.TrimPrefix(k.runner.Calls(), calls), k.log)
			}
		}
	}
	runner := func(want string) {
		t.Helper()
		until("the runner got "+want, func() bool { return strings.TrimPrefix(k.runner.Calls(), calls) == want })
	}
	end := gives()
	until("the line", func() bool { return k.lines(t, a) != "" })
	end()
	runner("")

	// It ends and starts, as at a restart; meanwhile the turn ends.
	k.ends(t, busy)
	end = gives()
	runner("; deliver a1")
	k.say(t, a, "a2") // to its own turn
	k.say(t, busy, "b1")
	k.ends(t, a)
	runner("; deliver a1; deliver b1")
	k.ends(t, busy)
	runner("; deliver a1; deliver b1; deliver a2")
	k.ends(t, a)
	// With no place free, then nobody's turn in the way: told by the message.
	end()
	k.core.places = places{}
	k.say(t, busy, "b2")
	k.say(t, busy, "b3")
	k.ends(t, busy)
	end = gives()
	runner("; deliver a1; deliver b1; deliver a2; deliver b2; deliver b3")
	k.say(t, busy, "b4")
	k.ends(t, busy)
	runner("; deliver a1; deliver b1; deliver a2; deliver b2; deliver b3; deliver b4")
	end()

	k.ends(t, busy)
	k.limit(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.core.GiveWaiting(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("GiveWaiting goes on though nothing waits and nothing can")
	}
}
