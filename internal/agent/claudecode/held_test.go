package claudecode

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// held is a capture of a paste that was in the input box when a task
// notification started the agent's next turn: 01 to 05 the caller's turn,
// 07 to 11 the agent's own, 12 and 13 the pasted message's.
const (
	held     = "../2.1.331/held_notification/hooks/"
	heldText = "Reply with just: held message"
)

// heldTurn brings c to the end of the caller's turn of the capture, with
// the message given during it, and returns what that end wrote.
func heldTurn(t *testing.T, c *codec) Actions {
	decodeFile(t, c, "01", held+"01-SessionStart.json")
	decodeFile(t, c, "02", held+"02-UserPromptSubmit.json")
	if a, _ := c.Input(agent.Input{ID: "in-7", Text: heldText}); len(a.Keys) != 0 {
		t.Fatalf("written during a turn: %q", a.Keys)
	}
	a := decodeFile(t, c, "05", held+"05-Stop.json")
	if len(a.Keys) != 1 || a.Keys[0].Paste != heldText {
		t.Fatalf("at the end of the turn: keys %q, want the paste", a.Keys)
	}
	return a
}

// quiet ticks every 100 ms for d and fails at any key or event.
func quiet(t *testing.T, c *codec, now time.Time, d time.Duration, when string) time.Time {
	t.Helper()
	for end := now.Add(d); now.Before(end); now = now.Add(100 * time.Millisecond) {
		if a := c.Tick(now); len(a.Keys) != 0 || len(a.Events) != 0 {
			t.Fatalf("%s: keys %q, events %+v", when, a.Keys, a.Events)
		}
	}
	return now
}

// enters ticks every 100 ms for d, fails at any event, and returns how
// often Enter was pressed.
func enters(t *testing.T, c *codec, now time.Time, d time.Duration) (time.Time, int) {
	t.Helper()
	n := 0
	for end := now.Add(d); now.Before(end); now = now.Add(100 * time.Millisecond) {
		a := c.Tick(now)
		if len(a.Events) != 0 {
			t.Fatalf("events %+v while the input waits for its report", a.Events)
		}
		for _, k := range a.Keys {
			if k != confirm {
				t.Fatalf("key %q, want Enter", k)
			}
			n++
		}
	}
	return now, n
}

// A held message pasted at the end of a turn, as the CLI starts a turn of
// its own for a task notification, is submitted once that turn ended, once,
// as the caller's turn (#593).
func TestHeldInputWaitsForTheAgentsOwnTurn(t *testing.T) {
	c := newTestCodec()
	heldTurn(t, c)
	now := time.Now()
	c.Tick(now)
	if got := summary(decodeFile(t, c, "07", held+"07-UserPromptSubmit.json")); got != "turn(agent)" {
		t.Fatalf("the notification: %s", got)
	}
	now = quiet(t, c, now, 3*DefaultInputTimeout, "during the agent's own turn")
	if got := summary(decodeFile(t, c, "11", held+"11-Stop.json")); got != "text(final=true) end(completed,bg=0)" {
		t.Fatalf("the own turn's end: %s", got)
	}
	now, n := enters(t, c, now, 2*time.Second)
	if n != 1 {
		t.Fatalf("Enter pressed %d times after the own turn, want once", n)
	}
	a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json")
	if summary(a) != "turn(caller)" || a.Events[0].Input != "in-7" {
		t.Fatalf("the message's prompt: %s, input %+v", summary(a), a.Events)
	}
	decodeFile(t, c, "13", held+"13-Stop.json")
	quiet(t, c, now, 3*DefaultInputTimeout, "after the message's turn")
}

// Two of the agent's own turns in a row: Enter waits for the second too.
func TestHeldInputWaitsForASecondOwnTurn(t *testing.T) {
	c := newTestCodec()
	heldTurn(t, c)
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		decodeFile(t, c, "07"+id, held+"07-UserPromptSubmit.json")
		now = quiet(t, c, now, 2*DefaultInputTimeout, "during own turn "+id)
		decodeFile(t, c, "11"+id, held+"11-Stop.json")
		now = quiet(t, c, now, pasteSettle/2, "between the turns")
	}
	if _, n := enters(t, c, now, 2*time.Second); n != 1 {
		t.Fatalf("Enter pressed %d times, want once", n)
	}
}

// The input timeout starts over at the end of the agent's own turn: an
// input that is not reported then still ends "not delivered".
func TestHeldInputTimesOutAfterTheOwnTurn(t *testing.T) {
	c := newTestCodec()
	heldTurn(t, c)
	decodeFile(t, c, "07", held+"07-UserPromptSubmit.json")
	now := quiet(t, c, time.Now(), 2*DefaultInputTimeout, "during the own turn")
	decodeFile(t, c, "11", held+"11-Stop.json")
	now, _ = enters(t, c, now, DefaultInputTimeout)
	a := c.Tick(now.Add(time.Second))
	if len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered || a.Events[0].Input != "in-7" {
		t.Fatalf("after the timeout: %+v", a.Events)
	}
}

// A daemon restarted while the message waits in the box for the agent's own
// turn to end: the rebuilt codec presses Enter after it.
func TestHeldInputAcrossARestart(t *testing.T) {
	c := newTestCodec()
	for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop", "07-UserPromptSubmit"} {
		decodeFile(t, c, f, held+f+".json")
	}
	if a, _ := c.Input(agent.Input{ID: "in-7", Text: heldText, Written: true}); len(a.Keys) != 0 {
		t.Fatalf("written again: %q", a.Keys)
	}
	now := quiet(t, c, time.Now(), 2*DefaultInputTimeout, "during the own turn")
	decodeFile(t, c, "11", held+"11-Stop.json")
	if _, n := enters(t, c, now, 2*time.Second); n != 1 {
		t.Fatalf("Enter pressed %d times, want once", n)
	}
	a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json")
	if summary(a) != "turn(caller)" || a.Events[0].Input != "in-7" {
		t.Fatalf("the message's prompt: %s", summary(a))
	}
}
