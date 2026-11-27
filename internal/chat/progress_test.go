package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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
	for _, c := range f.driver.Calls(f.conv(id)) {
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

// waitLines waits for the expected calls and their persisted acknowledgements.
// Failed or dropped rows are not visible calls and need not finish first.
func (f *front) waitLines(t *testing.T, id control.ThreadID, want ...string) {
	t.Helper()
	conv := f.conv(id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := f.lines(id)
		var sent int
		if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM chat_outbox
			WHERE client = ? AND channel = ? AND thread = ? AND state = 'sent'
			AND (part >= ? OR part = ?)`, f.fe.Outbox.Client, conv.Channel, conv.Thread, linePart, deletePart).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, "|") == strings.Join(want, "|") && sent == len(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress line calls\n%s\nwant\n%s\n%d acknowledged rows, want %d", strings.Join(got, "\n"), strings.Join(want, "\n"), sent, len(want))
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

// heldProgressReply makes a progress call visible before its sender can
// record the successful reply. Other driver calls complete normally.
type heldProgressReply struct {
	*fake.Driver
	op      fake.Op
	called  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *heldProgressReply) hold(ctx context.Context, op fake.Op) error {
	var err error
	if op == d.op {
		d.once.Do(func() {
			close(d.called)
			select {
			case <-d.release:
			case <-ctx.Done():
				err = ctx.Err()
			}
		})
	}
	return err
}

func (d *heldProgressReply) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	ref, err := d.Driver.Post(ctx, conv, key, m)
	if err == nil && strings.HasPrefix(m.Text, "Still working") {
		err = d.hold(ctx, fake.OpPost)
	}
	return ref, err
}

func (d *heldProgressReply) Edit(ctx context.Context, ref surface.MessageRef, m surface.Message) error {
	if err := d.Driver.Edit(ctx, ref, m); err != nil {
		return err
	}
	return d.hold(ctx, fake.OpEdit)
}

func (d *heldProgressReply) Delete(ctx context.Context, ref surface.MessageRef) error {
	if err := d.Driver.Delete(ctx, ref); err != nil {
		return err
	}
	return d.hold(ctx, fake.OpDelete)
}

// A visible post, edit or delete is not yet an acknowledged outbox row.
// The next clock step must wait for that acknowledgement, even when the
// reader's cursor has already caught up. synctest makes the interleaving
// deterministic without a real-time allowance for an unfinished send.
func TestProgressClockWaitsForOutboxAcknowledgement(t *testing.T) {
	for _, op := range []fake.Op{fake.OpPost, fake.OpEdit, fake.OpDelete} {
		t.Run(op.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				heldAt := t0.Add(91 * time.Second)
				c := &clock{t: heldAt}
				d := &heldProgressReply{op: op, called: make(chan struct{}), release: make(chan struct{})}
				f := newFront(t, workspaces{}, withClock(c), func(fe *FrontEnd) {
					d.Driver = fe.Driver.(*fake.Driver)
					fe.Outbox.Driver = d
				})
				var release sync.Once
				unblock := func() { release.Do(func() { close(d.release) }) }
				t.Cleanup(unblock)
				id, _ := f.started(t)
				f.showing(t)
				f.advance(t, c, 91) // cursor convergence alone must not release the next step
				conv := f.conv(id)
				const text = "Still working: 1m 31s without output."
				row := Row{Event: 1 << 40, Part: linePart, Conv: conv, Decoration: true, Message: surface.Message{Text: text}}
				want := []string{"post " + text}
				write := func() {
					t.Helper()
					if err := f.fe.Outbox.Write(context.Background(), row); err != nil {
						t.Fatal(err)
					}
				}
				state := func() (string, int64) {
					t.Helper()
					var state string
					var sent int64
					if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT state, coalesce(sent_at, 0) FROM chat_outbox
						WHERE client = ? AND channel = ? AND thread = ? AND event = ? AND part = ?`,
						f.fe.Name, conv.Channel, conv.Thread, row.Event, row.Part).Scan(&state, &sent); err != nil {
						t.Fatal(err)
					}
					return state, sent
				}
				write()
				if op != fake.OpPost {
					waitFor(t, "the initial progress post acknowledgement", func() bool { s, _ := state(); return s == "sent" })
					row.Target = &RowKey{Event: row.Event, Part: linePart}
					if op == fake.OpEdit {
						row.Op, row.Part = Edit, linePart+1
						row.Message.Text = "Still working: edited."
						want = append(want, "edit "+row.Message.Text)
					} else {
						row.Op, row.Event, row.Part = Delete, row.Event+1, deletePart
						want = append(want, "delete")
					}
					write()
				}
				<-d.called // visible on the fake service, but the reply is held
				if s, _ := state(); s != "pending" {
					t.Fatalf("held reply row is %s, want pending", s)
				}
				advanced := make(chan struct{})
				go func() {
					defer close(advanced)
					f.waitLines(t, id, want...)
					f.advance(t, c, 104)
				}()
				synctest.Wait()
				if now := c.now(); !now.Equal(heldAt) {
					t.Errorf("clock advanced to %s before the outbox acknowledged the %s", now, op)
				}
				unblock()
				<-advanced
				waitFor(t, "the held progress acknowledgement", func() bool { s, _ := state(); return s == "sent" })
				if _, sent := state(); sent != heldAt.UnixMilli() {
					t.Errorf("sent_at = %s, want the held clock %s", time.UnixMilli(sent).UTC(), heldAt)
				}
			})
		})
	}
}

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
	f.advance(t, c, 105) // first edit slot, but only 14 seconds since the post: no edit
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
