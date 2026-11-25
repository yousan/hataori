package claudecode

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// The workspace the trust capture was taken in, as scrubbed.
const trustDir = "/tmp/capture-work/sessions/trust"

// screenCodec returns a codec for the workspace dir, opened as for a new
// start: watching the screen.
func screenCodec(dir string) *codec {
	c := (&Adapter{}).NewCodec(agent.Spec{Dir: dir}).(*codec)
	c.Open()
	return c
}

// settle gives the codec the screen, then ticks as the runner does until
// the screen has been still long enough, and returns the keys sent.
func settle(c *codec, screen []byte, t0 time.Time) (string, time.Time) {
	var all Actions
	all = merge(all, c.Decode(agent.Screen, screen))
	for i := 0; i <= 4; i++ {
		all = merge(all, c.Tick(t0.Add(time.Duration(i)*time.Second)))
	}
	return keysOf(all), t0.Add(5 * time.Second)
}

func keysOf(a Actions) string {
	var ks []string
	for _, w := range a.Keys {
		switch {
		case w == keyUp:
			ks = append(ks, "up")
		case w == keyDown:
			ks = append(ks, "down")
		case w == keyEnter:
			ks = append(ks, "enter")
		case w.Key == "":
			ks = append(ks, pasteStart+w.Paste+pasteEnd)
		default:
			ks = append(ks, string(w.Key))
		}
	}
	return strings.Join(ks, " ")
}

func readScreen(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The captured dialog, and the same dialog with its choices the other way
// round: the line is found by its text.
func TestTrustDialogMovesTowardYes(t *testing.T) {
	dialog := readScreen(t, "testdata/2.1.322/trust/terminal.raw")
	no := []byte("\x1b[2G\x1b[38;5;153m❯\x1b[4GNo,\x1b[8Gexit\x1b[39m")
	yes := []byte("\x1b[4GYes,\x1b[9GI\x1b[11Gtrust\x1b[17Gthis\x1b[22Gfolder")
	nl := []byte("\r\r\n")
	order := append(append(append([]byte{}, no...), nl...), yes...)
	if !bytes.Contains(dialog, order) {
		t.Fatal("the capture's choices are not where the test expects them")
	}
	swapped := bytes.Replace(dialog, order, append(append(append([]byte{}, yes...), nl...), no...), 1)
	for _, tc := range []struct {
		name   string
		screen []byte
		want   string
	}{
		{"captured", dialog, "down"},
		{"choices the other way round", swapped, "up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := screenCodec(trustDir)
			if !c.Open().WatchScreen {
				t.Fatal("Open does not watch the screen")
			}
			t0 := time.Now()
			if got := keysOf(c.Decode(agent.Screen, tc.screen)); got != "" {
				t.Fatalf("keys %q before the screen settled", got)
			}
			if got := keysOf(merge(c.Tick(t0), c.Tick(t0.Add(time.Second)))); got != "" {
				t.Fatalf("keys %q within %v", got, screenSettle)
			}
			got, now := settle(c, nil, t0)
			if got != tc.want {
				t.Fatalf("keys %q, want %q", got, tc.want)
			}
			// Until the screen shows the cursor moved, nothing more.
			if got, _ := settle(c, []byte("\x1b[?25l"), now); got != "" {
				t.Fatalf("a second key %q before the cursor moved", got)
			}
		})
	}
}

// Other screens get no key: another captured prompt, the dialog for another
// directory, the dialog with a third choice.
func TestOtherScreensGetNoKey(t *testing.T) {
	dialog := readScreen(t, "testdata/2.1.322/trust/terminal.raw")
	for _, tc := range []struct {
		name, dir string
		screen    []byte
	}{
		{"the login command's prompt", trustDir, readScreen(t, "testdata/2.1.322/login_terminal/terminal.raw")},
		{"no conversation to resume", trustDir, readScreen(t, "testdata/2.1.322/resume_missing/terminal.raw")},
		{"the dialog for another directory", "/tmp/capture-work/sessions/other", dialog},
		{"a third choice", trustDir, bytes.Replace(dialog, []byte("folder\r\r\n"), []byte("folder\r\r\n\x1b[4GMaybe\r\r\n"), 1)},
		{"text after the footer", trustDir, append(append([]byte{}, dialog...), []byte("\r\nSomething else\r\n")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := screenCodec(tc.dir)
			if got, _ := settle(c, tc.screen, time.Now()); got != "" {
				t.Fatalf("keys %q for %s", got, tc.name)
			}
		})
	}
}

// After the first hook call, the session is past any dialog: no key is
// sent for it, whatever the screen shows.
func TestNoKeyAfterTheStart(t *testing.T) {
	c := screenCodec(trustDir)
	start := readScreen(t, "testdata/2.1.322/plain/hooks/01-SessionStart.json")
	c.Decode(agent.Hook, envelope("c1", "plain/hooks/01-SessionStart.json", start))
	if got, _ := settle(c, readScreen(t, "testdata/2.1.322/trust/terminal.raw"), time.Now()); got != "" {
		t.Fatalf("keys %q after SessionStart", got)
	}
}

// The real CLI's screen (2.1.326, at the session host's 80 columns), as it
// was given to the codec. Chunks 1 to 7 came together: the dialog, then
// the CLI's questions to the terminal. One move once that settled, and
// Enter once the screen showed the cursor on the line that says yes.
func TestTrustDialogAnsweredAsCaptured(t *testing.T) {
	dir := "testdata/2.1.326/trust_answered/"
	c := screenCodec("/tmp/capture-work/sessions/trust_answered")
	var first []byte
	for i := 1; i <= 7; i++ {
		first = append(first, readScreen(t, fmt.Sprintf("%sscreen-%02d.raw", dir, i))...)
	}
	keys, now := settle(c, first, time.Now())
	if keys != "down" {
		t.Fatalf("keys %q for the dialog, want down", keys)
	}
	if keys, _ = settle(c, readScreen(t, dir+"screen-08.raw"), now); keys != "enter" {
		t.Fatalf("keys %q once the cursor moved, want enter", keys)
	}
}

// A screen the renderer cannot read gets no key, even when the dialog was
// drawn before: a reset, an erasure it does not know, the alternate screen.
func TestUnreadableScreenGetsNoKey(t *testing.T) {
	dialog := readScreen(t, "testdata/2.1.322/trust/terminal.raw")
	for _, tc := range []struct{ name, after string }{
		{"a reset", "\x1bc"},
		{"erasing up to the cursor", "\x1b[1J"},
		{"the alternate screen", "\x1b[?1049h"},
		{"lines deleted", "\x1b[2M"},
		{"a mode that clears the screen", "\x1b[?3h"},
		{"the cursor saved by a mode", "\x1b[?1048h"},
		{"concealed text", "\x1b[8m"},
		{"a window resized", "\x1b[8;24;40t"},
		{"an inline image", "\x1b]1337;File=inline=1:AAAA\x07"},
		{"line drawing characters", "\x1b(0"},
		{"synchronized output", "\x1b[?2026h"},
		{"concealed text, written with zeros", "\x1b[008m"},
		{"an end of bold on some terminals only", "\x1b[1m\x1b[21m"},
		{"bold with a part", "\x1b[1:2m"},
		{"a colour of no known kind", "\x1b[38;1m"},
		{"a rendition that is no number", "\x1b[ m"},
		{"a position restored in another weight", "\x1b[1m\x1b[s\x1b[22m\x1b[u"},
		{"a cursor restored in another weight", "\x1b7\x1b[1m\x1b8"},
		{"the alternate screen left in another weight", "\x1b[?1049h\x1b[1m\x1b[?1049l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := screenCodec(trustDir)
			if got, _ := settle(c, append(append([]byte{}, dialog...), tc.after...), time.Now()); got != "" {
				t.Fatalf("keys %q after %s", got, tc.name)
			}
		})
	}
}

// The weight of what a line shows, as the sequences that set it say.
func TestLineWeight(t *testing.T) {
	for _, tc := range []struct {
		written string
		want    weight
	}{
		{"x", plain},
		{"\x1b[1mx y\x1b[22m", bold},
		{"\x1b[1mx\x1b[0my", mixed},
		{"\x1b[1m\x1b[mx", plain},
		{"\x1b[01m\x1b[022mx", plain},
		{"\x1b[01mx", bold},
		{"\x1b[1;38;5;22mx", bold},
		{"\x1b[1;38;2;0;22;0mx", bold},
		{"\x1b[38;5;1mx", plain},
		{"\x1b[48;2;1;1;1mx", plain},
		{"\x1b[38:2::1:22:0mx", plain},
		{"\x1b[38;5;220m\x1b[1mx\x1b[39m y", bold},
		{"\x1b[1mx\rx", bold},
		{"\x1b[1mx\x1b[22m\ry", plain}, // written over
		{"x\x1b[1m  \x1b[22my", plain},
		{"x\x1b[1m y\x1b[22m\x1b[2G\x1b[K", plain}, // erased
		{"\x1b[1mx\x1b[s\x1b[u", bold},
		{"x\x1b[1m \u0301", mixed}, // a mark on a blank shows
		{"\x1b[1mx \x1b[22m\u0301", mixed},
		{"\x1b[1m\x1b[?1049hx", bold},
	} {
		lines, ok := renderScreen([]byte(tc.written))
		if !ok || len(lines) != 1 || lines[0].weight != tc.want {
			t.Errorf("%q: %+v %v, want weight %d", tc.written, lines, ok, tc.want)
		}
	}
}

// A combining mark goes with the character left of the cursor, and at the
// start of a line there is none.
func TestCombiningMarks(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{"e\u0301\u0302x\x1b[4Gy", "e\u0301\u0302x y"},
		{"e\u20ddx\x1b[3Gy", "e\u20ddxy"},
		{"e\u0301x\x1b[1Gy", "yx"}, // written over
		{"ab\u0301\x1b[2G\x1b[K", "a"},
		{"\x1b[3G\u0301x", "  \u0301x"},
	} {
		if lines, ok := renderScreen([]byte(tc.written)); !ok || len(lines) != 1 || lines[0].text != tc.want {
			t.Errorf("%+q: %+q %v, want %+q", tc.written, lineTexts(lines), ok, tc.want)
		}
	}
	if lines, ok := renderScreen([]byte("x\r\n\u0301y")); ok {
		t.Errorf("at the start of a line: %+q, want it not read", lineTexts(lines))
	}
}

// Confirming the dialog does not end the watch: the first input needs the
// screen (first_enter_test.go). The session's end does, and so does too
// much screen.
func TestScreenWatchEnds(t *testing.T) {
	c := screenCodec("/tmp/capture-work/sessions/trust_answered")
	dir := "testdata/2.1.326/trust_answered/"
	var first []byte
	for i := 1; i <= 7; i++ {
		first = append(first, readScreen(t, fmt.Sprintf("%sscreen-%02d.raw", dir, i))...)
	}
	_, now := settle(c, first, time.Now())
	a := merge(c.Decode(agent.Screen, readScreen(t, dir+"screen-08.raw")), Actions{})
	for i := 0; i <= 4; i++ {
		a = merge(a, c.Tick(now.Add(time.Duration(i)*time.Second)))
	}
	if keysOf(a) != "enter" || a.StopScreen {
		t.Fatalf("keys %q, stop %t: want enter, and the watch going on", keysOf(a), a.StopScreen)
	}
	end := readScreen(t, dir+"hooks/02-SessionEnd.json")
	if !c.Decode(agent.Hook, envelope("e1", "trust_answered/hooks/02-SessionEnd.json", end)).StopScreen {
		t.Fatal("the session's end does not end the watch")
	}

	c = screenCodec(trustDir)
	if !c.Decode(agent.Screen, bytes.Repeat([]byte("x"), maxScreen+1)).StopScreen {
		t.Fatal("more than maxScreen does not end the watch")
	}
}
