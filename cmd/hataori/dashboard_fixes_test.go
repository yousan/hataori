package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// The four fixes of #603, driven by the real server with the fake surface,
// the fake agent and a fake list of host terminals:
//
//  1. every count of hidden threads on the page is the number of threads
//     the choice hides; the idle shell panes it hides are counted apart;
//  2. a scope's header shows at most 12 state dots, then "+N", and the
//     dots never cut the scope's name;
//  3. a column of the All threads table with no value for any thread shown
//     is left out, and the table fits a window 1500 px wide;
//  4. with every thread hidden, the All threads tab says so, with a button
//     that shows them all.
func TestDashboardFourFixes(t *testing.T) {
	const idleShells = 3
	terms := &listedTerms{}
	for i := range idleShells {
		terms.list = append(terms.list, control.HostTerminal{Device: fmt.Sprintf("/dev/pts/%d", 951+i), Process: "bash", Activity: ago(72 * time.Hour),
			Tmux: &control.TmuxPane{Pane: fmt.Sprintf("%%%d", 51+i), Session: "work", Window: fmt.Sprint(i), WindowName: "w", Index: "0"}})
	}
	hostTermsSource = terms
	t.Cleanup(func() { hostTermsSource = nil })
	// A name that fits the tree's width whole, beside its count, and that
	// sixty dots would cut.
	const scopeName = "hataori-dashboard-work"
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["host_terminals"] = true
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		cfg["scopes"].([]map[string]any)[0]["name"] = scopeName
	})
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repo := bareRepo(t)

	// Sixty threads in the scope, each asleep at once: all hidden by the
	// Active choice.
	const asleep = 60
	var wg sync.WaitGroup
	ids := make([]control.ThreadID, asleep)
	errs := make([]error, asleep)
	for i := range asleep {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = c.Start(ctx, control.StartRequest{Key: fmt.Sprintf("k-asleep-%d", i), Scope: "c1", Repo: repo, Input: control.Input{Text: "exit"}})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the sixty threads asleep", 2*time.Minute, func() bool {
		ts, err := c.Threads(ctx, control.Filter{})
		n := 0
		for _, x := range ts {
			if x.State == control.Asleep {
				n++
			}
		}
		return err == nil && n == asleep
	})

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1500, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})

	// counts is every count of hidden things the page shows.
	type counts struct {
		Bar, BarTerminals string // the tab bar's, "" when not shown
		Tree              string // the tree's line when no thread is shown
	}
	countsOf := func() counts {
		var s counts
		b.eval(`(() => ({
			bar: document.querySelector('.tools .show .hidden-count')?.dataset.hidden ?? '',
			barTerminals: document.querySelector('.tools .show .hidden-terminals')?.dataset.hiddenTerminals ?? '',
			tree: document.querySelector('[data-section=threads] .none')?.textContent ?? '',
		}))()`, &s)
		return s
	}

	// 1 and 4. Every thread hidden: the tab bar and the tree say 60, the
	// idle shells are counted apart; the All threads tab says so, with a
	// button.
	want := counts{Bar: fmt.Sprint(asleep), BarTerminals: fmt.Sprint(idleShells), Tree: fmt.Sprintf("No active threads. %d hidden.", asleep)}
	var got counts
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got = countsOf(); got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the counts are %+v, want %+v", got, want)
		}
	}
	b.until("the All threads tab's empty state", fmt.Sprintf(`(() => {
		const e = document.querySelector('.pane.active .table-empty');
		return !!e && !document.querySelector('.pane.active .all-threads') && e.querySelector('p').textContent === 'No active threads. %d hidden.' &&
			e.querySelector('button[data-show-all]')?.textContent === 'Show all';
	})()`, asleep))
	b.click(`.pane.active .table-empty button[data-show-all]`)
	b.until("every thread in the table after one click", fmt.Sprintf(`document.querySelector('.tools .show [aria-pressed=true]')?.dataset.choice === 'all' &&
		document.querySelectorAll('.pane.active .all-threads tr[data-thread]').length === %d`, asleep))

	// 2. Sixty threads in the scope: twelve dots, "+48", and the name whole.
	type head struct {
		Dots       int
		More, Name string
		Cut        bool
		MoreShown  bool // "+N" is within the header, not cut off
	}
	var h head
	headOf := func() head {
		var v head
		b.eval(`(() => {
			const n = document.querySelector('.tree [data-scope=c1] .node-head');
			const name = n?.querySelector('.scope-name');
			return {dots: n ? n.querySelectorAll('.dots .dot').length : -1, more: n?.querySelector('.dots-more')?.textContent ?? '',
				name: name?.textContent ?? '', cut: !name || name.scrollWidth > name.clientWidth,
				moreShown: (() => { const m = n?.querySelector('.dots-more'); return !!m && m.getBoundingClientRect().right <= n.getBoundingClientRect().right; })()};
		})()`, &v)
		return v
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if h = headOf(); h.Dots == 12 && h.More == "+48" && strings.TrimPrefix(h.Name, "#") == scopeName {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scope's header: %+v; want 12 dots, +48 and the name", h)
		}
	}
	if h.Dots != 12 || h.More != "+48" || h.Cut || !h.MoreShown {
		t.Fatalf("the scope's header: %+v; want 12 dots, +48 and the name whole", h)
	}

	// 3. No thread reports its context use: no Context column. One that
	// does brings it back.
	columns := func() string {
		var s string
		b.eval(`document.querySelector('.pane.active .all-threads')?.dataset.columns ?? ''`, &s)
		return s
	}
	if cols := columns(); strings.Contains(cols, "context") || !strings.Contains(cols, "title") {
		t.Fatalf("the table's columns are %q: Context has no value for any thread", cols)
	}
	if n := countOf(b, `.pane.active .all-threads [data-col=context]`); n != 0 {
		t.Fatalf("%d cells of the Context column", n)
	}
	// Two threads with long titles and long last lines, one with its
	// context use.
	long := strings.Repeat("a long line of words that goes on ", 8)
	for i, text := range []string{"reply " + long + "one", "usage 1200 300 40000 5000 45000 200000 m\nreply " + long + "two"} {
		if _, err := c.Start(ctx, control.StartRequest{Key: fmt.Sprintf("k-long-%d", i), Scope: "c1", Repo: repo, Input: control.Input{Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	b.until("the Context column back", `(document.querySelector('.pane.active .all-threads')?.dataset.columns ?? '').includes('context') &&
		[...document.querySelectorAll('.pane.active .all-threads [data-col=context] [role=meter]')].length === 1`)

	// 3. In a window 1500 px wide, with the tree and without the details:
	// the table does not scroll sideways, and a long value is cut, whole
	// in its tooltip. The last output is the long value: the title, cut to
	// 80 characters, shows whole where the table has room (#631).
	b.until("the long threads' last lines", `[...document.querySelectorAll('.pane.active .all-threads td[data-col=last]')].filter(td => td.textContent.startsWith('a long line')).length === 2`)
	b.click(`.tools .show [data-choice=active]`)
	b.until("only the two active threads", `document.querySelectorAll('.pane.active .all-threads tr[data-thread]').length === 2`)
	// fits checks the table in the window as it is laid out now.
	fits := func(what string, details bool) {
		t.Helper()
		var fit struct {
			Window, Pane, Scroll float64
			Tree, Details        bool
			CutLast, Tooltip     bool
		}
		b.eval(fmt.Sprintf(`(() => {
			const p = document.querySelector('.pane.active');
			const td = p.querySelector('.all-threads td[data-col=last]');
			return {window: innerWidth, pane: p.clientWidth, scroll: p.scrollWidth,
				tree: !!document.querySelector('.tree'), details: !!document.querySelector('.details'),
				cutLast: td.scrollWidth > td.clientWidth, tooltip: td.title.startsWith(%q)};
		})()`, long[:40]), &fit)
		if fit.Window != 1500 || !fit.Tree || fit.Details != details {
			t.Fatalf("%s: the window is %+v: the check would test another layout", what, fit)
		}
		if fit.Scroll > fit.Pane {
			t.Fatalf("%s: the All threads table scrolls sideways in a window 1500 px wide: %v px in %v", what, fit.Scroll, fit.Pane)
		}
		if !fit.CutLast || !fit.Tooltip {
			t.Fatalf("%s: a long last output: cut %v, whole in its tooltip %v", what, fit.CutLast, fit.Tooltip)
		}
	}
	fits("with the details panel", true)
	b.click(`.tools [data-tool=details]`)
	b.until("the details panel hidden", `!document.querySelector('.details')`)
	fits("without the details panel", false)

	// 1. The narrow list counts the same: the threads, and the idle
	// shells apart.
	b.phone()
	b.until("the narrow list's counts", fmt.Sprintf(`document.querySelector('.narrow-show .hidden-count')?.dataset.hidden === '%d' &&
		document.querySelector('.narrow-show .hidden-terminals')?.dataset.hiddenTerminals === '%d'`, asleep, idleShells))
}

// countOf is how many elements selector finds.
func countOf(b *browser, selector string) int {
	b.t.Helper()
	var n int
	b.eval(fmt.Sprintf(`document.querySelectorAll(%q).length`, selector), &n)
	return n
}
