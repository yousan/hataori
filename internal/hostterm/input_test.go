package hostterm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// marker is typed as text in the tests; it must reach no command line.
const marker = "hataori-typed-marker-5c1e"

const testBuffers = "hataori-input-test-"

// fake records every command and answers from answer, which may be nil.
type fake struct {
	rec    recorder
	answer func(argv []string) ([]byte, error)
}

func (f *fake) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.rec.mu.Lock()
	argv := append([]string{name}, args...)
	f.rec.argvs = append(f.rec.argvs, argv)
	f.rec.stdins = append(f.rec.stdins, stdin)
	f.rec.mu.Unlock()
	if f.answer != nil {
		return f.answer(argv)
	}
	return nil, nil
}

// noMarkerInArgs fails when the marker is in an argument of a command run.
func noMarkerInArgs(t *testing.T, argvs [][]string) {
	t.Helper()
	for _, argv := range argvs {
		for _, a := range argv {
			if strings.Contains(a, marker) {
				t.Errorf("the typed text is in the arguments of %q", argv)
			}
		}
	}
}

// The commands that type are exactly send-keys with one key name,
// load-buffer with the text on standard input, and paste-buffer -d of the
// same buffer; a refused batch runs nothing.
func TestInputCommands(t *testing.T) {
	ctx := context.Background()
	f := &fake{}
	l := &Lister{Tmux: []string{"-L", "x"}, Buffers: testBuffers, run: f.run}
	buf, err := l.NewBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf, testBuffers) || !l.ownBuffer(buf) {
		t.Fatalf("buffer %q", buf)
	}
	keys := []control.HostKey{{Key: "Up"}, {Text: marker + "\x7f"}, {Key: "C-c"}, {Text: marker, Paste: true}, {Key: "C-M-x"}, {Key: "M-;"}}
	if err := l.Keys(ctx, "%4", buf, keys); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"tmux", "-L", "x", "send-keys", "-t", "%4", "Up"},
		{"tmux", "-L", "x", "load-buffer", "-b", buf, "-"},
		{"tmux", "-L", "x", "paste-buffer", "-d", "-b", buf, "-t", "%4", "-S"}, // typed text goes as it is
		{"tmux", "-L", "x", "send-keys", "-t", "%4", "C-c"},
		{"tmux", "-L", "x", "load-buffer", "-b", buf, "-"},
		{"tmux", "-L", "x", "paste-buffer", "-d", "-b", buf, "-t", "%4", "-p"},
		{"tmux", "-L", "x", "send-keys", "-t", "%4", "C-M-x"},
		{"tmux", "-L", "x", "send-keys", "-t", "%4", `M-\;`}, // a ";" at the end would end tmux's command
	}
	if !slices.EqualFunc(f.rec.argvs, want, slices.Equal) {
		t.Errorf("ran\n%q\nwant\n%q", f.rec.argvs, want)
	}
	if !bytes.Equal(f.rec.stdins[1], []byte(marker+"\x7f")) || !bytes.Equal(f.rec.stdins[4], []byte(marker)) {
		t.Errorf("standard input %q", f.rec.stdins)
	}
	for i, in := range f.rec.stdins {
		if in != nil && !strings.HasPrefix(f.rec.argvs[i][3], "load-buffer") {
			t.Errorf("%q got standard input", f.rec.argvs[i])
		}
	}
	noMarkerInArgs(t, f.rec.argvs)

	refused := map[string]struct {
		pane, buffer string
		keys         []control.HostKey
	}{
		"a key name off the list":     {"%4", buf, []control.HostKey{{Key: "Up"}, {Key: "Space"}}},
		"a character alone":           {"%4", buf, []control.HostKey{{Key: "a"}}},
		"a modifier twice":            {"%4", buf, []control.HostKey{{Key: "C-C-a"}}},
		"two characters":              {"%4", buf, []control.HostKey{{Key: "C-ab"}}},
		"S- with a character":         {"%4", buf, []control.HostKey{{Key: "S-a"}}},
		"C- with a brace":             {"%4", buf, []control.HostKey{{Key: "C-{"}}},
		"S-Tab, which tmux types":     {"%4", buf, []control.HostKey{{Key: "S-Tab"}}},
		"C-Enter, which tmux drops":   {"%4", buf, []control.HostKey{{Key: "C-Enter"}}},
		"text as a key":               {"%4", buf, []control.HostKey{{Key: marker}}},
		"a key and a text":            {"%4", buf, []control.HostKey{{Key: "Up", Text: marker}}},
		"a paste without text":        {"%4", buf, []control.HostKey{{Key: "Up", Paste: true}}},
		"nothing":                     {"%4", buf, []control.HostKey{{}}},
		"a pane that is no pane id":   {"%4; kill-server", buf, []control.HostKey{{Key: "Up"}}},
		"another instance's buffer":   {"%4", "hataori-input-other-0123456789abcdef", []control.HostKey{{Text: marker}}},
		"a buffer with a longer name": {"%4", buf + "0", []control.HostKey{{Text: marker}}},
	}
	for name, r := range refused {
		f.rec.argvs = nil
		err := l.Keys(ctx, r.pane, r.buffer, r.keys)
		if err == nil || len(f.rec.argvs) != 0 {
			t.Errorf("%s: %v, ran %q", name, err, f.rec.argvs)
		}
		if err != nil && strings.Contains(err.Error(), marker) {
			t.Errorf("%s: the error holds the text: %v", name, err)
		}
	}

	// A server before tmux 3.7 refuses -S: the text is pasted again without
	// it, and so is every later one.
	f.rec.argvs = nil
	f.answer = func(argv []string) ([]byte, error) {
		if argv[3] == "paste-buffer" && argv[len(argv)-1] == "-S" {
			return nil, errors.New("hostterm: tmux paste-buffer: exit status 1: command paste-buffer: unknown flag -S")
		}
		return nil, nil
	}
	if err := l.Keys(ctx, "%4", buf, []control.HostKey{{Text: marker}, {Text: marker}}); err != nil {
		t.Fatal(err)
	}
	want = [][]string{
		{"tmux", "-L", "x", "load-buffer", "-b", buf, "-"},
		{"tmux", "-L", "x", "paste-buffer", "-d", "-b", buf, "-t", "%4", "-S"},
		{"tmux", "-L", "x", "paste-buffer", "-d", "-b", buf, "-t", "%4"},
		{"tmux", "-L", "x", "load-buffer", "-b", buf, "-"},
		{"tmux", "-L", "x", "paste-buffer", "-d", "-b", buf, "-t", "%4"},
	}
	if !slices.EqualFunc(f.rec.argvs, want, slices.Equal) {
		t.Errorf("with an older tmux, ran\n%q\nwant\n%q", f.rec.argvs, want)
	}
	l.noRawPaste.Store(false)

	// A paste that fails deletes the buffer, and so does a load that fails.
	for _, failing := range []string{"paste-buffer", "load-buffer"} {
		f.rec.argvs = nil
		f.answer = func(argv []string) ([]byte, error) {
			if argv[3] == failing {
				return nil, errors.New("hostterm: tmux " + failing + ": exit status 1: can't find pane: %4")
			}
			return nil, nil
		}
		if err := l.Keys(ctx, "%4", buf, []control.HostKey{{Text: marker}}); err == nil {
			t.Errorf("a failing %s is no error", failing)
		}
		last := f.rec.argvs[len(f.rec.argvs)-1]
		if !slices.Equal(last, []string{"tmux", "-L", "x", "delete-buffer", "-b", buf}) {
			t.Errorf("after a failing %s, ran %q", failing, f.rec.argvs)
		}
	}
	noMarkerInArgs(t, f.rec.argvs)
}

// A batch whose context ends runs no further command.
func TestKeysStopWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fake{answer: func([]string) ([]byte, error) { cancel(); return nil, nil }}
	l := &Lister{Buffers: testBuffers, run: f.run}
	buf, _ := l.NewBuffer()
	if err := l.Keys(ctx, "%1", buf, []control.HostKey{{Key: "Up"}, {Key: "Down"}, {Text: marker}}); err == nil {
		t.Error("a stopped batch is no error")
	}
	if len(f.rec.argvs) != 1 {
		t.Errorf("ran %q", f.rec.argvs)
	}
}

// A command that has started runs to its end when the batch's context ends:
// a load is not cut short, and is followed by the buffer's deletion, not by
// a paste.
func TestACommandRunsToItsEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var argvs [][]string
	var cut []bool
	l := &Lister{Buffers: testBuffers, run: func(cctx context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		argvs = append(argvs, args)
		if args[0] == "load-buffer" {
			cancel()
		}
		cut = append(cut, cctx.Err() != nil)
		return nil, nil
	}}
	buf, _ := l.NewBuffer()
	if err := l.Keys(ctx, "%1", buf, []control.HostKey{{Text: marker}, {Key: "Up"}}); err == nil {
		t.Error("a batch stopped after a load is no error")
	}
	if len(argvs) != 2 || argvs[0][0] != "load-buffer" || argvs[1][0] != "delete-buffer" || slices.Contains(cut, true) {
		t.Errorf("ran %q, cut short %v", argvs, cut)
	}
}

// DropLeftovers deletes this instance's buffers and no other.
func TestDropLeftoversTouchesNoOtherBuffer(t *testing.T) {
	ours := testBuffers + "0123456789abcdef"
	f := &fake{answer: func(argv []string) ([]byte, error) {
		if argv[3] == "list-buffers" {
			return []byte(strings.Join([]string{"buffer0", ours, "hataori-input-other-0123456789abcdef",
				testBuffers + "0123", testBuffers + "0123456789abcdef0", "x" + ours}, "\n") + "\n"), nil
		}
		return nil, nil
	}}
	l := &Lister{Tmux: []string{"-L", "x"}, Buffers: testBuffers, run: f.run}
	n, err := l.DropLeftovers(context.Background())
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	want := [][]string{{"tmux", "-L", "x", "list-buffers", "-F", "#{buffer_name}"}, {"tmux", "-L", "x", "delete-buffer", "-b", ours}}
	if !slices.EqualFunc(f.rec.argvs, want, slices.Equal) {
		t.Errorf("ran %q", f.rec.argvs)
	}
	if _, err := (&Lister{run: f.run}).DropLeftovers(context.Background()); err == nil {
		t.Error("without a prefix every buffer would be this instance's")
	}
	if err := l.DropBuffer(context.Background(), "buffer0"); err == nil {
		t.Error("DropBuffer deleted another buffer")
	}
}

var tmuxServers atomic.Int64

// ownTmux starts a tmux server of the test's own, on a socket in a
// temporary directory, with no configuration file, and returns how to
// reach it. It never reaches the default server.
func ownTmux(t *testing.T) (sock []string, tmux func(args ...string) (string, error)) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("tmux is not installed; CI must run this test")
		}
		t.Skip("no tmux")
	}
	dir, err := os.MkdirTemp("/tmp", "ht") // short on macOS too: a socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", dir)
	sock = []string{"-L", fmt.Sprintf("hataori-input-test-%d-%d", os.Getpid(), tmuxServers.Add(1)), "-f", "/dev/null"}
	tmux = func(args ...string) (string, error) {
		out, err := exec.Command("tmux", slices.Concat(sock, args)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("tmux %v: %v: %s", args, err, out)
		}
		return string(out), nil
	}
	t.Cleanup(func() { tmux("kill-server") })
	return sock, tmux
}

// waitFor polls until cond holds, for at most ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Against a real tmux (Appendix B, "tmux: typing into a pane"): Up reaches
// a program in application cursor mode as ESC O A; text with a DEL in it
// arrives byte for byte; a paste is bracketed when the program asked for
// it; the buffer is gone after each paste; the window's size and the
// session's session_attached do not change; and the text is in no
// command's arguments.
func TestTypingIntoARealPane(t *testing.T) {
	sock, tmux := ownTmux(t)
	dir := t.TempDir()
	// Each pane puts its terminal in raw mode, switches a mode on, shows a
	// word once tmux has read the switch, and keeps the bytes it reads.
	progs := []struct {
		name, mode, ready string
		n                 int
	}{
		{"cursor", `\033[?1h`, "READY-CURSOR", 3},
		{"text", ``, "READY-TEXT", len(marker) + 4},
		{"paste", `\033[?2004h`, "READY-PASTE", 14},
		{"semicolon", ``, "READY-SEMICOLON", 3},
	}
	panes := map[string]string{}
	for i, p := range progs {
		cmd := fmt.Sprintf(`stty raw -echo; printf '%s%s'; head -c %d > %s; exec sleep 600`, p.mode, p.ready, p.n, filepath.Join(dir, p.name))
		var err error
		if i == 0 {
			_, err = tmux("new-session", "-d", "-s", "typing", "-x", "100", "-y", "30", "sh", "-c", cmd)
		} else {
			_, err = tmux("new-window", "-d", "-t", "typing:", "sh", "-c", cmd)
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := tmux("display-message", "-p", "-t", fmt.Sprintf("typing:%d", i), "#{pane_id}")
		if err != nil {
			t.Fatal(err)
		}
		panes[p.name] = strings.TrimSpace(out)
		waitFor(t, p.ready, func() bool {
			s, _ := tmux("capture-pane", "-p", "-t", panes[p.name])
			return strings.Contains(s, p.ready)
		})
	}
	size := func() string {
		out, err := tmux("display-message", "-p", "-t", panes["cursor"], "#{window_width}x#{window_height} #{session_attached}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	before := size()
	if before != "100x30 0" {
		t.Fatalf("before: %s", before)
	}

	rec := &recorder{}
	l := &Lister{Tmux: sock, Own: "hataori-never-runs", Buffers: testBuffers, run: rec.run}
	ctx := context.Background()
	for _, p := range panes {
		if tp, own, err := l.Check(ctx, p, nil, true); err != nil || tp == nil || own {
			t.Fatalf("Check(%s): %+v %v %v", p, tp, own, err)
		}
	}
	buf, _ := l.NewBuffer()
	noBuffers := func(when string) {
		out, err := tmux("list-buffers", "-F", "#{buffer_name}")
		if err != nil && !strings.Contains(err.Error(), "no buffers") {
			t.Fatal(err)
		}
		if strings.Contains(out, testBuffers) {
			t.Errorf("%s: buffers %q", when, out)
		}
	}
	if err := l.Keys(ctx, panes["cursor"], buf, []control.HostKey{{Key: "Up"}}); err != nil {
		t.Fatal(err)
	}
	during := size()
	text := marker + "\x7f\tz\r"
	if err := l.Keys(ctx, panes["text"], buf, []control.HostKey{{Text: text}}); err != nil {
		t.Fatal(err)
	}
	noBuffers("after a paste")
	if err := l.Keys(ctx, panes["paste"], buf, []control.HostKey{{Text: "xy", Paste: true}}); err != nil {
		t.Fatal(err)
	}
	noBuffers("after a bracketed paste")
	if err := l.Keys(ctx, panes["semicolon"], buf, []control.HostKey{{Key: "M-;"}, {Key: "C-\\"}}); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"cursor": "\x1bOA", "text": text, "paste": "\x1b[200~xy\x1b[201~", "semicolon": "\x1b;\x1c"}
	for name, w := range want {
		var got []byte
		waitFor(t, "what "+name+" read", func() bool {
			got, _ = os.ReadFile(filepath.Join(dir, name))
			return len(got) >= len(w)
		})
		if string(got) != w {
			t.Errorf("%s read %q, want %q", name, got, w)
		}
	}
	if after := size(); during != before || after != before {
		t.Errorf("window size and session_attached: before %s, during %s, after %s", before, during, after)
	}
	noMarkerInArgs(t, rec.argvs)
	if !slices.ContainsFunc(rec.stdins, func(b []byte) bool { return bytes.Contains(b, []byte(marker)) }) {
		t.Error("the text went to no command's standard input")
	}

	// A paste into a pane that is gone fails, and the buffer is deleted.
	if err := l.Keys(ctx, "%999", buf, []control.HostKey{{Text: marker}}); err == nil {
		t.Error("a paste into a pane that is not there")
	}
	noBuffers("after a failed paste")
	noMarkerInArgs(t, rec.argvs)
}

// Check finds a pane gone, and hataori's own by the command tmux reports
// and, with the process table, by a process on the pane's terminal.
func TestCheckFindsHataorisOwn(t *testing.T) {
	sock, tmux := ownTmux(t)
	if _, err := tmux("new-session", "-d", "-s", "own", "exec sleep 600"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("new-window", "-d", "-t", "own:", "sh", "-c", "sleep 600 & exec cat"); err != nil {
		t.Fatal(err)
	}
	pane := func(w string) string {
		out, err := tmux("display-message", "-p", "-t", "own:"+w, "#{pane_id}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	fg, bg := pane("0"), pane("1")
	l := &Lister{Tmux: sock, Own: "sleep"}
	ctx := context.Background()
	waitFor(t, "the panes' commands", func() bool {
		ps, _ := l.panes(ctx)
		n := 0
		for _, p := range ps {
			if p.Tmux.Pane == fg && p.Process == "sleep" || p.Tmux.Pane == bg && p.Process == "cat" {
				n++
			}
		}
		return n == 2
	})
	if p, own, err := l.Check(ctx, fg, nil, false); err != nil || p == nil || !own {
		t.Errorf("a pane whose command is hataori's: %+v %v %v", p, own, err)
	}
	if p, own, err := l.Check(ctx, bg, nil, false); err != nil || p == nil || own {
		t.Errorf("without the process table: %+v %v %v", p, own, err)
	}
	waitFor(t, "the background process", func() bool {
		_, own, _ := l.Check(ctx, bg, nil, true)
		return own
	})
	if p, own, err := l.Check(ctx, "%999", nil, true); err != nil || p != nil || own {
		t.Errorf("a pane that is not there: %+v %v %v", p, own, err)
	}
	other := &Lister{Tmux: sock, Own: "hataori-never-runs"}
	if p, own, err := other.Check(ctx, bg, nil, true); err != nil || p == nil || own {
		t.Errorf("a pane hataori does not run in: %+v %v %v", p, own, err)
	}
}

// DropLeftovers on a real server deletes only this instance's buffers.
func TestDropLeftoversOnARealServer(t *testing.T) {
	sock, tmux := ownTmux(t)
	if _, err := tmux("new-session", "-d", "-s", "left", "exec sleep 600"); err != nil {
		t.Fatal(err)
	}
	ours := testBuffers + "00112233445566ff"
	for name, v := range map[string]string{ours: marker, "mine": "keep", "hataori-input-other-00112233445566ff": "keep"} {
		if _, err := tmux("set-buffer", "-b", name, v); err != nil {
			t.Fatal(err)
		}
	}
	l := &Lister{Tmux: sock, Buffers: testBuffers}
	if n, err := l.DropLeftovers(context.Background()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	out, _ := tmux("list-buffers", "-F", "#{buffer_name}")
	names := strings.Fields(out)
	slices.Sort(names)
	if want := []string{"hataori-input-other-00112233445566ff", "mine"}; !slices.Equal(names, want) {
		t.Errorf("buffers left %q, want %q", names, want)
	}
}

// Every key name ValidHostKey accepts, with its modifiers in one order,
// reaches a real tmux's pane as something: not nothing, and not its own
// letters, which is what tmux types for a name it cannot encode.
func TestEveryValidKeyIsEncodedByTmux(t *testing.T) {
	_, tmux := ownTmux(t)
	got := filepath.Join(t.TempDir(), "got")
	if _, err := tmux("new-session", "-d", "-s", "keys", "sh", "-c", "stty raw -echo; printf READY; exec cat > "+got); err != nil {
		t.Fatal(err)
	}
	pane, err := tmux("display-message", "-p", "-t", "keys:0", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	pane = strings.TrimSpace(pane)
	waitFor(t, "the pane", func() bool { s, _ := tmux("capture-pane", "-p", "-t", pane); return strings.Contains(s, "READY") })
	var bases []string
	for k := range anyKeysForTest() {
		bases = append(bases, k)
	}
	for ch := byte(0x21); ch < 0x7f; ch++ {
		bases = append(bases, string(ch))
	}
	slices.Sort(bases)
	var names []string
	for _, mods := range []string{"", "C-", "M-", "S-", "C-M-", "C-S-", "M-S-", "C-M-S-"} {
		for _, b := range bases {
			if control.ValidHostKey(mods + b) {
				names = append(names, mods+b)
			}
		}
	}
	if len(names) < 300 {
		t.Fatalf("only %d names", len(names))
	}
	// One tmux command line per few keys: each key, then a 0xff byte,
	// which no key makes, to tell them apart.
	for i := 0; i < len(names); i += 40 {
		var args []string
		for _, n := range names[i:min(i+40, len(names))] {
			args = append(args, "send-keys", "-t", pane, keyArg(n), ";", "send-keys", "-t", pane, "-H", "ff", ";")
		}
		if _, err := tmux(args[:len(args)-1]...); err != nil {
			t.Fatal(err)
		}
	}
	var out []byte
	waitFor(t, "every key", func() bool {
		out, _ = os.ReadFile(got)
		return bytes.Count(out, []byte{0xff}) == len(names)
	})
	chunks := bytes.Split(out, []byte{0xff})
	for i, n := range names {
		if len(chunks[i]) == 0 || bytes.Contains(chunks[i], []byte(n)) {
			t.Errorf("%s reached the pane as %q", n, chunks[i])
		}
	}
}

// anyKeysForTest are the named keys, as control documents them.
func anyKeysForTest() map[string]bool {
	m := map[string]bool{}
	for _, k := range strings.Fields("Enter Tab BTab BSpace Escape Up Down Left Right Home End PPage NPage IC DC F1 F2 F3 F4 F5 F6 F7 F8 F9 F10 F11 F12") {
		m[k] = true
	}
	return m
}
