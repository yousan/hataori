package chat

import (
	"context"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// A second press of a Stop button, also delivered again, stops nothing
// more: the agent is interrupted once.
func TestSecondPressOfAStopButtonStopsNothingMore(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	var turn uint64
	if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT pos FROM events WHERE thread_id = ? AND kind = 'turn_started' ORDER BY pos DESC LIMIT 1`,
		int64(id)).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	token := stopChoice(id, turn).Token
	press := surface.Selection{Ref: surface.MessageRef{Conv: f.conv(id), ID: "line-1"}, Author: fake.Person(owner), Token: token, Tokens: []string{token}}
	f.fe.Choice(press)
	f.fe.Choice(press)
	f.runner.mu.Lock()
	stops := f.runner.stops
	f.runner.mu.Unlock()
	if stops != 1 {
		t.Fatalf("%d interrupts, want 1", stops)
	}
}

// An edit that failed and is tried again after a later edit of the same
// message was sent is dropped: the progress line's button, taken away by
// the later edit, does not come back.
func TestAnOlderEditIsNotSentAfterANewerOne(t *testing.T) {
	f := newFront(t, workspaces{})
	conv := surface.Conversation{Channel: channel, Thread: "t1"}
	line := RowKey{Event: 1 << 40, Part: linePart}
	stop := stopChoice(1, 1)
	if err := f.fe.Outbox.Write(context.Background(), Row{Event: line.Event, Part: line.Part, Conv: conv, Decoration: true,
		Message: surface.Message{Text: "Still working.", Choices: []surface.Choice{stop}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the line", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), line.Event, line.Part); return ok })
	f.driver.Fail(fake.OpEdit, 1, nil) // the timed edit fails once
	timed := Row{Event: line.Event, Part: linePart + 1, Conv: conv, Decoration: true, Op: Edit, Target: &line,
		Message: surface.Message{Text: "Still working, later.", Choices: []surface.Choice{stop}}}
	stopping := Row{Event: line.Event, Part: stoppedPart, Conv: conv, Decoration: true, Op: Edit, Target: &line,
		Message: surface.Message{Text: stoppingText}}
	if err := f.fe.Outbox.Write(context.Background(), timed, stopping); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stopping edit", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), line.Event, stoppedPart); return ok })
	// Rows after it, posted, show that the queue went past the timed edit.
	if err := f.fe.Outbox.Write(context.Background(), row(1<<41, 0, conv, "marker")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the marker", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), 1<<41, 0); return ok })
	time.Sleep(100 * time.Millisecond)
	var edits []string
	for _, c := range f.driver.Calls(conv) {
		if c.Op == fake.OpEdit {
			edits = append(edits, c.Message.Text)
		}
	}
	if len(edits) != 1 || edits[0] != stoppingText {
		t.Fatalf("edits %q, want the stopping one only", edits)
	}
}
