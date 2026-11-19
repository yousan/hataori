package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// showing runs the event reader of f until the returned function is called.
func (f *front) showing(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.fe.show(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// started delivers a trigger and returns the thread and the trigger.
func (f *front) started(t *testing.T) (control.ThreadID, surface.Inbound) {
	t.Helper()
	in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git the prompt")
	ths := f.threads()
	if len(ths) != 1 {
		t.Fatalf("%d threads", len(ths))
	}
	return f.threadAt(t, ths[0].Opened), in
}

// event appends an event of thread id with data given as fields.
func (f *front) event(t *testing.T, id control.ThreadID, kind string, data map[string]any) uint64 {
	t.Helper()
	data["session"] = 1
	b, _ := json.Marshal(data)
	var pos uint64
	err := f.fe.Outbox.Store.Update(context.Background(), func(tx *store.Tx) (err error) {
		pos, err = tx.Append(context.Background(), control.Event{Thread: id, Kind: control.EventKind(kind), Data: b})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

// conv is the conversation thread id is shown in, through its link.
func (f *front) conv(id control.ThreadID) surface.Conversation {
	c, ok, err := f.fe.convOf(context.Background(), id)
	if err != nil || !ok {
		panic(fmt.Sprintf("thread %d has no link: %v", id, err))
	}
	return c
}

// threadAt is the thread linked to conv.
func (f *front) threadAt(t *testing.T, conv surface.Conversation) control.ThreadID {
	t.Helper()
	api := f.fe.API(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpRead})
	th, ok, err := f.fe.threadIn(context.Background(), api, conv)
	if err != nil || !ok {
		t.Fatalf("no thread linked to %+v: %v", conv, err)
	}
	return th.ID
}

// postedUntil waits for a post with text last in the thread, and returns
// the thread's posts.
func (f *front) postedUntil(t *testing.T, id control.ThreadID, last string) []fake.Call {
	t.Helper()
	var calls []fake.Call
	waitFor(t, "a post of "+last, func() bool {
		calls = nil
		for _, c := range f.driver.Calls(f.conv(id)) {
			if c.Op == fake.OpPost {
				calls = append(calls, c)
			}
		}
		return len(calls) > 0 && calls[len(calls)-1].Message.Text == last
	})
	return calls
}

func texts(calls []fake.Call) string {
	var s []string
	for _, c := range calls {
		s = append(s, c.Message.Text)
	}
	return strings.Join(s, "|")
}

func turn(f *front, t *testing.T, id control.ThreadID, n int, key string) {
	t.Helper()
	d := map[string]any{"turn": n, "by": "caller", "principal": hook}
	if key != "" {
		d["key"] = key
	} else {
		d["by"] = "agent"
		delete(d, "principal")
	}
	f.event(t, id, "turn_started", d)
}

func ended(f *front, t *testing.T, id control.ThreadID, n int, outcome, reason string) {
	t.Helper()
	d := map[string]any{"turn": n, "by": "caller", "outcome": outcome, "background": 0}
	if reason != "" {
		d["reason"] = reason
	}
	f.event(t, id, "turn_ended", d)
}

func TestTextQuietAndTheFinalAnswerRepliesToTheTurnsMessage(t *testing.T) {
	f := newFront(t, workspaces{})
	id, trigger := f.started(t)
	f.showing(t)
	turn(f, t, id, 1, trigger.Ref.ID)
	f.event(t, id, "text", map[string]any{"text": "Looking.", "final": false})
	f.event(t, id, "text", map[string]any{"text": "Done.", "final": true})
	ended(f, t, id, 1, "completion", "")
	// A follow-up in the thread starts the second turn.
	follow := f.driver.Inject(f.conv(id), fake.Webhook(hook), "and the docs")
	turn(f, t, id, 2, follow.Ref.ID)
	f.event(t, id, "text", map[string]any{"text": "Docs done.", "final": true})
	ended(f, t, id, 2, "completion", "")
	calls := f.postedUntil(t, id, "Docs done.")
	if got := texts(calls); got != "the prompt|Looking.|Done.|Docs done." {
		t.Fatalf("posts %s", got)
	}
	prompt, progress, final, final2 := calls[0], calls[1], calls[2], calls[3]
	if !progress.Message.Quiet || progress.Message.ReplyTo != nil {
		t.Fatalf("progress text %+v, want quiet and no reply", progress.Message)
	}
	if final.Message.Quiet || final.Message.ReplyTo == nil || *final.Message.ReplyTo != prompt.Ref {
		t.Fatalf("final answer %+v, want a reply to the prompt %+v, which stands for the trigger", final.Message, prompt.Ref)
	}
	if r := final2.Message.ReplyTo; r == nil || *r != follow.Ref {
		t.Fatalf("second final answer replies to %+v, want %+v", r, follow.Ref)
	}
}

func TestQuestionWithNumberedOptions(t *testing.T) {
	f := newFront(t, workspaces{})
	id, trigger := f.started(t)
	f.showing(t)
	turn(f, t, id, 1, trigger.Ref.ID)
	f.event(t, id, "request", map[string]any{"request": "q1", "approval": false, "text": "Which colour?", "choices": []string{"red", "blue"}})
	calls := f.postedUntil(t, id, "Which colour?\n\n1. red\n2. blue")
	if len(calls) != 2 {
		t.Fatalf("posts %s", texts(calls))
	}
}

func TestOneLineForAFailedTurnAndNoneForACompletedOne(t *testing.T) {
	f := newFront(t, workspaces{})
	id, trigger := f.started(t)
	f.showing(t)
	for i, reason := range []string{"limit", "login", "crashed", "api_error"} {
		turn(f, t, id, i+1, trigger.Ref.ID)
		ended(f, t, id, i+1, "failure", reason)
	}
	turn(f, t, id, 5, trigger.Ref.ID)
	ended(f, t, id, 5, "completion", "")
	turn(f, t, id, 6, "")
	f.event(t, id, "text", map[string]any{"text": "marker", "final": true})
	calls := f.postedUntil(t, id, "marker")
	want := "the prompt|" +
		"The turn failed: the usage limit was reached.|" +
		"The turn failed: the agent is not logged in.|" +
		"The turn failed: the agent crashed.|" +
		"The turn failed: error (api_error).|marker"
	if got := texts(calls); got != want {
		t.Fatalf("posts\n%s\nwant\n%s", got, want)
	}
	if r := calls[len(calls)-1].Message.ReplyTo; r != nil {
		t.Fatalf("the final answer of a turn the agent started replies to %+v", r)
	}
}

func TestUnknownEventKindPostsNothing(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.fe.Outbox.Store.CheckEvent = nil // let the test store a kind of its own
	before := len(f.log.lines())
	f.showing(t)
	f.event(t, id, "made_up_kind", map[string]any{"text": "not to be shown"})
	f.event(t, id, "text", map[string]any{"text": "marker", "final": false})
	if got := texts(f.postedUntil(t, id, "marker")); got != "the prompt|marker" {
		t.Fatalf("posts %s", got)
	}
	if lines := f.log.lines()[before:]; len(lines) != 0 {
		t.Fatalf("log %q", lines)
	}
}

func TestReadingATurnAgainPostsNothing(t *testing.T) {
	f := newFront(t, workspaces{})
	id, trigger := f.started(t)
	stop := f.showing(t)
	turn(f, t, id, 1, trigger.Ref.ID)
	f.event(t, id, "text", map[string]any{"text": "Looking.", "final": false})
	f.event(t, id, "request", map[string]any{"request": "q1", "approval": false, "text": "Sure?"})
	ended(f, t, id, 1, "failure", "crashed")
	turn(f, t, id, 2, trigger.Ref.ID)
	f.event(t, id, "text", map[string]any{"text": "Done.", "final": true})
	first := texts(f.postedUntil(t, id, "Done."))
	stop()

	// Read everything again, from the start, with nothing remembered.
	if _, err := f.fe.Outbox.Store.DB.Exec(`DELETE FROM chat_cursors`); err != nil {
		t.Fatal(err)
	}
	f.showing(t)
	f.event(t, id, "text", map[string]any{"text": "marker", "final": false})
	if got := texts(f.postedUntil(t, id, "marker")); got != first+"|marker" {
		t.Fatalf("posts after reading again:\n%s\nwant\n%s|marker", got, first)
	}
}

// The prompt row is written after Start returns, so the final answer can
// be read before it exists; it still replies to the prompt.
func TestFinalAnswerWaitsForThePromptItRepliesTo(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) { fe.Outbox.MaxFailures = 1000 })
	st := f.fe.Outbox.Store
	st.DB.Exec(`CREATE TRIGGER lost AFTER INSERT ON chat_outbox BEGIN DELETE FROM chat_outbox WHERE id = NEW.id; END`)
	id, trigger := f.started(t)
	st.DB.Exec(`DROP TRIGGER lost`)
	f.showing(t)
	turn(f, t, id, 1, trigger.Ref.ID)
	pos := f.event(t, id, "text", map[string]any{"text": "Done.", "final": true})
	waitFor(t, "the answer to be tried", func() bool {
		var n int
		st.DB.QueryRow(`SELECT failures FROM chat_outbox WHERE event = ?`, pos).Scan(&n)
		return n > 0
	})
	f.fe.Message(trigger) // delivered again: the prompt row is written now
	calls := f.postedUntil(t, id, "Done.")
	if len(calls) != 2 || calls[1].Message.ReplyTo == nil || *calls[1].Message.ReplyTo != calls[0].Ref {
		t.Fatalf("posts %+v, want the prompt, then the answer replying to it", calls)
	}
}
