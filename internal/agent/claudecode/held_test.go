package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// heldTurnEnds ticks every 100 ms for d, and fails at any key, and unless
// the turn that a held input was to start ends once, "not delivered",
// retaining the input, either by its flag or the legacy unnamed end.
func heldTurnEnds(t *testing.T, c *codec, now time.Time, d time.Duration, when string) time.Time {
	t.Helper()
	heldID := ""
	if c.sent != nil {
		heldID = c.sent.id
	}
	var evs []agent.Event
	for end := now.Add(d); now.Before(end); now = now.Add(100 * time.Millisecond) {
		a := c.Tick(now)
		if len(a.Keys) != 0 {
			t.Fatalf("%s: keys %q", when, a.Keys)
		}
		evs = append(evs, a.Events...)
	}
	if len(evs) != 1 || evs[0].Kind != agent.TurnEnded || evs[0].Status != agent.Failed || evs[0].Reason != agent.NotDelivered || (evs[0].Input != "" && (!evs[0].RetainInput || evs[0].Input != heldID)) {
		t.Fatalf("%s: events %+v; want the turn's end, not delivered, retaining its input", when, evs)
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

// An Enter pressed just as the agent's own turn started, and not reported,
// is pressed again after that turn.
func TestHeldInputWhoseEnterWasLost(t *testing.T) {
	c := newTestCodec()
	heldTurn(t, c)
	now, n := enters(t, c, time.Now(), time.Second)
	if n != 1 {
		t.Fatalf("Enter pressed %d times after the paste, want once", n)
	}
	decodeFile(t, c, "07", held+"07-UserPromptSubmit.json")
	now = quiet(t, c, now, 2*DefaultInputTimeout, "during the own turn")
	decodeFile(t, c, "11", held+"11-Stop.json")
	if _, n = enters(t, c, now, 2*time.Second); n != 1 {
		t.Fatalf("Enter pressed %d times after the own turn, want once", n)
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
// turn to end: the rebuilt codec waits for its prompt, without Enter.
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
	if _, n := enters(t, c, now, 2*time.Second); n != 0 {
		t.Fatalf("Enter pressed %d times after reconnect, want none", n)
	}
	a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json")
	if summary(a) != "turn(caller)" || a.Events[0].Input != "in-7" {
		t.Fatalf("the message's prompt: %s", summary(a))
	}
}

// A reconnect holds both legacy unsafe pastes and current pastes: it cannot
// prove their Enter arrived. No key is sent after an unrelated turn. Only
// the matching prompt releases each input; other prompts leave it held.
func TestOlderPasteIsNotSubmittedAfterARestart(t *testing.T) {
	for _, tc := range []struct {
		text    string
		own     bool // the capture's next prompt, 12, is the input's own
		current bool // this hataori pasted it
	}{
		{"\u001b[31m!id", false, false},
		{"#x", false, false},
		{heldText + "\r", true, false},      // an older paste with a carriage return
		{"\u0007" + heldText, false, false}, // which this one pastes as the next prompt's text
		{heldText, true, false},
		{"/add-dir x", false, false},
		{"\u200b/add-dir x", false, false},
		{"\u001b[31m!id", false, true},
		{"#x", false, true},
	} {
		c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
		for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop", "07-UserPromptSubmit"} {
			decodeFile(t, c, f, held+f+".json")
		}
		if a, _ := c.Input(agent.Input{ID: "in-7", Text: tc.text, Written: true, Earlier: !tc.current}); len(a.Keys) != 0 {
			t.Fatalf("%q: written again: %q", tc.text, a.Keys)
		}
		now := quiet(t, c, time.Now(), 2*DefaultInputTimeout, "during the own turn")
		decodeFile(t, c, "11", held+"11-Stop.json")
		now, n := enters(t, c, now, 2*time.Second)
		if n != 0 {
			t.Errorf("%q: Enter pressed %d times after reconnect, want none", tc.text, n)
		}
		now = heldTurnEnds(t, c, now, 2*DefaultInputTimeout, "after the own turn")
		a, _ := c.Input(agent.Input{ID: "in-8", Text: "list the threads"})
		if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered || a.Events[0].Input != "in-8" {
			t.Fatalf("%q: the next input: keys %q, events %+v", tc.text, a.Keys, a.Events)
		}
		now = quiet(t, c, now, 2*DefaultInputTimeout, "after the next input")
		a = decodeFile(t, c, "12", held+"12-UserPromptSubmit.json")
		decodeFile(t, c, "13", held+"13-Stop.json")
		if tc.own {
			// The box was submitted: input is pasted again.
			if summary(a) != "turn(caller)" || a.Events[0].Input != "in-7" {
				t.Fatalf("%q: its prompt: %s", tc.text, summary(a))
			}
			if a, _ := c.Input(agent.Input{ID: "in-9", Text: "list the threads"}); len(a.Keys) != 1 || a.Keys[0].Paste != "list the threads" {
				t.Fatalf("%q: after the box was submitted: keys %q, events %+v", tc.text, a.Keys, a.Events)
			}
			continue
		}
		quiet(t, c, now, 2*DefaultInputTimeout, "after another prompt")
		a, _ = c.Input(agent.Input{ID: "in-9", Text: "list the threads"})
		if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered {
			t.Fatalf("%q: after another prompt: keys %q, events %+v", tc.text, a.Keys, a.Events)
		}
	}
}

// Of two inputs that may be in the box after a restart, the prompt of one
// releases that one only: nothing is pasted while the other may be there.
func TestOnlyItsOwnPromptReleasesAnInput(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop", "07-UserPromptSubmit"} {
		decodeFile(t, c, f, held+f+".json")
	}
	for _, in := range []agent.Input{{ID: "in-6", Text: heldText + "\r"}, {ID: "in-7", Text: "#x"}} {
		in.Written, in.Earlier = true, true
		if a, _ := c.Input(in); len(a.Keys) != 0 {
			t.Fatalf("%s: written again: %q", in.ID, a.Keys)
		}
	}
	decodeFile(t, c, "11", held+"11-Stop.json")
	if a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json"); summary(a) != "turn(caller)" || a.Events[0].Input != "in-6" {
		t.Fatalf("the prompt: %s", summary(a))
	}
	decodeFile(t, c, "13", held+"13-Stop.json")
	a, _ := c.Input(agent.Input{ID: "in-8", Text: "list the threads"})
	if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered || a.Events[0].Input != "in-8" {
		t.Fatalf("the next input: keys %q, events %+v", a.Keys, a.Events)
	}
	quiet(t, c, time.Now(), 2*DefaultInputTimeout, "after it")
}

// An input that this hataori pasted before a restart, after which what an
// older hataori pasted may be in the box, gets no Enter either: after the
// input timeout its turn ends as not delivered, retaining it. A later
// matching prompt releases it, while the older paste stays held (#837).
func TestOwnInputBeforeAnOlderPasteEnds(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop", "07-UserPromptSubmit"} {
		decodeFile(t, c, f, held+f+".json")
	}
	for _, in := range []agent.Input{{ID: "in-6", Text: heldText, Written: true}, {ID: "in-7", Text: "#x", Written: true, Earlier: true}} {
		if a, _ := c.Input(in); len(a.Keys) != 0 {
			t.Fatalf("%s: written again: %q", in.ID, a.Keys)
		}
	}
	decodeFile(t, c, "11", held+"11-Stop.json")
	var evs []agent.Event
	now := time.Now()
	for end := now.Add(2 * DefaultInputTimeout); now.Before(end); now = now.Add(100 * time.Millisecond) {
		a := c.Tick(now)
		if len(a.Keys) != 0 {
			t.Fatalf("keys %q while an older paste may be in the box", a.Keys)
		}
		evs = append(evs, a.Events...)
	}
	if len(evs) != 1 || evs[0].Kind != agent.TurnEnded || evs[0].Reason != agent.NotDelivered || evs[0].Input != "in-6" {
		t.Fatalf("events %+v; want the end of in-6's turn, not delivered", evs)
	}
	a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json")
	if len(a.Events) != 1 || a.Events[0].Input != "in-6" {
		t.Fatalf("confirmed original paste: %+v", a.Events)
	}
	if !slices.Equal(c.leftover, []string{"in-7"}) {
		t.Errorf("held %q, want in-7", c.leftover)
	}
}

// The end of the session releases what may be in the box: after a /clear in
// the window, which was submitted from it, input is pasted again. The input
// held, which got no Enter, ends with the session as not delivered.
func TestClearReleasesAnOlderPaste(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	decodeFile(t, c, "01", "clear/hooks/01-SessionStart.json")
	if a, _ := c.Input(agent.Input{ID: "in-7", Text: "#x", Written: true, Earlier: true}); len(a.Keys) != 0 {
		t.Fatalf("written again: %q", a.Keys)
	}
	if a, _ := c.Input(agent.Input{ID: "in-8", Text: "list the threads"}); len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered {
		t.Fatalf("while held: keys %q, events %+v", a.Keys, a.Events)
	}
	var ended []string
	for _, e := range decodeFile(t, c, "04", "clear/hooks/04-SessionEnd.json").Events {
		if e.Kind == agent.TurnEnded {
			ended = append(ended, e.Input+" "+string(e.Reason))
		}
	}
	if !slices.Equal(ended, []string{"in-7 " + string(agent.NotDelivered)}) {
		t.Fatalf("at the session's end: ended %q, want the held input", ended)
	}
	decodeFile(t, c, "05", "clear/hooks/05-SessionStart.json")
	if a, _ := c.Input(agent.Input{ID: "in-9", Text: "list the threads"}); len(a.Keys) == 0 || a.Keys[0].Paste != "list the threads" || len(a.Events) != 0 {
		t.Fatalf("after the clear: keys %q, events %+v; want the next input pasted", a.Keys, a.Events)
	}
}

// The end of the session, while an input is held, ends each input that was
// pasted and not reported as not delivered, naming it, so that the caller
// does not keep it: the one waited for first, whose turn runs, whether it is
// held or this hataori's, then each other, held or not, also after the turn
// ended after the input timeout. A later prompt the same as what was pasted
// for it is not taken for it, and no turn ends after (#914).
func TestClearEndsTheHeldInputs(t *testing.T) {
	held := agent.Input{ID: "in-7", Text: "&y", Earlier: true}
	for _, tc := range []struct {
		name          string
		first, second agent.Input
		turnTimedOut  bool
	}{
		{"held, its turn ended", agent.Input{ID: "in-6", Text: "#x", Earlier: true}, held, true},
		{"held, waited for", agent.Input{ID: "in-6", Text: "#x", Earlier: true}, held, false},
		{"this hataori's, waited for", agent.Input{ID: "in-6", Text: "#x"}, held, false},
		{"held, then one not held", agent.Input{ID: "in-6", Text: "#x", Earlier: true}, agent.Input{ID: "in-7", Text: "list the threads", Earlier: true}, false},
	} {
		c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
		decodeFile(t, c, "01", "clear/hooks/01-SessionStart.json")
		for _, in := range []agent.Input{tc.first, tc.second} {
			in.Written = true
			if a, _ := c.Input(in); len(a.Keys) != 0 || len(a.Events) != 0 {
				t.Fatalf("%s: %s: keys %q, events %+v", tc.name, in.ID, a.Keys, a.Events)
			}
		}
		now := time.Now()
		if tc.turnTimedOut {
			now = heldTurnEnds(t, c, now, 2*DefaultInputTimeout, tc.name)
		}
		var ended []string
		for _, e := range decodeFile(t, c, "04", "clear/hooks/04-SessionEnd.json").Events {
			if e.Kind == agent.TurnEnded {
				ended = append(ended, e.Input+" "+string(e.Reason))
			}
		}
		if want := []string{"in-6 " + string(agent.NotDelivered), "in-7 " + string(agent.NotDelivered)}; !slices.Equal(ended, want) {
			t.Fatalf("%s: at the session's end: ended %q, want %q", tc.name, ended, want)
		}
		decodeFile(t, c, "05", "clear/hooks/05-SessionStart.json")
		for _, prompt := range []string{tc.first.Text, tc.second.Text} {
			if by, id := c.starter(prompt); by == agent.ByCaller {
				t.Errorf("%s: the prompt %q counts as input %q, which ended", tc.name, prompt, id)
			}
		}
		for end := now.Add(2 * DefaultInputTimeout); now.Before(end); now = now.Add(100 * time.Millisecond) {
			if a := c.Tick(now); len(a.Events) != 0 {
				t.Fatalf("%s: after the session's end: %+v", tc.name, a.Events)
			}
		}
	}
}

// A session end removes the editor and completes a reconnected input that
// remained unconfirmed (#837).
func TestClearEndsAReconnectedUnconfirmedInput(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	decodeFile(t, c, "01", "clear/hooks/01-SessionStart.json")
	c.Input(agent.Input{ID: "in-6", Text: "list the threads", Written: true})
	var ended []agent.Event
	for _, e := range decodeFile(t, c, "04", "clear/hooks/04-SessionEnd.json").Events {
		if e.Kind == agent.TurnEnded {
			ended = append(ended, e)
		}
	}
	if len(ended) != 1 || ended[0].Input != "in-6" || ended[0].RetainInput || ended[0].Reason != agent.NotDelivered {
		t.Fatalf("session end: %+v", ended)
	}
	if c.sent != nil || c.holdsBox() {
		t.Fatalf("box retained after clear: %+v", c.sent)
	}
}

// A task notification that starts a turn does not take the box's text, also
// when it is the text of the input that may be there: the turn is the
// agent's, the input does not end, also when the turn it was to start ends
// after the input timeout, and nothing is pasted after it, nor at the end of
// the notification's turn.
func TestNotificationDoesNotTakeAnOlderPaste(t *testing.T) {
	var p payload
	b, err := os.ReadFile(filepath.Join("testdata/2.1.322", held+"07-UserPromptSubmit.json"))
	if err != nil || json.Unmarshal(b, &p) != nil || !notification(p.Prompt) {
		t.Fatalf("the notification's prompt: %v", err)
	}
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop"} {
		decodeFile(t, c, f, held+f+".json")
	}
	if a, _ := c.Input(agent.Input{ID: "in-7", Text: p.Prompt, Written: true, Earlier: true}); len(a.Keys) != 0 {
		t.Fatalf("written again: %q", a.Keys)
	}
	if a := decodeFile(t, c, "07", held+"07-UserPromptSubmit.json"); !strings.HasPrefix(summary(a), "turn(agent)") || a.Events[0].Input != "" {
		t.Fatalf("the notification: %s, input %q", summary(a), a.Events[0].Input)
	}
	decodeFile(t, c, "11", held+"11-Stop.json")
	a, _ := c.Input(agent.Input{ID: "in-8", Text: "list the threads"})
	if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered {
		t.Fatalf("the next input: keys %q, events %+v", a.Keys, a.Events)
	}
	now := heldTurnEnds(t, c, time.Now(), 2*DefaultInputTimeout, "after it")
	quiet(t, c, now, 2*DefaultInputTimeout, "after the turn's end")
}

// An input without an ID that may be in the box is not released by a
// prompt that is no input's, which has no ID either.
func TestHeldInputWithoutAnIDIsNotReleasedByAnotherPrompt(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop", "07-UserPromptSubmit"} {
		decodeFile(t, c, f, held+f+".json")
	}
	if a, _ := c.Input(agent.Input{Text: "#x", Written: true, Earlier: true}); len(a.Keys) != 0 {
		t.Fatalf("written again: %q", a.Keys)
	}
	decodeFile(t, c, "11", held+"11-Stop.json")
	if a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json"); summary(a) != "turn(terminal)" {
		t.Fatalf("the prompt: %s", summary(a))
	}
	decodeFile(t, c, "13", held+"13-Stop.json")
	a, _ := c.Input(agent.Input{ID: "in-8", Text: "list the threads"})
	if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered || a.Events[0].Input != "in-8" {
		t.Fatalf("the next input: keys %q, events %+v", a.Keys, a.Events)
	}
}
