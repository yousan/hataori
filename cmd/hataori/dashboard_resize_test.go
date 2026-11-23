package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// terminalWatch collects a thread's terminal stream as a viewer gets it.
type terminalWatch struct {
	mu     sync.Mutex
	chunks []control.TerminalChunk
}

func watchTerminal(t *testing.T, root string, id control.ThreadID) *terminalWatch {
	t.Helper()
	return watchTerminalFrom(t, root, id, "", 0)
}

// watchTerminalFrom collects the stream from position after of run, as a
// viewer that reconnects asks for it.
func watchTerminalFrom(t *testing.T, root string, id control.ThreadID, run string, after uint64) *terminalWatch {
	t.Helper()
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); c.Close() })
	ch, err := c.Terminal(ctx, id, run, after)
	if err != nil {
		t.Fatal(err)
	}
	w := &terminalWatch{}
	go func() {
		for c := range ch {
			w.mu.Lock()
			w.chunks = append(w.chunks, c)
			w.mu.Unlock()
		}
	}()
	return w
}

// resizedTo reports whether the stream took the size cols×rows in a chunk
// of its own, with no data, and the agent's redraw at that size came in a
// later chunk of that size.
func (w *terminalWatch) resizedTo(cols, rows int, redraw string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	marked := false
	for i, c := range w.chunks {
		if i > 0 && !marked && c.Cols == cols && c.Rows == rows {
			if len(c.Data) != 0 || c.Reset {
				return false // the size came with data, not where it changed
			}
			marked = true
			continue
		}
		if marked && c.Cols == cols && c.Rows == rows && strings.Contains(string(c.Data), redraw) {
			return true
		}
	}
	return false
}

// The acceptance criteria of #632 on the server, with the fake agent in a
// terminal and the real daemon: an attachment gives the session's terminal
// a size, which the agent gets (it redraws at it) and every viewer's
// stream carries, at the place in the output where it changed; a size out
// of bounds is refused and changes nothing; the attachment that resized
// logs one line at its end with the last size; the size stays after it
// ends; an attachment that sends no size changes none.
func TestDashboardAttachResizesTheTerminal(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen ready-to-resize")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	eventually(t, "the session's terminal", 15*time.Second, func() bool { x = threadOf(t, r.root); return x.Terminal != "" })
	if got := ptySize(t, x.Terminal); got != "80x24" {
		t.Fatalf("the session's terminal is %s, not 80x24", got)
	}
	view := watchTerminal(t, r.root, x.ID)
	var before control.TerminalChunk // where a viewer was before the resizes
	eventually(t, "the viewer's first chunk", 10*time.Second, func() bool {
		view.mu.Lock()
		defer view.mu.Unlock()
		if len(view.chunks) > 0 {
			before = view.chunks[0]
		}
		return before.Run != ""
	})
	base, s := dashSignIn(t, r.root)
	c, m := startAttach(t, base, s, x.ID)
	if m.Attached == nil {
		t.Fatalf("the start: %+v", m)
	}

	for _, z := range []struct {
		cols, rows int
		want       string
	}{{120, 40, "120x40"}, {90, 30, "90x30"}} {
		c.WriteJSON(map[string]any{"resize": map[string]any{"cols": z.cols, "rows": z.rows}})
		eventually(t, "the terminal at "+z.want, 10*time.Second, func() bool { return ptySize(t, x.Terminal) == z.want })
		eventually(t, "the viewer at "+z.want, 10*time.Second, func() bool { return view.resizedTo(z.cols, z.rows, "fake: size "+z.want) })
	}

	// Out of bounds: refused, and the terminal keeps its size.
	for _, z := range [][2]int{{39, 30}, {90, 9}, {501, 30}, {90, 201}} {
		c.WriteJSON(map[string]any{"resize": map[string]any{"cols": z[0], "rows": z[1]}})
		if m := readAttach(t, c); !strings.Contains(m.Error, "is not given") {
			t.Fatalf("a size of %v: %+v", z, m)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := ptySize(t, x.Terminal); got != "90x30" {
		t.Fatalf("after the sizes out of bounds the terminal is %s", got)
	}
	if strings.Contains(logged(), `msg="dashboard: terminal resized"`) {
		t.Fatal("a resize logged before the attachment ended")
	}
	c.WriteJSON(map[string]any{"end": true})
	if m := readAttach(t, c); m.Ended != "ended" {
		t.Fatalf("the end: %+v", m)
	}
	eventually(t, "the resize logged", 5*time.Second, func() bool {
		return strings.Count(logged(), `msg="dashboard: terminal resized"`) == 1
	})
	if !strings.Contains(logged(), "cols=90 rows=30") {
		t.Fatalf("the resize is not logged with the last size: %s", logged())
	}
	// The size stays once the attachment ended; a viewer that comes now
	// gets it.
	time.Sleep(300 * time.Millisecond)
	if got := ptySize(t, x.Terminal); got != "90x30" {
		t.Fatalf("after the end the terminal is %s", got)
	}
	late := watchTerminal(t, r.root, x.ID)
	eventually(t, "a later viewer at the size", 10*time.Second, func() bool {
		late.mu.Lock()
		defer late.mu.Unlock()
		return len(late.chunks) > 0 && late.chunks[0].Cols == 90 && late.chunks[0].Rows == 30
	})

	// A viewer that reconnects from before the resizes gets the log afresh
	// at the new size, not the bytes since at it.
	again := watchTerminalFrom(t, r.root, x.ID, before.Run, before.Pos)
	eventually(t, "the reconnected viewer's first chunk", 10*time.Second, func() bool {
		again.mu.Lock()
		defer again.mu.Unlock()
		return len(again.chunks) > 0
	})
	again.mu.Lock()
	if f := again.chunks[0]; !f.Reset || f.Cols != 90 || f.Rows != 30 {
		t.Fatalf("a viewer reconnecting from before the resizes got reset=%v at %dx%d", f.Reset, f.Cols, f.Rows)
	}
	again.mu.Unlock()

	// An attachment that sends no size changes none, and logs none.
	c, m = startAttach(t, base, s, x.ID)
	if m.Attached == nil {
		t.Fatalf("the second start: %+v", m)
	}
	c.WriteJSON(map[string]any{"keys": "reply typed\r"})
	r.posted(t, th, "(typed at the terminal)\ntyped")
	c.WriteJSON(map[string]any{"end": true})
	readAttach(t, c)
	eventually(t, "the second end", 5*time.Second, func() bool {
		return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 2
	})
	if n := strings.Count(logged(), `msg="dashboard: terminal resized"`); n != 1 {
		t.Fatalf("%d resizes logged, want the first attachment's only", n)
	}
	if got := ptySize(t, x.Terminal); got != "90x30" {
		t.Fatalf("after an attachment with no size the terminal is %s", got)
	}
}

// resizesSent are the sizes the page gave the session on its attachments,
// from event from on, as COLSxROWS.
func (b *browser) resizesSent(from int) []string {
	b.t.Helper()
	var out []string
	for _, f := range b.sentFrames(from) {
		var m struct{ Resize *struct{ Cols, Rows int } }
		if json.Unmarshal([]byte(f), &m) == nil && m.Resize != nil {
			out = append(out, fmt.Sprintf("%dx%d", m.Resize.Cols, m.Resize.Rows))
		}
	}
	return out
}

// The acceptance criteria of #632 in the browser, against the daemon with
// the fake agent in a terminal: while only viewing, the grid is scaled to
// the pane and centered and the session keeps its size; while the owner
// types, the session takes the pane's size in whole cells at the page's
// font size, in two window sizes, the agent redraws at it and the grid
// fills the pane; a drag gives a few sizes, not one per step; a pane too
// small gives none and is drawn scaled; once typing ends the size stays;
// the attachment that resized logs it once, with the last size.
func TestDashboardTerminalTakesThePaneSize(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen ready-for-size")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	eventually(t, "the session's terminal", 15*time.Second, func() bool { x = threadOf(t, r.root); return x.Terminal != "" })

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	window := func(w, h int) {
		t.Helper()
		b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false})
	}
	window(1400, 900)
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	from := len(b.events)
	openThreadTerminal(t, b, x.ID, "ready-for-size")
	view := fmt.Sprintf(`.pane[data-pane="0"] [data-terminal="%d"]`, x.ID)
	centered := func(what string) {
		t.Helper()
		var g struct{ L, R, T, B float64 }
		b.eval(fmt.Sprintf(`(() => { const v = document.querySelector(%q);
			const box = v.querySelector('.xterm-box').getBoundingClientRect(), r = v.querySelector('.xterm-screen').getBoundingClientRect();
			return {l: r.left - box.left, r: box.right - r.right, t: r.top - box.top, b: box.bottom - r.bottom}; })()`, view), &g)
		if math.Abs(g.L-g.R) > 1.5 || math.Abs(g.T-g.B) > 1.5 {
			t.Fatalf("%s: the grid is not centered in its pane: %+v", what, g)
		}
	}

	// 1. Viewing: scaled and centered; the session keeps its size, and the
	// page gives none.
	b.untilFilled("viewing", view, 80, 24)
	centered("viewing")
	window(1000, 700)
	b.untilFilled("viewing, the window smaller", view, 80, 24)
	centered("viewing, the window smaller")
	window(1400, 900)
	b.untilFilled("viewing, the window back", view, 80, 24)
	time.Sleep(time.Second)
	if got := ptySize(t, x.Terminal); got != "80x24" {
		t.Fatalf("viewing resized the session's terminal to %s", got)
	}
	if z := b.resizesSent(from); len(z) != 0 {
		t.Fatalf("viewing gave the session the sizes %v", z)
	}

	// 2. Typing: the session takes the pane's size, the agent redraws at it,
	// and the grid fills the pane at about the page's font size.
	untilResized := func(what string, n int) string {
		t.Helper()
		var z []string
		eventually(t, what+": a size given", 10*time.Second, func() bool { z = b.resizesSent(from); return len(z) > n })
		last := z[len(z)-1]
		eventually(t, what+": the session at "+last, 10*time.Second, func() bool { return ptySize(t, x.Terminal) == last })
		b.until(what+": the agent's redraw", fmt.Sprintf(`[...document.querySelectorAll('%s .xterm-rows > div')].some(d => d.textContent.includes('fake: size %s'))`, view, last))
		cs, rs, _ := strings.Cut(last, "x")
		cols, _ := strconv.Atoi(cs)
		rows, _ := strconv.Atoi(rs)
		f := b.untilFilled(what, view, cols, rows)
		font, _ := strconv.ParseFloat(strings.TrimSuffix(f.Font, "px"), 64)
		if font < 14.9 || font >= 16 {
			t.Fatalf("%s: the session's %s is drawn at %s, not in whole cells at the page's 15px: %+v", what, last, f.Font, f)
		}
		return last
	}
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "x"})
	first := untilResized("typing in a wide window", 0)
	window(1100, 650)
	second := untilResized("typing in a smaller window", 1)
	if first == second {
		t.Fatalf("two pane sizes gave the session one size, %s", first)
	}
	// A drag: ten sizes in less than half a second give a few, not ten.
	n := len(b.resizesSent(from))
	for i := range 10 {
		window(1100+20*i, 650+10*i)
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	dragged := b.resizesSent(from)[n:]
	if len(dragged) < 1 || len(dragged) > 3 {
		t.Fatalf("a drag gave the session %d sizes: %v", len(dragged), dragged)
	}
	last := untilResized("after the drag", n)

	// 3. A pane too small, while typing: three panes in a small window
	// give no size; the grid is drawn scaled, as when viewing.
	n = len(b.resizesSent(from))
	window(1400, 600)
	for _, split := range []string{"2", "3"} {
		b.click(`[data-tool=split]`)
		b.until("split "+split, fmt.Sprintf(`document.querySelector('.panes').dataset.split === %q`, split))
	}
	time.Sleep(time.Second)
	if z := b.resizesSent(from)[n:]; len(z) != 0 {
		t.Fatalf("a pane too small gave the session %v", z)
	}
	if strings.Contains(logged(), `msg="dashboard: terminal detached"`) {
		t.Fatal("the attachment ended: the pane too small was not typed into")
	}
	cs, rs, _ := strings.Cut(last, "x")
	cols, _ := strconv.Atoi(cs)
	rows, _ := strconv.Atoi(rs)
	b.untilFilled("a pane too small", view, cols, rows)
	if got := ptySize(t, x.Terminal); got != last {
		t.Fatalf("a pane too small left the session at %s, not %s", got, last)
	}

	// 4. Typing ended: the size stays while the pane changes.
	b.eval(`document.querySelector('.filter').focus(), true`, nil)
	eventually(t, "the attachment's end", 10*time.Second, func() bool {
		return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 1
	})
	b.click(`[data-tool=split]`)
	b.until("split 1", `document.querySelector('.panes').dataset.split === "1"`)
	window(1400, 900)
	b.untilFilled("viewing after typing", view, cols, rows)
	time.Sleep(time.Second)
	if got := ptySize(t, x.Terminal); got != last {
		t.Fatalf("after typing ended the session went from %s to %s", last, got)
	}

	// 5. Logged once, by the attachment that resized, with its last size.
	if n := strings.Count(logged(), `msg="dashboard: terminal resized"`); n != 1 || !strings.Contains(logged(), fmt.Sprintf("cols=%d rows=%d", cols, rows)) {
		t.Fatalf("%d resizes logged, want one with %s:\n%s", n, last, logged())
	}
}
