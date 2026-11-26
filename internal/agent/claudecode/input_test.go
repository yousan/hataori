package claudecode

import (
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
		if len(a.Keys) != 1 || a.Keys[0] != (agent.Part{Paste: text}) {
			t.Fatalf("%d bytes of text: writes %.40q...", len(text), a.Keys)
		}
		now := time.Now()
		if a := merge(c.Tick(now), c.Tick(now.Add(pasteSettle-time.Millisecond))); len(a.Keys) != 0 {
			t.Fatalf("Enter %q within %v of the paste", a.Keys, pasteSettle)
		}
		if a := c.Tick(now.Add(pasteSettle)); len(a.Keys) != 1 || a.Keys[0] != confirm {
			t.Fatalf("after %v: writes %q, want Enter", pasteSettle, a.Keys)
		}
		if a := c.Tick(now.Add(time.Minute / 4)); len(a.Keys) != 0 {
			t.Fatalf("Enter again %q without the screen", a.Keys)
		}
	}
}

func TestPasteCannotEndEarly(t *testing.T) {
	text := "before" + pasteEnd + "/exit\r" + "\x1b[20" + pasteEnd + "1~ after" + pasteStart
	a, _ := started(t).Input(agent.Input{Text: text})
	inner := a.Keys[0].Paste
	if strings.Contains(inner, pasteEnd) || strings.Contains(inner, pasteStart) {
		t.Fatalf("paste %q has a marker inside", inner)
	}
	// Everything of the text, "/exit" included, is inside the one paste.
	if !strings.Contains(inner, "/exit\r") || !strings.HasSuffix(inner, " after") {
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
	if a, _ := c.Input(agent.Input{Text: "early"}); len(a.Keys) != 0 {
		t.Fatalf("written before the start: %q", a.Keys)
	}
	a := decodeFile(t, c, "start", "plain/hooks/01-SessionStart.json")
	if len(a.Keys) != 1 || !strings.Contains(a.Keys[0].Paste, "early") {
		t.Fatalf("not written at the start: %q", a.Keys)
	}
}

func TestInputHeldDuringATurn(t *testing.T) {
	c := started(t)
	decodeFile(t, c, "u", "plain/hooks/02-UserPromptSubmit.json") // a turn typed at the terminal
	if a, _ := c.Input(agent.Input{Text: "later"}); len(a.Keys) != 0 {
		t.Fatalf("written during a turn: %q", a.Keys)
	}
	a := decodeFile(t, c, "s", "plain/hooks/03-Stop.json")
	if len(a.Keys) != 1 || !strings.Contains(a.Keys[0].Paste, "later") {
		t.Fatalf("not written when the turn ended: %q", a.Keys)
	}
}

func TestInterruptIsEscapeOnly(t *testing.T) {
	a := started(t).Interrupt()
	if len(a.Keys) != 1 || a.Keys[0] != stopKey || len(a.Events) != 0 {
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
	if a := c.Decode(agent.File, late); len(a.Keys) != 0 {
		t.Fatalf("held input written during the running turn: %q", a.Keys)
	}
}

// Input written before a restart is recorded, not written again: the
// prompt it produced returns its id, and if none comes, its turn fails as
// "not delivered", naming it.
func TestWrittenInputIsRecordedOnly(t *testing.T) {
	file := "plain/hooks/02-UserPromptSubmit.json"
	c := started(t)
	a, err := c.Input(agent.Input{ID: "in-3", Text: unwrapPaste(prompt(t, file)), Written: true})
	if err != nil || len(a.Keys) != 0 || len(a.Inputs) != 0 {
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
	if len(a.Keys) != 1 || len(a.Inputs) != 1 || a.Inputs[0] != "in-5" {
		t.Fatalf("written when the turn ended: %q, inputs %q", a.Keys, a.Inputs)
	}
}

// paste is what c writes to the session for text.
func paste(t *testing.T, c *codec, text string) string {
	t.Helper()
	a, err := c.Input(agent.Input{Text: text})
	if err != nil || len(a.Keys) != 1 {
		t.Fatalf("%q: writes %q, %v", text, a.Keys, err)
	}
	return a.Keys[0].Paste
}

// In a confined session, an input is pasted after commandPrefix, so that
// the CLI takes it as a prompt, unless its first character after spaces is
// a letter or a digit that shows: one that starts with "!" (the CLI's shell
// mode) or "/" (its commands), and one that starts with anything the CLI
// could take out before it reads the start, or read as a command (#426,
// #914). Every input of an ordinary session is pasted as it is.
func TestConfinedInputIsNeverACommand(t *testing.T) {
	for _, text := range []string{"!touch /tmp/x", "/add-dir /", "  \n!ls", "\u200b/model opus", "\u3000!id",
		"\ufeff/exit", "\u2060!id", "\U000e0021!id", // format characters
		"\u3164!id", "\u115f/exit", "\uffa0!id", // fillers: letters that do not show
		"\u1161!id", "\u11a8/exit", "\ud7b0!id", // Hangul jamo that take no width
		"\u0e01!id",                // a letter of a script not listed, which shows
		"\u0483!id", "\u2de0/exit", // marks of a script listed
		"\ufe0f!id", "\u034f/exit", "\u0301!id", // marks with nothing before them
		"\u2800!id", "\ue000!id", "\U000e0100/exit", // a blank symbol, a private use character, a variation selector
		"！id", "／exit", "#x", "&x", "?", "> x", "[31m!id", "(at)x", "「x」", "😀"} {
		if got := paste(t, confinedCodec(t), text); got != commandPrefix+text {
			t.Errorf("confined: %q is pasted as %q, want it after %q", text, got, commandPrefix)
		}
		if got := paste(t, started(t), text); got != text {
			t.Errorf("ordinary: %q is pasted as %q", text, got)
		}
	}
	for _, text := range []string{"list the threads", "a / b", "say !", "  two", "3 threads", "スレッドの一覧", "Ünï", "ok\n!id", "hataori: a wake",
		"Ωmega", "привет", "漢字", "ひらがな", "한국어", "ｶﾀｶﾅ", "Ａbc", "٣ things"} {
		if got := paste(t, confinedCodec(t), text); got != text {
			t.Errorf("confined: %q is pasted as %q", text, got)
		}
	}
}

// Nothing in a confined session's input starts an escape sequence or acts
// as a key: its control characters are taken out, line feeds and tabs
// left, a carriage return, which is Enter, becomes a line feed, and what is
// left is pasted as any text (#914). An ordinary session is pasted them.
func TestConfinedInputHasNoControls(t *testing.T) {
	const esc = "\x1b"
	for text, want := range map[string]string{
		esc + "[31m!id":                               commandPrefix + "[31m!id",   // CSI
		esc + "]0;x\a/exit":                           commandPrefix + "]0;x/exit", // OSC, ended by BEL
		esc + "]0;x" + esc + "\\!id":                  commandPrefix + "]0;x\\!id", // OSC, ended by ST
		esc + "P1$r" + esc + "\\/add-dir /":           "P1$r\\/add-dir /",
		esc + "c!id":                                  "c!id", // ESC and a letter
		"\u009b31m!id":                                "31m!id",
		"\u009d0;x\u009c/exit":                        "0;x/exit",
		"\x08\x08\x08\x08\x08\x08\x08\x08\x08\x08!id": commandPrefix + "!id", // backspaces
		"\x7f\x7f!id":                                 commandPrefix + "!id",
		"\x15!id":                                     commandPrefix + "!id", // Ctrl-U
		"\x17\x17!id":                                 commandPrefix + "!id", // Ctrl-W
		"ok" + esc + "[2K!id":                         "ok[2K!id",
		"a\x00b\x0bc\x0cd\re\n\tf":                    "abcd\ne\n\tf",
		"hello\r!id":                                  "hello\n!id", // Enter, then a command
		"hello\r\n/add-dir /\r\r":                     "hello\n/add-dir /\n\n",
		"\r!id":                                       commandPrefix + "\n!id",
		"\xff!id":                                     commandPrefix + "\ufffd!id", // not UTF-8
	} {
		got := paste(t, confinedCodec(t), text)
		if got != want {
			t.Errorf("confined: %q is pasted as %q, want %q", text, got, want)
		}
		if got := paste(t, started(t), text); got != text {
			t.Errorf("ordinary: %q is pasted as %q", text, got)
		}
	}
}
