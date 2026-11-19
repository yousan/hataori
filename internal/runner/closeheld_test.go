package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

// A message sent while a turn runs is held until the turn ends. Closing
// the thread after that turn ended, before the held message's turn has
// started, is refused and loses nothing: the thread stays idle, the
// message's row stays, and its turn then runs. Once it has, the close is
// accepted.
func TestCloseDoesNotDropAHeldMessage(t *testing.T) {
	f := newFixture(t, options{instructions: "input-delay 2s"})
	f.start("tool wait 1s\nreply one")
	waitFor(t, "the first tool", 20*time.Second, f.has("tool_started", 1))
	api := f.core.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all | control.OpClose})
	if err := api.Send(context.Background(), thread, "m2", control.Input{Text: "reply two"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the end of the first turn", 20*time.Second, f.has("turn_ended", 1))

	count := func(q string) int {
		var n int
		f.st.DB.QueryRow(q).Scan(&n)
		return n
	}
	threadState := func() string {
		var s string
		f.st.DB.QueryRow(`SELECT state FROM threads`).Scan(&s)
		return s
	}
	err := api.CloseThread(context.Background(), thread)
	pending := count(`SELECT count(*) FROM pending_inputs`)
	notDelivered := count(`SELECT count(*) FROM events WHERE kind = 'turn_ended' AND json_extract(data, '$.reason') = 'not delivered'`)
	if err == nil && threadState() == "closed" && pending > 0 && notDelivered == 0 {
		t.Fatalf("the thread was closed while the accepted message %q waited for its turn: %d pending input left, never delivered and not reported", "reply two", pending)
	}
	if !errors.Is(err, session.ErrRefused) {
		t.Fatalf("close with a held message: %v, want refused", err)
	}
	if s := threadState(); s != "idle" && s != "running" || pending != 1 {
		t.Fatalf("after the refused close: thread %s, %d pending inputs", s, pending)
	}

	waitFor(t, "the held message's turn", 20*time.Second, func() bool {
		return count(`SELECT count(*) FROM events WHERE kind = 'turn_ended'`) >= 2 &&
			count(`SELECT count(*) FROM pending_inputs`) == 0 && threadState() == "idle"
	})
	if n := count(`SELECT count(*) FROM events WHERE kind = 'turn_ended' AND json_extract(data, '$.outcome') = 'completion'`); n != 2 {
		t.Fatalf("%d turns completed, want 2", n)
	}
	if err := api.CloseThread(context.Background(), thread); err != nil {
		t.Fatalf("close once the held message's turn ran: %v", err)
	}
	if s := threadState(); s != "closed" {
		t.Fatalf("thread %s after the close", s)
	}
}
