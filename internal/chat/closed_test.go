package chat

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface/fake"
)

// A message in a closed thread reaches no agent and gets one line, also
// when the driver delivers it twice; a guest's message gets nothing.
func TestMessageInAClosedThreadGetsOneLine(t *testing.T) {
	f := newFront(t, workspaces{})
	f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
	ths := f.threads()
	if len(ths) != 1 {
		t.Fatalf("%d threads", len(ths))
	}
	th := ths[0].Opened
	waitFor(t, "the prompt", func() bool { return len(postsIn(f.driver, th)) == 1 })
	// Closed as CloseThread leaves it; the front end only reads the state.
	db := f.fe.Outbox.Store.DB
	for _, q := range []string{`UPDATE sessions SET state = 'closed'`, `UPDATE threads SET state = 'closed'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var inputs int
	countInputs := func() int {
		var n int
		db.QueryRow(`SELECT count(*) FROM events WHERE kind IN ('input', 'turn_started')`).Scan(&n)
		return n
	}
	inputs = countInputs()

	f.deliver(th, fake.Person("guest-1"), "let me in")
	in := f.deliver(th, fake.Webhook(hook), "are you there")
	f.fe.Message(in)
	waitFor(t, "the closed line", func() bool { return len(postsIn(f.driver, th)) == 2 })
	time.Sleep(50 * time.Millisecond)
	posts := postsIn(f.driver, th)
	if len(posts) != 2 || posts[1] != ClosedLine {
		t.Fatalf("posts %q, want the prompt and one closed line", posts)
	}
	for _, c := range f.driver.Calls(th) {
		if c.Op == fake.OpPost && c.Message.Text == ClosedLine && (c.Message.ReplyTo == nil || c.Message.ReplyTo.ID != in.Ref.ID) {
			t.Fatalf("the closed line replies to %v, want %s", c.Message.ReplyTo, in.Ref.ID)
		}
	}
	if countInputs() != inputs || len(f.runner.all()) != 1 {
		t.Fatal("a message in a closed thread reached the agent")
	}
}
