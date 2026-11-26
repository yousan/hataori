package claudecode

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func capturedWideDialog(t *testing.T) []byte {
	t.Helper()
	return readScreen(t, filepath.Join("testdata", "2.1.332", "trust_marks_emoji", "screen-04.raw"))
}

// The unchanged real capture contains narrow emoji bases, a keycap and a
// wide watch, all with emoji selectors. A wide base must not become three
// columns. Derived variants remove its selector or substitute another
// two-column character, retaining every captured absolute cursor position.
// These variants are not claimed as additional CLI captures.
func TestWideColumnsFromCapturedDialog(t *testing.T) {
	raw := capturedWideDialog(t)
	const watch = "⌚\ufe0f"
	if bytes.Count(raw, []byte(watch)) != 1 {
		t.Fatal("capture changed: expected one watch")
	}
	for _, tc := range []struct{ name, glyph string }{
		{"actual captured emoji", watch},
		{"derived watch without selector", "⌚"},
		{"derived Japanese", "語"},
		{"derived fullwidth", "Ａ"},
		{"derived wide combining", "⌚\u0301"},
		{"derived text selector", "⌚\ufe0e"},
		{"derived supplementary selector", "⌚\U000e0100"},
		{"derived repeated emoji selector", watch + "\ufe0f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bytes.Replace(raw, []byte(watch), []byte(tc.glyph), 1)
			lines, ok := renderScreen(b)
			want := "Bash(" + tc.glyph + "x y)"
			if !ok || !strings.Contains(strings.Join(lineTexts(lines), "\n"), want) {
				t.Fatalf("rendered %q, readable=%v; want %q", lineTexts(lines), ok, want)
			}
			c := screenCodec("/tmp/capture-work/sessions/trust_marks_emoji")
			all := c.Decode(agent.Screen, b)
			now := time.Now()
			for range 4 {
				now = now.Add(time.Second)
				all = merge(all, c.Tick(now))
			}
			if keysOf(all) != "" || len(all.Events) != 1 || all.Events[0].Reason != agent.BlockedAtStart || !strings.Contains(all.Events[0].Text, want) {
				t.Fatalf("keys %q, events %+v", keysOf(all), all.Events)
			}
		})
	}
}

// Derive boundary operations from the same captured watch and its column
// positions. Ambiguous half-character writes/erasures must not silently
// drop repository text and leave an apparently safe dialog.
func TestWideCharacterBoundariesFromCapture(t *testing.T) {
	raw := capturedWideDialog(t)
	before, after, ok := bytes.Cut(raw, []byte("⌚\ufe0f"))
	if !ok {
		t.Fatal("no captured watch")
	}
	wide := append(bytes.Clone(before), []byte("⌚")...)
	for _, suffix := range []string{
		"\r\x1b[36Gx", // overwrite the continuation
		"\r\x1b[35Gx", // overwrite only the leading cell
		"\r\x1b[36G\x1b[K",
		"\r\x1b[36G\x1b[J",
		"\r\x1b[35G\x1b[1K",
		"\r\x1b[36G\u0301", // mark with a cursor inside the glyph
	} {
		b := append(bytes.Clone(wide), suffix...)
		if lines, ok := renderScreen(b); ok {
			t.Errorf("partial operation %q was readable: %q", suffix, lineTexts(lines))
		}
	}
	// Replacing a whole wide glyph is unambiguous and keeps later words.
	replaced := append(bytes.Clone(wide), []byte("\r\x1b[35G語")...)
	replaced = append(replaced, after...)
	if lines, ok := renderScreen(replaced); !ok || !strings.Contains(strings.Join(lineTexts(lines), "\n"), "Bash(語x y)") {
		t.Fatalf("whole replacement: %q, %v", lineTexts(lines), ok)
	}
	// A selector must not expand a narrow base across existing text.
	covered := append(bytes.Clone(before), []byte("❤x\r\x1b[35G❤\ufe0f")...)
	if _, ok := renderScreen(covered); ok {
		t.Fatal("emoji selector covered existing text")
	}
	// Joined emoji require grapheme-cluster layout, which is not captured.
	// Such a screen is left unread instead of guessed to be five columns.
	for _, cluster := range []string{"👩\u200d💻", "⌚\U0001f3fb"} {
		b := bytes.Replace(raw, []byte("⌚\ufe0f"), []byte(cluster), 1)
		if _, ok := renderScreen(b); ok {
			t.Errorf("unsupported cluster %q was read", cluster)
		}
	}
}
