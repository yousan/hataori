package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// phone makes the browser's page 390 px wide, as a phone held upright.
func (b *browser) phone() {
	b.t.Helper()
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 3, "mobile": true})
}

// noSideScroll fails when the page scrolls sideways.
func (b *browser) noSideScroll(what string) {
	b.t.Helper()
	var w struct{ Scroll, Client, Body, Inner float64 }
	b.eval(`({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth, body: document.body.scrollWidth, inner: innerWidth})`, &w)
	if w.Client != 390 || w.Inner != 390 {
		b.t.Fatalf("%s: the page is %v wide (window %v), not 390: the check would test nothing", what, w.Client, w.Inner)
	}
	if w.Scroll > w.Client || w.Body > w.Client {
		b.t.Fatalf("%s: the page scrolls sideways: %v wide content in %v", what, w.Scroll, w.Client)
	}
	// Nor does anything scroll sideways inside the page, or get cut off:
	// only a terminal wider than the screen may scroll sideways in its own
	// box, and every other element shown ends within the screen.
	var out []string
	b.eval(`(() => {
		const terminalBox = e => !!e.querySelector(':scope > .xterm, :scope > .xterm-host, :scope > .terminal-view');
		const inTerminal = e => !!e.closest('.terminal-view');
		const bad = [];
		for (const e of document.querySelectorAll('body *')) {
			const r = e.getBoundingClientRect();
			if (r.width === 0 || r.height === 0 || getComputedStyle(e).visibility === 'hidden') continue;
			// Text cut with an ellipsis is cut on purpose; anything else that
			// is wider than its box scrolls sideways or is cut off.
			const ellipsis = getComputedStyle(e).textOverflow === 'ellipsis';
			if (e.scrollWidth > e.clientWidth + 1 && !terminalBox(e) && !inTerminal(e) && !ellipsis) bad.push((getComputedStyle(e).overflowX === 'hidden' ? 'cut off: ' : 'scrolls sideways: ') + e.tagName + '.' + e.className + ' ' + e.scrollWidth + '>' + e.clientWidth);
			if (!inTerminal(e) && (r.right > innerWidth + 0.5 || r.left < -0.5)) bad.push('beyond: ' + e.tagName + '.' + e.className + ' ' + Math.round(r.left) + '..' + Math.round(r.right));
		}
		return bad.slice(0, 5);
	})()`, &out)
	if len(out) > 0 {
		b.t.Fatalf("%s: shown beyond the screen: %q", what, out)
	}
}

// narrowRig starts a daemon and threads, and a browser at 390 px on the
// dashboard.
func narrowRig(t *testing.T, root string, starts []string, settled func(c *wire.Client, ids []control.ThreadID) bool) (*wire.Client, []control.ThreadID, *browser) {
	t.Helper()
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	repo := bareRepo(t)
	var ids []control.ThreadID
	for i, text := range starts {
		id, err := c.Start(ctx, control.StartRequest{Key: fmt.Sprint("k", i), Scope: "s", Thread: fmt.Sprint("t", i), Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if i == 0 {
			// The first thread settles before the others start: it may
			// be the one meant to fail.
			eventually(t, "the first thread", 20*time.Second, func() bool { return settled(c, ids) })
		}
	}
	eventually(t, "the threads to settle", 20*time.Second, func() bool { return settled(c, ids) })
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	b.phone()
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the narrow list", `document.querySelector('.shell')?.dataset.layout === 'narrow' && !!document.querySelector('.narrow-list .row')`)
	return c, ids, b
}

func stateOf(c *wire.Client, id control.ThreadID) control.ThreadDetail {
	d, _ := c.Thread(context.Background(), id, 0, 0)
	return d
}

// At 390 px the page is a list with no sideways scroll: needs attention,
// running, then the rest folded. A thread opened from it shows its question
// or failure and its terminal, and the list comes back with the page's
// button and with the browser's Back. The same address serves the wide
// page to a wide window.
func TestDashboardNarrowList(t *testing.T) {
	root := terminalRoot(t)
	once := filepath.Join(t.TempDir(), "fail-once")
	os.WriteFile(once, nil, 0o600)
	setConfig(t, root, func(cfg map[string]any) {
		ad := cfg["adapters"].(map[string]any)[fake.Name].(map[string]any)
		ad["command"] = []string{"sh", "-c", `once=$1; shift; if [ -e "$once" ]; then rm -f "$once"; exit 3; fi; exec "$@"`, "sh", once, os.Args[0], "fake"}
	})
	starts := []string{
		"reply never", // 0: fails
		"limits 0 300 five_hour:300:4:1795000000 seven_day:10080:21:1795161600\nreply done", // 1: idle, with usage limits in the status bar
		"ask Which colour? -- red | green | blue",                                           // 2: a question
		"tool long 10m", // 3: running
		"reply also done, and this first line is long enough to be wider than a phone held upright", // 4: idle, a long title
	}
	want := []control.State{control.Failed, control.Idle, control.Waiting, control.Running, control.Idle}
	c, ids, b := narrowRig(t, root, starts, func(c *wire.Client, ids []control.ThreadID) bool {
		for i, id := range ids {
			if stateOf(c, id).State != want[i] {
				return false
			}
		}
		return true
	})
	num := make([]int64, len(ids))
	for i, id := range ids {
		num[i] = int64(stateOf(c, id).ID)
	}

	// 1. No sideways scroll; needs attention, running, the rest folded.
	b.noSideScroll("the list")
	var order []string
	b.eval(`[...document.querySelectorAll('.narrow-list > [data-section]')].map(s => s.dataset.section)`, &order)
	if fmt.Sprint(order) != "[attention running rest]" {
		t.Fatalf("the list's sections: %v, want attention, running, rest", order)
	}
	rows := func(section string) string {
		var ns []int64
		b.eval(fmt.Sprintf(`[...document.querySelectorAll('.narrow-list [data-section=%q] .row')].map(r => +r.dataset.thread)`, section), &ns)
		return fmt.Sprint(ns)
	}
	b.until("the failure and the question in needs attention", fmt.Sprintf(
		`JSON.stringify([...document.querySelectorAll('.narrow-list [data-section=attention] .row')].map(r => +r.dataset.thread)) === '[%d,%d]'`, num[0], num[2]))
	if got := rows("running"); got != fmt.Sprintf("[%d]", num[3]) {
		t.Fatalf("running: %s, want [%d]", got, num[3])
	}
	if got := rows("rest"); got != fmt.Sprintf("[%d %d]", num[1], num[4]) {
		t.Fatalf("the rest: %s, want [%d %d]", got, num[1], num[4])
	}
	var folded bool
	// Folded: a closed <details> that takes no more room than its summary.
	var why string
	foldedJS := `(() => { const d = document.querySelector('.narrow-list [data-section=rest]'); const h = d.getBoundingClientRect().height, s = d.querySelector('summary').getBoundingClientRect().height;
		return {tag: d.tagName, open: d.open, h, s}; })()`
	b.eval(`JSON.stringify(`+foldedJS+`)`, &why)
	b.eval(`(() => { const f = `+foldedJS+`; return f.tag === 'DETAILS' && !f.open && f.h <= f.s + 2; })()`, &folded)
	if !folded {
		t.Fatalf("the rest is not folded: %s", why)
	}
	b.click(`.narrow-list [data-section=rest] summary`)
	b.until("the rest unfolded", `(() => { const f = `+foldedJS+`; return f.open && f.h > f.s + 40; })()`)
	b.noSideScroll("the list with the rest unfolded")

	listShown := `!!document.querySelector('.narrow-list') && !document.querySelector('.narrow-thread')`
	// 2. A thread opened: its question and its terminal, nothing that
	// answers, and back with the page's button.
	b.click(fmt.Sprintf(`.narrow-list .row[data-thread="%d"]`, num[2]))
	b.until("the question and its terminal", fmt.Sprintf(`document.querySelector('[data-narrow-thread="%d"] [data-block=question] [data-field=question]')?.textContent === 'Which colour?'`+
		` && JSON.stringify([...document.querySelectorAll('[data-narrow-thread="%d"] [data-block=question] [data-field=choices] li')].map(li => li.textContent)) === '["red","green","blue"]'`+
		` && !!document.querySelector('[data-narrow-thread="%d"] .narrow-pane .xterm')`, num[2], num[2], num[2]))
	b.noSideScroll("a thread with its terminal")
	var openPath string
	b.eval(`location.pathname + location.search + location.hash`, &openPath)
	if openPath != "/" {
		t.Fatalf("an opened thread is at %q, want the same address, /", openPath)
	}
	var controls int
	b.eval(`document.querySelectorAll('.narrow-thread button:not(.narrow-back), .narrow-thread a[href], .narrow-thread input, .narrow-thread select, .narrow-thread textarea:not(.xterm-helper-textarea), .narrow-thread [role=button], .narrow-thread summary, .narrow-thread [contenteditable]').length`, &controls)
	// The terminal's own hidden textarea is how xterm.js takes the focus;
	// with stdin disabled it sends nothing (TestDashboardTerminalPaneIsReadOnly).
	if controls != 0 {
		t.Fatalf("the opened thread has %d controls besides the way back", controls)
	}
	b.click(`.narrow-back`)
	b.until("the list again", listShown)
	if d := stateOf(c, ids[2]); d.State != control.Waiting {
		t.Fatalf("the question is %s after looking at it", d.State)
	}

	// The failure and its terminal, and back with the browser's Back.
	b.click(fmt.Sprintf(`.narrow-list .row[data-thread="%d"]`, num[0]))
	b.until("the failure and its terminal", fmt.Sprintf(`!!document.querySelector('[data-narrow-thread="%d"] [data-block=failure] [data-field=reason]')?.textContent && !!document.querySelector('[data-narrow-thread="%d"] .narrow-pane .xterm')`, num[0], num[0]))
	b.noSideScroll("a failed thread")
	b.eval(`history.back(), true`, nil)
	b.until("the list again after Back", listShown)

	// A running thread: its terminal, live.
	b.click(fmt.Sprintf(`.narrow-list .row[data-thread="%d"]`, num[3]))
	b.until("the running thread's terminal", fmt.Sprintf(`!!document.querySelector('[data-narrow-thread="%d"] .narrow-pane .xterm')`, num[3]))
	b.noSideScroll("a running thread")
	b.click(`.narrow-back`)
	b.until("the list again", listShown)

	b.until("the usage limits in the status bar", `!!document.querySelector('.status .limit[data-agent=fake] [data-window=seven_day]')`)
	b.noSideScroll("the list with usage limits and a long title")

	// A reload with a thread open keeps it open; Back and Forward move
	// between the list and threads, and the wide page shows the same one.
	threadShown := func(n int64) string {
		return fmt.Sprintf(`!!document.querySelector('[data-narrow-thread="%d"]')`, n)
	}
	b.click(fmt.Sprintf(`.narrow-list .row[data-thread="%d"]`, num[2]))
	b.until("the question", threadShown(num[2]))
	b.call("Page.reload", map[string]any{})
	b.until("the question again after a reload", threadShown(num[2]))
	b.eval(`history.back(), true`, nil)
	b.until("the list after Back", listShown)
	b.eval(`history.forward(), true`, nil)
	b.until("the question after Forward", threadShown(num[2]))
	b.call("Emulation.clearDeviceMetricsOverride", map[string]any{})
	activeTab := `document.querySelector('.tabbar .tab-main[aria-selected=true]')?.closest('.tab').dataset.tab`
	b.until("the same thread's tab in the wide page", fmt.Sprintf(`document.querySelector('.shell')?.dataset.layout === 'wide' && %s === 'thread:%d'`, activeTab, num[2]))
	// A thread chosen in the wide page is the one the narrow page shows.
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, num[3]))
	b.until("the running thread's tab", fmt.Sprintf(`%s === 'thread:%d'`, activeTab, num[3]))
	b.phone()
	b.until("the running thread in the narrow page", threadShown(num[3]))
	b.click(`.narrow-back`)
	b.until("the list again", listShown)

	// 3. The same address serves both layouts.
	var path string
	b.eval(`location.pathname + location.search`, &path)
	if path != "/" {
		t.Fatalf("the narrow page is at %q, want /", path)
	}
	b.call("Emulation.clearDeviceMetricsOverride", map[string]any{})
	b.call("Page.reload", map[string]any{})
	b.until("the wide page at the same address", `document.querySelector('.shell')?.dataset.layout === 'wide' && !!document.querySelector('.tabbar') && location.pathname === '/'`)
	b.phone()
	b.call("Page.reload", map[string]any{})
	b.until("the narrow page again", `document.querySelector('.shell')?.dataset.layout === 'narrow' && !!document.querySelector('.narrow-list') && location.pathname === '/'`)

	var v []string
	b.eval(`window.__csp`, &v)
	if len(v) > 0 {
		t.Fatalf("Content-Security-Policy violations: %q", v)
	}
}

// A session without a terminal, opened from the narrow list, shows its
// rebuilt output, with no sideways scroll, and a way back.
func TestDashboardNarrowRebuiltOutput(t *testing.T) {
	root := newRoot(t) // the fake agent on pipes: no terminal
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	c, ids, b := narrowRig(t, root, []string{"tool quick 10ms\nreply a line long enough to wrap on a phone held upright, which is the point of this line"},
		func(c *wire.Client, ids []control.ThreadID) bool { return stateOf(c, ids[0]).State == control.Idle })
	n := int64(stateOf(c, ids[0]).ID)
	b.click(`.narrow-list [data-section=rest] summary`)
	b.click(fmt.Sprintf(`.narrow-list .row[data-thread="%d"]`, n))
	b.until("the rebuilt output", fmt.Sprintf(`document.querySelectorAll('[data-narrow-thread="%d"] .narrow-pane [data-events] .ev').length >= 5 && !document.querySelector('.narrow-pane .xterm')`, n))
	b.noSideScroll("the rebuilt output")
	b.click(`.narrow-back`)
	b.until("the list again", `!!document.querySelector('.narrow-list') && !document.querySelector('.narrow-thread')`)
}
