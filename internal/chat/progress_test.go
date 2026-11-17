package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

var t0 = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

func withClock(c *clock) func(*FrontEnd) {
	return func(fe *FrontEnd) {
		fe.Now, fe.Tick, fe.Outbox.Now = c.now, 2*time.Millisecond, c.now
	}
}

// at appends an event of thread id at the time t0+s.
func (f *front) at(t *testing.T, id control.ThreadID, s int, kind string, data map[string]any) {
	t.Helper()
	data["session"] = 1
	b, _ := json.Marshal(data)
	err := f.fe.Outbox.Store.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Append(context.Background(), control.Event{Thread: id, Kind: control.EventKind(kind), Data: b,
			Time: t0.Add(time.Duration(s) * time.Second)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// lines returns the calls on progress lines in the thread, and how many
// lines are shown now.
func (f *front) lines(id control.ThreadID) (calls []string, shown int) {
	lines := map[string]bool{}
	for _, c := range f.driver.Calls(conv(id)) {
		switch {
		case c.Op == fake.OpPost && strings.HasPrefix(c.Message.Text, "Still working"):
			lines[c.Ref.ID] = true
			calls = append(calls, "post "+c.Message.Text)
		case c.Op == fake.OpEdit && lines[c.Ref.ID]:
			calls = append(calls, "edit "+c.Message.Text)
		case c.Op == fake.OpDelete && lines[c.Ref.ID]:
			delete(lines, c.Ref.ID)
			calls = append(calls, "delete")
		}
	}
	return calls, len(lines)
}

func (f *front) waitLines(t *testing.T, id control.ThreadID, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := f.lines(id)
		if strings.Join(got, "|") == strings.Join(want, "|") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress line calls\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// advance waits until the reader has read every stored event, then sets
// the clock to t0+s.
func (f *front) advance(t *testing.T, c *clock, s int) {
	t.Helper()
	waitFor(t, "the reader to catch up", func() bool {
		var head, pos uint64
		f.fe.Outbox.Store.DB.QueryRow(`SELECT coalesce(max(pos), 0) FROM events`).Scan(&head)
		f.fe.Outbox.Store.DB.QueryRow(`SELECT coalesce(max(pos), 0) FROM chat_cursors`).Scan(&pos)
		return pos >= head
	})
	c.set(t0.Add(time.Duration(s) * time.Second))
}

// settle lets the reader tick a few times at the clock's time.
func settle() { time.Sleep(30 * time.Millisecond) }

func TestProgressLineAppearsIsEditedAndRemoved(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 10, "tool_started", map[string]any{"id": "t1", "tool": "Bash"})
	f.advance(t, c, 89)
	settle()
	f.waitLines(t, id)
	f.advance(t, c, 91)
	f.waitLines(t, id, "post Still working: 1m 31s without output. Running Bash.")
	f.advance(t, c, 104) // within 15 seconds of the post: no edit
	settle()
	f.waitLines(t, id, "post Still working: 1m 31s without output. Running Bash.")
	f.advance(t, c, 106)
	f.waitLines(t, id, "post Still working: 1m 31s without output. Running Bash.",
		"edit Still working: 1m 46s without output. Running Bash.")
	// Output ends the stretch; the next one gets a line of its own, and the
	// end of the turn removes it.
	f.at(t, id, 110, "tool_ended", map[string]any{"id": "t1", "tool": "Bash"})
	f.at(t, id, 120, "text", map[string]any{"text": "Halfway.", "final": false})
	f.waitLines(t, id, "post Still working: 1m 31s without output. Running Bash.",
		"edit Still working: 1m 46s without output. Running Bash.", "delete")
	f.advance(t, c, 211)
	f.waitLines(t, id, "post Still working: 1m 31s without output. Running Bash.",
		"edit Still working: 1m 46s without output. Running Bash.", "delete",
		"post Still working: 1m 31s without output.")
	f.at(t, id, 230, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 0})
	f.waitLines(t, id, "post Still working: 1m 31s without output. Running Bash.",
		"edit Still working: 1m 46s without output. Running Bash.", "delete",
		"post Still working: 1m 31s without output.", "delete")
	if _, shown := f.lines(id); shown != 0 {
		t.Fatalf("%d lines left", shown)
	}
	f.advance(t, c, 400)
	settle()
	if got, _ := f.lines(id); len(got) != 5 {
		t.Fatalf("calls after the turn ended: %v", got)
	}
}

func TestFailingProgressLineNeverHoldsBackTheTurn(t *testing.T) {
	c := &clock{t: t0}
	fl := newFlaky()
	fl.setFail(func(_ surface.Conversation, m surface.Message) bool {
		return strings.HasPrefix(m.Text, "Still working")
	})
	f := newFront(t, workspaces{}, withClock(c), func(fe *FrontEnd) { fe.Driver, fe.Outbox.Driver = fl, fl })
	f.driver = fl.Driver
	id, trigger := f.started(t)
	f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.advance(t, c, 120)
	settle()
	f.at(t, id, 130, "text", map[string]any{"text": "Done.", "final": true})
	f.at(t, id, 131, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 0})
	f.postedUntil(t, id, "Done.")
	// And edits and deletes that fail.
	f.driver.Fail(fake.OpEdit, 1000, nil)
	f.driver.Fail(fake.OpDelete, 1000, nil)
	fl.setFail(nil)
	f.at(t, id, 200, "turn_started", map[string]any{"turn": 2, "by": "caller", "key": trigger.Ref.ID})
	f.advance(t, c, 320)
	f.waitLines(t, id, "post Still working: 2m 00s without output.")
	f.advance(t, c, 340)
	settle()
	f.at(t, id, 345, "text", map[string]any{"text": "Done again.", "final": true})
	f.postedUntil(t, id, "Done again.")
}

func TestRestartDuringAQuietStretchUsesTheSameLine(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	stop := f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 5, "tool_started", map[string]any{"id": "t1", "tool": "Bash"})
	f.fe.Outbox.Store.DB.Exec(`UPDATE threads SET state = 'running'`)
	f.advance(t, c, 95)
	f.waitLines(t, id, "post Still working: 1m 35s without output. Running Bash.")
	stop()

	f.advance(t, c, 125)
	f.showing(t) // a new reader, with nothing remembered
	f.waitLines(t, id, "post Still working: 1m 35s without output. Running Bash.",
		"edit Still working: 2m 05s without output. Running Bash.")
	f.at(t, id, 130, "text", map[string]any{"text": "Out.", "final": false})
	f.waitLines(t, id, "post Still working: 1m 35s without output. Running Bash.",
		"edit Still working: 2m 05s without output. Running Bash.", "delete")
	if _, shown := f.lines(id); shown != 0 {
		t.Fatalf("%d lines left", shown)
	}
}

// Output, or the end of the turn, while the front end is down still
// removes the line.
func TestOutputWhileDownRemovesTheLineAfterARestart(t *testing.T) {
	for _, tc := range []struct {
		name, kind, state string
		data              map[string]any
	}{
		{"text", "text", "running", map[string]any{"text": "While down.", "final": false}},
		{"turn ended", "turn_ended", "idle", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: t0}
			f := newFront(t, workspaces{}, withClock(c))
			id, trigger := f.started(t)
			stop := f.showing(t)
			f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
			f.advance(t, c, 95)
			f.waitLines(t, id, "post Still working: 1m 35s without output.")
			stop()
			f.at(t, id, 100, tc.kind, tc.data)
			f.fe.Outbox.Store.DB.Exec(`UPDATE threads SET state = ?`, tc.state)
			c.set(t0.Add(110 * time.Second))
			f.showing(t)
			f.waitLines(t, id, "post Still working: 1m 35s without output.", "delete")
		})
	}
}

// While the line of the stretch before cannot be deleted, no second one
// is posted.
func TestNoSecondLineWhileTheFirstIsNotDeleted(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.advance(t, c, 95)
	f.waitLines(t, id, "post Still working: 1m 35s without output.")
	f.driver.Fail(fake.OpDelete, 1000, nil)
	f.at(t, id, 100, "text", map[string]any{"text": "Out.", "final": false})
	f.advance(t, c, 200)
	settle()
	f.postedUntil(t, id, "Out.")
	f.waitLines(t, id, "post Still working: 1m 35s without output.")
	// Nor in the next turn, nor after a stretch that had no line.
	f.at(t, id, 300, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 0})
	f.at(t, id, 310, "turn_started", map[string]any{"turn": 2, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 450, "text", map[string]any{"text": "Out again.", "final": false})
	f.advance(t, c, 600)
	settle()
	f.postedUntil(t, id, "Out again.")
	f.waitLines(t, id, "post Still working: 1m 35s without output.")
	f.driver.Fail(fake.OpDelete, 0, nil)
	f.waitLines(t, id, "post Still working: 1m 35s without output.", "delete", "post Still working: 2m 30s without output.")
}

// Text that shows nothing is not output: it neither ends a stretch nor
// removes the line.
func TestBlankTextIsNotOutput(t *testing.T) {
	if visible(control.Event{Kind: "text", Data: json.RawMessage(`{"text":" \n","final":false}`)}) {
		t.Fatal("blank text counts as output")
	}
}
