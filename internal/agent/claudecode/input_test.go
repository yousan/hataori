package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// started returns a codec whose session has reported its start.
func started(t *testing.T) *codec {
	c := newTestCodec()
	decodeFile(t, c, "start", "plain/hooks/01-SessionStart.json")
	return c
}

// Input is one paste, and Enter on a Tick at least pasteSettle after it:
// an Enter that comes with the paste can be taken as part of it (#178).
func TestInputIsOnePasteThenEnter(t *testing.T) {
	for _, text := range []string{"x", strings.Repeat("長い入力 ", 100<<10/13)} {
		c := started(t)
		a, err := c.Input(agent.Input{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Write) != 1 || !bytes.Equal(a.Write[0], []byte(pasteStart+text+pasteEnd)) {
			t.Fatalf("%d bytes of text: writes %.40q...", len(text), a.Write)
		}
		now := time.Now()
		if a := merge(c.Tick(now), c.Tick(now.Add(pasteSettle-time.Millisecond))); len(a.Write) != 0 {
			t.Fatalf("Enter %q within %v of the paste", a.Write, pasteSettle)
		}
		if a := c.Tick(now.Add(pasteSettle)); len(a.Write) != 1 || string(a.Write[0]) != confirm {
			t.Fatalf("after %v: writes %q, want Enter", pasteSettle, a.Write)
		}
		if a := c.Tick(now.Add(time.Minute / 4)); len(a.Write) != 0 {
			t.Fatalf("Enter again %q without the screen", a.Write)
		}
	}
}

func TestPasteCannotEndEarly(t *testing.T) {
	text := "before" + pasteEnd + "/exit\r" + "\x1b[20" + pasteEnd + "1~ after" + pasteStart
	a, _ := started(t).Input(agent.Input{Text: text})
	p := a.Write[0]
	if bytes.Count(p, []byte(pasteEnd)) != 1 || !bytes.HasSuffix(p, []byte(pasteEnd)) ||
		bytes.Count(p, []byte(pasteStart)) != 1 || !bytes.HasPrefix(p, []byte(pasteStart)) {
		t.Fatalf("paste %q has a marker inside", p)
	}
	// Everything of the text, "/exit" included, is inside the one paste.
	inner := bytes.TrimSuffix(bytes.TrimPrefix(p, []byte(pasteStart)), []byte(pasteEnd))
	if !bytes.Contains(inner, []byte("/exit\r")) || !bytes.HasSuffix(inner, []byte(" after")) {
		t.Fatalf("inside the paste: %q", inner)
	}
}

// prompt returns a captured UserPromptSubmit and its prompt.
func prompt(t *testing.T, file string) string {
	b := readCapture(t, file)
	var p payload
	json.Unmarshal(b, &p)
	return p.Prompt
}

func TestDeliveredWhenTheHookReportsIt(t *testing.T) {
	for _, file := range []string{"plain/hooks/02-UserPromptSubmit.json", "paste/hooks/02-UserPromptSubmit.json"} {
		c := started(t)
		c.Input(agent.Input{ID: "in-7", Text: unwrapPaste(prompt(t, file))})
		evs := decodeFile(t, c, "u", file)
		if got := summary(evs); got != "turn(caller)" {
			t.Fatalf("%s: %s", file, got)
		}
		if evs.Events[0].Input != "in-7" {
			t.Fatalf("%s: the turn reports input %q, want the id it was given", file, evs.Events[0].Input)
		}
		now := time.Now()
		c.Tick(now)
		if a := c.Tick(now.Add(time.Hour)); len(a.Events) != 0 {
			t.Fatalf("%s: delivered input timed out: %+v", file, a.Events)
		}
	}
}

func TestNotDeliveredAfterTheTimeout(t *testing.T) {
	c := started(t)
	c.Input(agent.Input{Text: "nobody reports this"})
	now := time.Now()
	c.Tick(now)
	if a := c.Tick(now.Add(DefaultInputTimeout - time.Second)); len(a.Events) != 0 {
		t.Fatalf("timed out early: %+v", a.Events)
	}
	a := c.Tick(now.Add(DefaultInputTimeout))
	if len(a.Events) != 1 || a.Events[0].Kind != agent.TurnEnded || a.Events[0].Reason != agent.NotDelivered {
		t.Fatalf("after the timeout: %+v", a.Events)
	}
}

func TestNoInputBeforeStart(t *testing.T) {
	c := newTestCodec()
	if a, _ := c.Input(agent.Input{Text: "early"}); len(a.Write) != 0 {
		t.Fatalf("written before the start: %q", a.Write)
	}
	a := decodeFile(t, c, "start", "plain/hooks/01-SessionStart.json")
	if len(a.Write) != 1 || !bytes.Contains(a.Write[0], []byte("early")) {
		t.Fatalf("not written at the start: %q", a.Write)
	}
}

func TestInputHeldDuringATurn(t *testing.T) {
	c := started(t)
	decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json") // a turn typed at the terminal
	if a, _ := c.Input(agent.Input{Text: "later"}); len(a.Write) != 0 {
		t.Fatalf("written during a turn: %q", a.Write)
	}
	a := decodeFile(t, c, "s", "plain/hooks/03-Stop.json")
	if len(a.Write) != 1 || !bytes.Contains(a.Write[0], []byte("later")) {
		t.Fatalf("not written when the turn ended: %q", a.Write)
	}
}

func TestInterruptIsEscapeOnly(t *testing.T) {
	a := started(t).Interrupt()
	if len(a.Write) != 1 || string(a.Write[0]) != stopKey || len(a.Events) != 0 {
		t.Fatalf("interrupt: %+v", a)
	}
}

func readCapture(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/2.1.322", file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An interrupt line of an earlier prompt, read after the next turn
// started, does not let held input in during that turn.
func TestLateEndOfAnotherTurn(t *testing.T) {
	c := started(t)
	late := []byte(`{"type":"user","uuid":"l1","promptId":"old","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)
	c.Decode(agent.Hook, []byte(`{"hook":"u1","wait":false,"payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"old","prompt":"one"}}`))
	c.Decode(agent.Hook, []byte(`{"hook":"u2","wait":false,"payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"new","prompt":"two"}}`))
	c.Input(agent.Input{Text: "held"})
	if a := c.Decode(agent.File, late); len(a.Write) != 0 {
		t.Fatalf("held input written during the running turn: %q", a.Write)
	}
}

// Input written before a restart is recorded, not written again: the
// prompt it produced returns its id, and if none comes, its turn fails as
// "not delivered", naming it.
func TestWrittenInputIsRecordedOnly(t *testing.T) {
	file := "plain/hooks/02-UserPromptSubmit.json"
	c := started(t)
	a, err := c.Input(agent.Input{ID: "in-3", Text: unwrapPaste(prompt(t, file)), Written: true})
	if err != nil || len(a.Write) != 0 || len(a.Inputs) != 0 {
		t.Fatalf("written input: %+v, %v", a, err)
	}
	evs := decodeFile(t, c, "u", file)
	if summary(evs) != "turn(caller)" || evs.Events[0].Input != "in-3" {
		t.Fatalf("its prompt: %s, input %q", summary(evs), evs.Events[0].Input)
	}

	c = started(t)
	c.Input(agent.Input{ID: "in-4", Text: "never reported", Written: true})
	now := time.Now()
	c.Tick(now)
	a = c.Tick(now.Add(DefaultInputTimeout))
	if len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered || a.Events[0].Input != "in-4" {
		t.Fatalf("after the timeout: %+v", a.Events)
	}
}

// Input held during a turn and written when it ends names itself in the
// actions that write it.
func TestHeldInputNamedWhenWritten(t *testing.T) {
	c := started(t)
	decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json")
	c.Input(agent.Input{ID: "in-5", Text: "later"})
	a := decodeFile(t, c, "s", "plain/hooks/03-Stop.json")
	if len(a.Write) != 1 || len(a.Inputs) != 1 || a.Inputs[0] != "in-5" {
		t.Fatalf("written when the turn ended: %q, inputs %q", a.Write, a.Inputs)
	}
}
