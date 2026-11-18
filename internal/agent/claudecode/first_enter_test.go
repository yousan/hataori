package claudecode

import (
	"bytes"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// The start captured on 2.1.327 under the user's own settings (#178), at
// the session host's 80 columns. screen-before.raw is the screen when
// SessionStart was reported: the trust dialog confirmed and taken down,
// bracketed paste switched off. echo.raw is how the first paste came back
// then: echoed by the terminal's line discipline, the CLI not yet reading
// keys, its Enter turned into a new line. screen-after.raw is the CLI's
// input box coming up, bracketed paste on, and holding the text with an
// empty line below it: the Enter was lost, and no UserPromptSubmit came.
const firstEnter = "testdata/2.1.327/first_enter_lost/"

const firstText = "Smoke test 2. Reply with the single word ok and change nothing."

// firstStart returns a watching codec given the screen up to SessionStart,
// and SessionStart.
func firstStart(t *testing.T) *codec {
	c := screenCodec("/tmp/capture-work/sessions/first_enter_lost")
	if a := c.Decode(agent.Screen, readScreen(t, firstEnter+"screen-before.raw")); len(a.Write) != 0 {
		t.Fatalf("keys %q for the screen before the start", a.Write)
	}
	start := readScreen(t, firstEnter+"hooks/01-SessionStart.json")
	c.Decode(agent.Hook, envelope("s1", "first_enter_lost/hooks/01-SessionStart.json", start))
	return c
}

// The first input waits for the input box, then goes as a paste, Enter
// after it, and Enter again while the box still holds the text.
func TestFirstInputWaitsForTheInputBox(t *testing.T) {
	c := firstStart(t)
	a, _ := c.Input(agent.Input{ID: "first", Text: firstText})
	if len(a.Write) != 0 {
		t.Fatalf("written %q at the start, before the CLI reads keys", a.Write)
	}
	if !c.watch {
		t.Fatal("the watch ended at the start, before the first input")
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if a := c.Tick(now.Add(time.Duration(i) * time.Second)); len(a.Write) != 0 {
			t.Fatalf("written %q before the input box came up", a.Write)
		}
	}
	now = now.Add(5 * time.Second)

	a = c.Decode(agent.Screen, readScreen(t, firstEnter+"screen-after.raw"))
	if len(a.Write) != 1 || !bytes.Equal(a.Write[0], []byte(pasteStart+firstText+pasteEnd)) || len(a.Inputs) != 1 {
		t.Fatalf("once the box came up: writes %q, inputs %q; want the paste alone", a.Write, a.Inputs)
	}
	if got := keysOf(c.Tick(now)); got != "" {
		t.Fatalf("keys %q with the paste", got)
	}
	if got := keysOf(c.Tick(now.Add(time.Second))); got != "enter" {
		t.Fatalf("keys %q after the paste, want enter", got)
	}
	// The screen as captured: the text still in the box, no hook.
	var keys []string
	for i := 2; i <= 20; i++ {
		if k := keysOf(c.Tick(now.Add(time.Duration(i) * time.Second))); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) != maxAgain {
		t.Fatalf("Enter pressed again %q while the box holds the text, want %d times", keys, maxAgain)
	}
}

// With the input box empty, as the CLI first draws it, Enter is not
// pressed again; and the watch ends once the prompt is reported.
func TestNoEnterAgainForAnEmptyBox(t *testing.T) {
	c := firstStart(t)
	after := readScreen(t, firstEnter+"screen-after.raw")
	frame := []byte("\x1b[?25l\x1b[H")
	first := bytes.Index(after, frame)
	second := first + len(frame) + bytes.Index(after[first+len(frame):], frame)
	if first < 0 || second < first+len(frame) || bytes.Contains(after[:second], []byte("Smoke")) {
		t.Fatal("the capture's first frame is not where the test expects it")
	}
	c.Decode(agent.Screen, after[:second])
	if a, _ := c.Input(agent.Input{ID: "first", Text: firstText}); len(a.Write) != 1 {
		t.Fatalf("writes %q with the box up, want the paste", a.Write)
	}
	now := time.Now()
	var keys []string
	for i := 0; i <= 20; i++ {
		if k := keysOf(c.Tick(now.Add(time.Duration(i) * time.Second))); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) != 1 || keys[0] != "enter" {
		t.Fatalf("keys %q for an empty box, want one enter", keys)
	}
	prompt := []byte(`{"hook":"u1","wait":false,"payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"p1","prompt":"` + firstText + `"}}`)
	a := c.Decode(agent.Hook, prompt)
	if !a.StopScreen || summary(a) != "turn(caller)" {
		t.Fatalf("the reported prompt: %s, stop %t", summary(a), a.StopScreen)
	}
}

// A turn that starts between the paste and its Enter, typed at the
// terminal, gets no Enter; the input then ends "not delivered".
func TestNoEnterDuringATurn(t *testing.T) {
	c := firstStart(t)
	c.Decode(agent.Screen, readScreen(t, firstEnter+"screen-after.raw"))
	if a, _ := c.Input(agent.Input{Text: firstText}); len(a.Write) != 1 {
		t.Fatalf("writes %q, want the paste", a.Write)
	}
	typed := []byte(`{"hook":"u1","wait":false,"payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"p1","prompt":"typed"}}`)
	if summary(c.Decode(agent.Hook, typed)) != "turn(terminal)" {
		t.Fatal("the typed prompt did not start a turn")
	}
	now := time.Now()
	for i := 0; i <= 20; i++ {
		if k := keysOf(c.Tick(now.Add(time.Duration(i) * time.Second))); k != "" {
			t.Fatalf("keys %q during the typed turn", k)
		}
	}
}

// A screen that never shows the box holds the first input back for
// pasteWait only.
func TestFirstInputGoesAfterPasteWait(t *testing.T) {
	c := firstStart(t)
	c.Input(agent.Input{Text: firstText})
	now := time.Now()
	c.Tick(now)
	if a := c.Tick(now.Add(pasteWait - time.Second)); len(a.Write) != 0 {
		t.Fatalf("written %q before pasteWait", a.Write)
	}
	if a := c.Tick(now.Add(pasteWait)); len(a.Write) != 1 {
		t.Fatalf("writes %q after pasteWait, want the paste", a.Write)
	}
}
