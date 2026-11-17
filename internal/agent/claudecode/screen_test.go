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

func screenCodec(dir string) *codec {
	return (&Adapter{}).NewCodec(agent.Spec{Dir: dir}).(*codec)
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
	for _, w := range a.Write {
		switch {
		case bytes.Equal(w, keyUp):
			ks = append(ks, "up")
		case bytes.Equal(w, keyDown):
			ks = append(ks, "down")
		case bytes.Equal(w, keyEnter):
			ks = append(ks, "enter")
		default:
			ks = append(ks, string(w))
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

// After the first hook call, the session is past any dialog: the screen is
// not read and no key is sent, whatever it shows.
func TestNoKeyAfterTheStart(t *testing.T) {
	c := screenCodec(trustDir)
	start := readScreen(t, "testdata/2.1.322/plain/hooks/01-SessionStart.json")
	c.Decode(agent.Hook, envelope("c1", "plain/hooks/01-SessionStart.json", start))
	if got, _ := settle(c, readScreen(t, "testdata/2.1.322/trust/terminal.raw"), time.Now()); got != "" {
		t.Fatalf("keys %q after SessionStart", got)
	}
	if len(c.scr.seen) != 0 {
		t.Fatal("the screen was kept after SessionStart")
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := screenCodec(trustDir)
			if got, _ := settle(c, append(append([]byte{}, dialog...), tc.after...), time.Now()); got != "" {
				t.Fatalf("keys %q after %s", got, tc.name)
			}
		})
	}
}

// The codec ends the watch once it has confirmed, and at the first hook.
func TestScreenWatchEnds(t *testing.T) {
	c := screenCodec("/tmp/capture-work/sessions/trust_answered")
	dir := "testdata/2.1.326/trust_answered/"
	var first []byte
	for i := 1; i <= 7; i++ {
		first = append(first, readScreen(t, fmt.Sprintf("%sscreen-%02d.raw", dir, i))...)
	}
	_, now := settle(c, first, time.Now())
	a := merge(c.Decode(agent.Screen, readScreen(t, dir+"screen-08.raw")), Actions{})
	for i := 0; i <= 4 && !a.StopScreen; i++ {
		a = merge(a, c.Tick(now.Add(time.Duration(i)*time.Second)))
	}
	if keysOf(a) != "enter" || !a.StopScreen {
		t.Fatalf("keys %q, stop %t: want enter and the end of the watch", keysOf(a), a.StopScreen)
	}

	c = screenCodec(trustDir)
	start := readScreen(t, "testdata/2.1.322/plain/hooks/01-SessionStart.json")
	if !c.Decode(agent.Hook, envelope("c1", "plain/hooks/01-SessionStart.json", start)).StopScreen {
		t.Fatal("the first hook does not end the watch")
	}
}
