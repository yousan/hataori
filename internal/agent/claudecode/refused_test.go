package claudecode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// handing stands for the terminal of a codec under test: it hands the keys
// over as the runner does, or refuses them.
type handing struct {
	refuse bool // nothing of the keys is handed over
	cut    bool // part of the keys may be
	taken  []string
}

func (w *handing) act(a agent.Actions) agent.Actions {
	switch {
	case len(a.Keys) == 0:
	case (w.refuse || w.cut) && a.KeysFailed != nil:
		a.KeysFailed(w.refuse)
	case w.refuse || w.cut:
	default:
		for _, k := range a.Keys {
			w.taken = append(w.taken, k.Paste+string(k.Key))
		}
	}
	return a
}

// tick ticks c every 200ms for d, and returns the events and the time after.
func (w *handing) tick(c *codec, now time.Time, d time.Duration) ([]agent.Event, time.Time) {
	var evs []agent.Event
	for end := now.Add(d); now.Before(end); now = now.Add(200 * time.Millisecond) {
		evs = append(evs, w.act(c.Tick(now)).Events...)
	}
	return evs, now
}

func (w *handing) input(t *testing.T, c *codec, id, text string) {
	t.Helper()
	a, err := c.Input(agent.Input{ID: id, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	w.act(a)
}

func (w *handing) want(t *testing.T, keys ...string) {
	t.Helper()
	if strings.Join(w.taken, "|") != strings.Join(keys, "|") {
		t.Fatalf("handed over %q, want %q", w.taken, keys)
	}
}

const enterKey = string(agent.Enter)

// An Enter that the window refused is pressed for its message once it takes
// keys again, however long that is; the next message waits for it, and is
// not submitted with it (#741).
func TestRefusedEnterIsPressedAgain(t *testing.T) {
	c, w := started(t), &handing{}
	first := prompt(t, "plain/hooks/02-UserPromptSubmit.json")
	w.input(t, c, "in-1", first)
	w.refuse = true
	evs, now := w.tick(c, time.Now(), 2*DefaultInputTimeout)
	if len(evs) != 1 || evs[0].Kind != agent.InputWaiting || evs[0].Input != "in-1" {
		t.Fatalf("waiting notice while the Enter is refused: %+v", evs)
	}
	w.input(t, c, "in-2", "the next")
	w.want(t, first)
	w.refuse = false
	_, now = w.tick(c, now, 10*time.Second)
	w.want(t, first, enterKey)
	if got := summary(decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json")); got != "turn(caller)" {
		t.Fatalf("the reported prompt: %s", got)
	}
	w.tick(c, now, 10*time.Second)
	w.want(t, first, enterKey)
}

// A paste that the window refused is pasted again, and no Enter is pressed
// for it before (#741).
func TestRefusedPasteIsPastedAgain(t *testing.T) {
	c, w := started(t), &handing{refuse: true}
	w.input(t, c, "in-1", "held up")
	w.input(t, c, "in-2", "the next")
	now := time.Now()
	asked := 0
	for i := 0; i < 100; i++ {
		a := w.act(c.Tick(now))
		if len(a.Keys) > 0 {
			asked++
			if a.Keys[0].Paste != "held up" || len(a.Inputs) != 1 || a.Inputs[0] != "in-1" {
				t.Fatalf("tried again with %q, inputs %q", a.Keys, a.Inputs)
			}
		}
		if len(a.Events) != 0 {
			t.Fatalf("events %+v before the timeout", a.Events)
		}
		now = now.Add(200 * time.Millisecond)
	}
	if asked < 3 || asked > 10 {
		t.Fatalf("tried %d times again in 20s", asked)
	}
	w.refuse = false
	w.tick(c, now, 8*time.Second)
	w.want(t, "held up", enterKey)
}

// A paste refused until the timeout ends its turn with that reason; nothing
// is submitted for it, and the next message goes alone (#741).
func TestPasteRefusedUntilTheTimeout(t *testing.T) {
	c, w := started(t), &handing{refuse: true}
	w.input(t, c, "in-1", "never taken")
	w.input(t, c, "in-2", "the next")
	now := time.Now()
	evs, now := w.tick(c, now, DefaultInputTimeout)
	if len(evs) != 0 {
		t.Fatalf("early: %+v", evs)
	}
	w.refuse = false
	a := w.act(c.Tick(now))
	if len(a.Events) != 1 || a.Events[0].Kind != agent.TurnEnded || a.Events[0].Reason != agent.NotTaken ||
		a.Events[0].Status != agent.Failed || a.Events[0].Input != "in-1" {
		t.Fatalf("after the timeout: %+v", a.Events)
	}
	w.tick(c, now, 10*time.Second)
	w.want(t, "the next", enterKey)
}

// No Enter is pressed for a paste that may have been handed over in part,
// and it is not pasted again: its turn ends as not delivered (#741).
func TestNoEnterForAnUnsurePaste(t *testing.T) {
	c, w := started(t), &handing{cut: true}
	w.input(t, c, "in-1", "perhaps")
	w.cut = false
	evs, _ := w.tick(c, time.Now(), DefaultInputTimeout+time.Second)
	if len(evs) != 1 || evs[0].Reason != agent.NotDelivered || evs[0].Input != "in-1" {
		t.Fatalf("events %+v", evs)
	}
	w.want(t)
}

// A refused paste waits through a turn that runs meanwhile, with no Enter
// at its end (#741).
func TestRefusedPasteWaitsForATurn(t *testing.T) {
	c, w := started(t), &handing{refuse: true}
	w.input(t, c, "in-1", "held up")
	if got := summary(decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json")); got != "turn(terminal)" {
		t.Fatalf("a prompt typed at the terminal: %s", got)
	}
	w.refuse = false
	evs, now := w.tick(c, time.Now(), 2*DefaultInputTimeout)
	w.want(t)
	if len(evs) != 0 {
		t.Fatalf("events during the turn %+v", evs)
	}
	c.mu.Lock()
	c.track("", agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Status: agent.Completed}}})
	c.mu.Unlock()
	w.tick(c, now, 8*time.Second)
	w.want(t, "held up", enterKey)
}

// An interrupt of a message that was not handed over ends its turn and
// drops it (#741).
func TestInterruptOfARefusedPaste(t *testing.T) {
	c, w := started(t), &handing{refuse: true}
	w.input(t, c, "in-1", "held up")
	w.input(t, c, "in-2", "the next")
	a := c.Interrupt()
	if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Status != agent.Interrupted || a.Events[0].Input != "in-1" {
		t.Fatalf("the interrupt: %+v", a)
	}
	w.refuse = false
	w.tick(c, time.Now(), 5*time.Second)
	w.want(t, "the next", enterKey)
}

// An interrupt while a message's Enter is owed is none: the CLI has no turn
// to stop, and the Enter is pressed (#741).
func TestInterruptOfAnOwedEnter(t *testing.T) {
	c, w := started(t), &handing{}
	w.input(t, c, "in-1", "held up")
	w.refuse = true
	_, now := w.tick(c, time.Now(), 10*time.Second)
	if a := c.Interrupt(); !a.NotInterrupted || len(a.Keys) != 0 || len(a.Events) != 0 {
		t.Fatalf("the interrupt: %+v", a)
	}
	w.refuse = false
	w.tick(c, now, 10*time.Second)
	w.want(t, "held up", enterKey)
	if a := c.Interrupt(); a.NotInterrupted || len(a.Keys) != 1 {
		t.Fatalf("the interrupt after the Enter: %+v", a)
	}
}

// An Enter that was owed when another turn started is pressed after its end,
// once it has settled (#741).
func TestAnOwedEnterWaitsForATurn(t *testing.T) {
	c, w := started(t), &handing{}
	w.input(t, c, "in-1", "held up")
	w.refuse = true
	_, now := w.tick(c, time.Now(), 10*time.Second)
	if got := summary(decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json")); got != "turn(terminal)" {
		t.Fatalf("a prompt typed at the terminal: %s", got)
	}
	w.refuse = false
	_, now = w.tick(c, now, 10*time.Second)
	w.want(t, "held up")
	c.mu.Lock()
	c.track("", agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Status: agent.Completed}}})
	c.mu.Unlock()
	w.act(c.Tick(now))
	w.want(t, "held up")
	w.tick(c, now, 8*time.Second)
	w.want(t, "held up", enterKey)
}

// What a refused paste would have reported is not the caller's when a
// person types it, once its turn has ended (#741).
func TestARefusedPasteIsForgotten(t *testing.T) {
	c, w := started(t), &handing{refuse: true}
	w.input(t, c, "in-1", prompt(t, "plain/hooks/02-UserPromptSubmit.json"))
	evs, _ := w.tick(c, time.Now(), DefaultInputTimeout+time.Second)
	if len(evs) != 1 || evs[0].Reason != agent.NotTaken {
		t.Fatalf("events %+v", evs)
	}
	if got := summary(decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json")); got != "turn(terminal)" {
		t.Fatalf("the same text typed: %s", got)
	}
}

// An Enter that may have been handed over is not pressed again (#741).
func TestAnUnsureEnterIsNotPressedAgain(t *testing.T) {
	c, w := started(t), &handing{}
	w.input(t, c, "in-1", "pasted")
	w.cut = true
	_, now := w.tick(c, time.Now(), 2*time.Second)
	w.cut = false
	w.tick(c, now, 10*time.Second)
	w.want(t, "pasted")
}

// A key for the folder-trust dialog that the window refused is pressed on a
// later Tick (#741).
func TestRefusedTrustKeyIsPressedAgain(t *testing.T) {
	// The cursor moves to the folder's line, then Enter (2.1.326, answered).
	dir := "testdata/2.1.326/trust_answered/"
	c, w := screenCodec("/tmp/capture-work/sessions/trust_answered"), &handing{refuse: true}
	for i := 1; i <= 7; i++ {
		w.act(c.Decode(agent.Screen, readScreen(t, fmt.Sprintf("%sscreen-%02d.raw", dir, i))))
	}
	_, now := w.tick(c, time.Now(), 5*time.Second)
	w.want(t)
	w.refuse = false
	_, now = w.tick(c, now, 5*time.Second)
	w.want(t, string(agent.Down))
	w.refuse = true
	w.act(c.Decode(agent.Screen, readScreen(t, dir+"screen-08.raw")))
	_, now = w.tick(c, now, 5*time.Second)
	w.want(t, string(agent.Down))
	w.refuse = false
	_, now = w.tick(c, now, 5*time.Second)
	w.want(t, string(agent.Down), enterKey)
	w.tick(c, now, 10*time.Second)
	w.want(t, string(agent.Down), enterKey)
}
