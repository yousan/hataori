package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/wire"
)

// ptySize is the size of a terminal device as the kernel has it.
func ptySize(t *testing.T, device string) string {
	t.Helper()
	f, err := os.OpenFile(device, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ws, err := pty.GetsizeFull(f)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%dx%d", ws.Cols, ws.Rows)
}

// fitted is how a terminal view's grid sits in its pane: the session's
// columns and rows, the size of a cell, the font size, and the room the
// grid leaves to the right of it and under it in the pane's content box
// (negative when it goes past).
type fitted struct {
	Cols, Rows     int
	CellW, CellH   float64
	Font           string
	SpareW, SpareH float64
	PaneW, PaneH   float64
	DPR            float64
}

// fitOf reads how the terminal view at selector sits in its pane.
func (b *browser) fitOf(selector string) fitted {
	b.t.Helper()
	var f fitted
	b.eval(fmt.Sprintf(`(() => {
		const view = document.querySelector(%q);
		const pane = view.closest('.pane');
		const term = view.querySelector('.xterm');
		const r = view.querySelector('.xterm-screen').getBoundingClientRect();
		const p = pane.getBoundingClientRect(), cs = getComputedStyle(pane);
		const px = k => parseFloat(cs[k]);
		const right = p.right - px('paddingRight') - px('borderRightWidth');
		const bottom = p.bottom - px('paddingBottom') - px('borderBottomWidth');
		const cols = +term.dataset.cols, rows = +term.dataset.rows;
		return {cols, rows, cellW: r.width / cols, cellH: r.height / rows,
			font: getComputedStyle(view.querySelector('.xterm-rows')).fontSize,
			spareW: right - r.right, spareH: bottom - r.bottom, paneW: p.width, paneH: p.height, dpr: devicePixelRatio};
	})()`, selector), &f)
	return f
}

// fills reports whether f's grid fills its pane's width or its height,
// and goes past neither. The width is filled within one cell. xterm.js
// draws each row a whole number of device pixels tall, so the height is
// filled within one cell or within one device pixel per row, whichever is
// more: no font size puts the grid's bottom closer to the pane's.
func (f fitted) fills() bool {
	if f.Cols == 0 || f.Rows == 0 || f.SpareW < -0.5 || f.SpareH < -0.5 {
		return false
	}
	return f.widthBound() || f.SpareH < max(f.CellH, float64(f.Rows)/f.DPR)
}

// widthBound reports whether the grid reached the pane's width first.
func (f fitted) widthBound() bool { return f.SpareW < f.CellW }

// untilFilled waits for the terminal view at selector to fill its pane
// with cols columns and rows rows, and returns how it sits.
func (b *browser) untilFilled(what, selector string, cols, rows int) fitted {
	b.t.Helper()
	var f fitted
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var ok bool
		b.eval(fmt.Sprintf(`!!document.querySelector(%q)?.querySelector('.xterm[data-cols]')`, selector), &ok)
		if ok {
			if f = b.fitOf(selector); f.fills() && f.Cols == cols && f.Rows == rows {
				return f
			}
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: the terminal does not fill its pane: %+v", what, f)
		}
	}
}

// The read-only terminal of a thread's pane and of a tmux pane fills its
// pane's width or height, whichever comes first, within a cell, at its
// session's own size. It fits again when the window is resized, the panes
// are split, the tree and the details are hidden and shown, and the
// session's size changes; the session's terminal keeps its size and gets
// nothing; the scrollback still scrolls; the narrow list is as before.
func TestDashboardTerminalFillsPane(t *testing.T) {
	rig := newHostTerminalRig(t)
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = rig.socket }
	t.Cleanup(func() { hostTermsHook = nil })
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = true })
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	// Sixty lines: more than the terminal's rows, so some are scrollback.
	var script strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&script, "screen scroll-line-%02d\n", i)
	}
	script.WriteString("reply done")
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: script.String()}})
	if err != nil {
		t.Fatal(err)
	}
	var x control.ThreadInfo
	eventually(t, "the thread", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		x = d.ThreadInfo
		return err == nil && d.State == control.Idle && d.Terminal != ""
	})
	sizeBefore := ptySize(t, x.Terminal)
	if sizeBefore != "80x24" {
		t.Fatalf("the session's terminal is %s, not 80x24", sizeBefore)
	}

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	dense := func(w, h int, dpr float64) {
		t.Helper()
		b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": h, "deviceScaleFactor": dpr, "mobile": false})
	}
	window := func(w, h int) { t.Helper(); dense(w, h, 1) }
	window(1400, 900)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	n := int64(x.ID)
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, n))
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, n))
	view := fmt.Sprintf(`.pane[data-pane="0"] [data-terminal="%d"]`, n)
	b.until("the thread's output", fmt.Sprintf(`[...document.querySelectorAll('%s .xterm-rows > div')].some(d => d.textContent.includes('scroll-line-60'))`, view))

	// 1. Two pane sizes, and every change of the pane: each fills.
	sizes, fonts, bound := map[string]bool{}, map[string]bool{}, map[bool]bool{}
	check := func(what string) fitted {
		t.Helper()
		f := b.untilFilled(what, view, 80, 24)
		sizes[fmt.Sprintf("%.0fx%.0f", f.PaneW, f.PaneH)] = true
		fonts[f.Font] = true
		bound[f.widthBound()] = true
		return f
	}
	check("one pane in a wide window")
	window(1000, 700)
	check("after the window is resized")
	window(1400, 900)
	check("after the window is resized back")
	for _, split := range []string{"2", "3", "1"} {
		b.click(`[data-tool=split]`)
		b.until("split "+split, fmt.Sprintf(`document.querySelector('.panes').dataset.split === %q`, split))
		check("split " + split)
	}
	for _, tool := range []string{"details", "tree"} {
		b.click(fmt.Sprintf(`[data-tool=%s]`, tool))
		check(tool + " hidden")
		b.click(fmt.Sprintf(`[data-tool=%s]`, tool))
		check(tool + " shown")
	}
	// A short, wide pane: the grid reaches the height first.
	b.click(`[data-tool=details]`)
	b.click(`[data-tool=tree]`)
	window(1400, 600)
	if f := check("a short, wide pane"); f.widthBound() {
		t.Fatalf("the short, wide pane is filled across: %+v", f)
	}
	// Screens of other pixel densities, where rows are drawn in other
	// steps; the window keeps its CSS size. A fit made for one density
	// can go past the pane on the other.
	for _, d := range []struct {
		w   int
		dpr float64
	}{{1400, 2}, {1401, 2}, {1401, 1}, {1401, 1.25}, {1400, 1.25}, {1400, 1}} {
		dense(d.w, 600, d.dpr)
		b.until("the screen's density", fmt.Sprintf(`devicePixelRatio === %v`, d.dpr))
		check(fmt.Sprintf("%d px wide at density %v", d.w, d.dpr))
	}
	window(1400, 900)
	b.click(`[data-tool=details]`)
	b.click(`[data-tool=tree]`)
	check("the tree and the details back")
	if len(sizes) < 4 || len(fonts) < 4 {
		t.Fatalf("the pane took only %d sizes and the terminal %d font sizes: %v %v", len(sizes), len(fonts), sizes, fonts)
	}
	if !bound[true] || !bound[false] {
		t.Fatalf("the grid never reached the pane's width first, or never its height first: %v", bound)
	}

	// 2. The scrollback still scrolls: the wheel over the terminal brings
	// back the first lines.
	var at struct{ X, Y float64 }
	b.eval(fmt.Sprintf(`(() => { const r = document.querySelector('%s .xterm-screen').getBoundingClientRect(); return {x: r.left + r.width / 2, y: r.top + r.height / 2}; })()`, view), &at)
	for i := 0; i < 40; i++ {
		b.call("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": at.X, "y": at.Y, "deltaX": 0, "deltaY": -400})
	}
	b.until("the first lines after scrolling up", fmt.Sprintf(`[...document.querySelectorAll('%s .xterm-rows > div')].some(d => d.textContent.trim() === 'scroll-line-01')`, view))
	check("after scrolling")

	// 3. A tmux pane fills its pane too, and fits again when its size
	// changes.
	hostRow := fmt.Sprintf(`.tree [data-host=%q]`, rig.device)
	b.until("the tmux pane in the tree", `!!document.querySelector(`+strconv.Quote(hostRow)+`)`)
	b.click(hostRow)
	hostView := fmt.Sprintf(`.pane[data-pane="0"] [data-host-screen=%q]`, rig.pane)
	b.until("the tmux pane's screen", fmt.Sprintf(`[...document.querySelectorAll('%s .xterm-rows > div')].some(d => d.textContent.includes('pane-marker'))`, hostView))
	var hostCols, hostRows int
	b.eval(fmt.Sprintf(`+document.querySelector('%s .xterm').dataset.cols`, hostView), &hostCols)
	b.eval(fmt.Sprintf(`+document.querySelector('%s .xterm').dataset.rows`, hostView), &hostRows)
	b.untilFilled("the tmux pane", hostView, hostCols, hostRows)
	tmux := func(args ...string) {
		t.Helper()
		cmd := exec.Command("tmux", append(append([]string{}, rig.socket...), args...)...)
		cmd.Env = append(os.Environ(), "TMUX=")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}
	// The page draws a pane's screen as many rows tall as the screen it
	// reads: more when the window is taller.
	tmux("resize-window", "-t", "work", "-x", "80", "-y", "40")
	b.untilFilled("the tmux pane, taller", hostView, hostCols, hostRows+16)
	tmux("resize-window", "-t", "work", "-x", "80", "-y", "24")
	b.untilFilled("the tmux pane, back", hostView, hostCols, hostRows)

	// 4. Back on the thread: it still fills.
	b.click(fmt.Sprintf(`.tabbar .tab[data-tab="thread:%d"] .tab-main`, n))
	check("the thread again")

	// 5. Nothing went to the session: its terminal has the size it had,
	// and every request of the page was a GET or HEAD of its own origin.
	if got := ptySize(t, x.Terminal); got != sizeBefore {
		t.Fatalf("the session's terminal is %s, was %s", got, sizeBefore)
	}
	page, _ := url.Parse(link)
	requests := 0
	for _, e := range b.events {
		switch e.Method {
		case "Network.webSocketCreated":
			t.Fatalf("the page opened a WebSocket: %s", e.Params)
		case "Network.requestWillBeSent":
			var p struct{ Request struct{ URL, Method string } }
			json.Unmarshal(e.Params, &p)
			u, _ := url.Parse(p.Request.URL)
			if u == nil || u.Scheme == "data" || u.Scheme == "about" {
				continue
			}
			requests++
			if (p.Request.Method != "GET" && p.Request.Method != "HEAD") || u.Host != page.Host {
				t.Fatalf("the page sent %s %s", p.Request.Method, p.Request.URL)
			}
		}
	}
	if requests == 0 {
		t.Fatal("no request of the page was seen: the check would test nothing")
	}

	// 6. The narrow list is as before: the session's grid at the
	// terminal's own font size, wider than the phone, scrolling in its box.
	b.phone()
	b.until("the narrow list", fmt.Sprintf(`!!document.querySelector('.narrow-list .row[data-thread="%d"]')`, n))
	b.click(fmt.Sprintf(`.narrow-list .row[data-thread="%d"]`, n))
	b.until("the thread in the narrow layout", fmt.Sprintf(`!!document.querySelector('.narrow-pane [data-terminal="%d"] .xterm[data-cols]')`, n))
	var narrow struct {
		Font  string
		Width float64
		Fit   bool
	}
	b.eval(fmt.Sprintf(`(() => { const v = document.querySelector('.narrow-pane [data-terminal="%d"]');
		return {font: getComputedStyle(v.querySelector('.xterm-rows')).fontSize, width: v.querySelector('.xterm-screen').getBoundingClientRect().width, fit: v.classList.contains('fit')}; })()`, n), &narrow)
	if narrow.Font != "15px" || narrow.Width <= 390 || narrow.Fit {
		t.Fatalf("the narrow layout's terminal changed: %+v", narrow)
	}
	b.noSideScroll("the narrow thread")

	if got := ptySize(t, x.Terminal); got != sizeBefore {
		t.Fatalf("the session's terminal is %s, was %s", got, sizeBefore)
	}
	var csp []string
	b.eval(`window.__csp`, &csp)
	if len(csp) > 0 {
		t.Fatalf("CSP violations: %v", csp)
	}
}
