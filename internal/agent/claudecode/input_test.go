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

func TestInputIsOnePasteAndEnter(t *testing.T) {
	for _, text := range []string{"x", strings.Repeat("長い入力 ", 100<<10/13)} {
		a, err := started(t).Input(agent.Input{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		want := [][]byte{[]byte(pasteStart + text + pasteEnd), []byte(confirm)}
		if len(a.Write) != 2 || !bytes.Equal(a.Write[0], want[0]) || !bytes.Equal(a.Write[1], want[1]) {
			t.Fatalf("%d bytes of text: writes %q...", len(text), a.Write)
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
		c.Input(agent.Input{Text: unwrapPaste(prompt(t, file))})
		if got := summary(decodeFile(t, c, "u", file)); got != "turn(caller)" {
			t.Fatalf("%s: %s", file, got)
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
	if len(a.Write) != 2 || !bytes.Contains(a.Write[0], []byte("early")) {
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
	if len(a.Write) != 2 || !bytes.Contains(a.Write[0], []byte("later")) {
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
