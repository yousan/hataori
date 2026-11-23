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

// tabsOf is the keys of the tab bar's tabs, in order, and of the active one.
func (b *browser) tabsOf() (open []string, active string) {
	b.t.Helper()
	var r struct {
		Open   []string
		Active string
	}
	b.eval(`({open: [...document.querySelectorAll('.tabbar .tab')].map(x => x.dataset.tab),
		active: (document.querySelector('.tabbar .tab-main[aria-selected=true]')?.closest('.tab').dataset.tab) ?? ''})`, &r)
	return r.Open, r.Active
}

// click clicks the element that selector names.
func (b *browser) click(selector string) {
	b.t.Helper()
	var ok bool
	b.eval(fmt.Sprintf(`(() => { const e = document.querySelector(%q); if (!e) return false; e.click(); return true; })()`, selector), &ok)
	if !ok {
		var html string
		b.eval("document.body.innerHTML", &html)
		b.t.Fatalf("nothing to click at %s\n%s", selector, html)
	}
}

// The tab bar and the panes, driven by the real server: "All threads"
// cannot be closed; a tab's badge says "question" or "failed" while its
// thread is in that state and goes when it is not; split shows the active
// tab and those viewed before it; hiding the tree and the details gives
// the main area their width, and the choice survives a reload.
func TestDashboardTabsAndPanes(t *testing.T) {
	root := terminalRoot(t)
	// The fake agent's first start exits at once while its session is
	// being created: that thread fails. Later starts run the fake agent.
	once := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(once, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setConfig(t, root, func(cfg map[string]any) {
		ad := cfg["adapters"].(map[string]any)[fake.Name].(map[string]any)
		// The path is an argument, not shell code.
		ad["command"] = []string{"sh", "-c", `once=$1; shift; if [ -e "$once" ]; then rm -f "$once"; exit 3; fi; exec "$@"`, "sh", once, os.Args[0], "fake"}
	})
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	repo := bareRepo(t)
	start := func(key, text string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: "k-" + key, Scope: "s", Thread: key, Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	state := func(id control.ThreadID) control.ThreadInfo {
		t.Helper()
		d, err := c.Thread(ctx, id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return d.ThreadInfo
	}
	failing := start("t1", "reply never")
	eventually(t, "the first thread to fail", 20*time.Second, func() bool { return state(failing).State == control.Failed })
	asking := start("t2", "ask Ship it?")
	idle := start("t3", "reply done")
	running := start("t4", "tool long 10m")
	eventually(t, "the threads to settle", 20*time.Second, func() bool {
		return state(asking).State == control.Waiting && state(idle).State == control.Idle && state(running).State == control.Running
	})
	nF, nQ, nI, nR := int64(state(failing).ID), int64(state(asking).ID), int64(state(idle).ID), int64(state(running).ID)
	tab := func(n int64) string { return fmt.Sprintf("thread:%d", n) }
	row := func(n int64) string { return fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, n) }

	b := newBrowser(t)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the four threads", `document.querySelectorAll('[data-section=threads] .row').length === 4`)

	// 1. "All threads" is pinned first, has no close button, and stays
	// when every other tab is closed.
	open, active := b.tabsOf()
	if len(open) != 1 || open[0] != "all" || active != "all" {
		t.Fatalf("tabs at the start: %v, active %q", open, active)
	}
	for _, n := range []int64{nQ, nF, nI, nR} {
		b.click(row(n))
	}
	b.until("four thread tabs after All threads", fmt.Sprintf(
		`JSON.stringify([...document.querySelectorAll('.tabbar .tab')].map(x => x.dataset.tab)) === %q`, js([]string{"all", tab(nQ), tab(nF), tab(nI), tab(nR)})))
	b.until("All threads without a close button", `!document.querySelector('.tab[data-tab=all] .tab-close') && document.querySelectorAll('.tab .tab-close').length === 4`)

	// 2. Badges: "question" on the asking thread, "failed" on the failed
	// one, none on the others; the question's goes once it is answered.
	badge := func(n int64, want string) string {
		return fmt.Sprintf(`(() => {
			const b = document.querySelector('.tab[data-tab="%s"] [data-badge]');
			return %q === '' ? !b : !!b && b.dataset.badge === %q && b.textContent === %q;
		})()`, tab(n), want, want, want)
	}
	b.until("the question badge", badge(nQ, "question"))
	b.until("the failed badge", badge(nF, "failed"))
	b.until("no badge on the idle thread", badge(nI, ""))
	b.until("no badge on the running thread", badge(nR, ""))
	b.until("the running thread's elapsed time", fmt.Sprintf(`!!document.querySelector('.tab[data-tab="%s"] .elapsed') && !document.querySelector('.tab[data-tab="%s"] .elapsed')`, tab(nR), tab(nI)))
	d, err := c.Thread(ctx, asking, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Requests) != 1 {
		t.Fatalf("the question: %+v", d.Requests)
	}
	if err := c.Answer(ctx, control.RequestID(d.Requests[0].ID), control.Answer{Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answered thread to leave waiting", 20*time.Second, func() bool { return state(asking).State != control.Waiting })
	b.until("the question badge gone", badge(nQ, ""))
	b.until("the failed badge still there", badge(nF, "failed"))

	// 3. Split: the active tab, then the ones viewed before it. The views go
	// in another order than the tabs were opened.
	for _, n := range []int64{nI, nQ, nR} {
		b.click(fmt.Sprintf(`.tab[data-tab="%s"] .tab-main`, tab(n)))
	}
	panesAre := func(want ...string) string {
		return fmt.Sprintf(`JSON.stringify([...document.querySelectorAll('.panes .pane')].map(p => p.dataset.tab)) === %q`, js(want))
	}
	b.until("one pane with the active tab", panesAre(tab(nR)))
	b.click(`[data-tool=split]`)
	b.until("two panes: the active tab and the one before", panesAre(tab(nR), tab(nQ)))
	b.click(`[data-tool=split]`)
	b.until("three panes: the active tab and the two before", panesAre(tab(nR), tab(nQ), tab(nI)))
	b.click(`[data-tool=split]`)
	b.until("back to one pane", panesAre(tab(nR)))

	// From the keyboard: the arrows move along the tabs and view them, and
	// the active tab has the focus; Delete closes a thread's tab, not All
	// threads.
	key := func(k string) {
		t.Helper()
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": k, "code": k})
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k, "code": k})
	}
	focused := func(k string) string {
		return fmt.Sprintf(`document.activeElement?.closest('.tab')?.dataset.tab === %q && document.activeElement.getAttribute('aria-selected') === 'true'`, k)
	}
	b.eval(fmt.Sprintf(`document.querySelector('.tab[data-tab="%s"] .tab-main').focus(), true`, tab(nR)), nil)
	key("ArrowLeft")
	b.until("the tab to the left, viewed and focused", focused(tab(nI)))
	key("Home")
	b.until("All threads, viewed and focused", focused("all"))
	key("Delete")
	b.until("All threads still there after Delete", focused("all"))
	key("End")
	b.until("the last tab, viewed and focused", focused(tab(nR)))

	// Closing the active tab makes the one viewed before it active; closing
	// every thread's tab leaves All threads, active.
	key("Delete")
	b.until("the tab viewed before, active and focused", panesAre("all")+" && "+focused("all"))
	for _, n := range []int64{nQ, nF, nI} {
		b.click(fmt.Sprintf(`.tab[data-tab="%s"] .tab-close`, tab(n)))
	}
	b.until("All threads alone and active", `JSON.stringify([...document.querySelectorAll('.tabbar .tab')].map(x => x.dataset.tab)) === '["all"]'`+
		` && document.querySelector('.tab[data-tab=all] .tab-main').getAttribute('aria-selected') === 'true'`)
	if open, active = b.tabsOf(); len(open) != 1 || active != "all" {
		t.Fatalf("tabs after closing the others: %v, active %q", open, active)
	}

	// 4. Hiding the tree and the details gives the main area their width,
	// and the choice is kept across a reload. The details panel is there
	// beside a thread; beside All threads alone it takes no width (#631).
	b.until("no details panel beside All threads alone", `document.querySelector('[data-tool=details]').getAttribute('aria-pressed') === 'true' && !document.querySelector('.details')`)
	openThread(t, b, control.ThreadID(nI))
	b.until("the details panel beside the thread", `!!document.querySelector('.details')`)
	type widths struct{ Center, Tree, Details, Page float64 }
	measure := func() widths {
		var w widths
		b.eval(`(() => { const w = s => document.querySelector(s)?.getBoundingClientRect().width ?? 0;
			return {center: w('.center'), tree: w('.tree'), details: w('.details'), page: document.documentElement.clientWidth}; })()`, &w)
		return w
	}
	near := func(a, b float64) bool { return a-b < 1 && b-a < 1 }
	w0 := measure()
	if w0.Tree == 0 || w0.Details == 0 || !near(w0.Center+w0.Tree+w0.Details, w0.Page) {
		t.Fatalf("widths with the tree and the details shown: %+v", w0)
	}
	b.click(`[data-tool=tree]`)
	b.until("the tree hidden", `!document.querySelector('.tree')`)
	if w := measure(); !near(w.Center, w0.Center+w0.Tree) {
		t.Fatalf("without the tree the main area is %v wide, want %v", w.Center, w0.Center+w0.Tree)
	}
	b.click(`[data-tool=details]`)
	b.until("the details hidden", `!document.querySelector('.details')`)
	if w := measure(); !near(w.Center, w0.Page) {
		t.Fatalf("without the tree and the details the main area is %v wide, the page %v", w.Center, w0.Page)
	}
	b.call("Page.reload", map[string]any{})
	b.until("the page again, without the tree and the details", `!!document.querySelector('.tabbar') && !document.querySelector('.tree') && !document.querySelector('.details')`+
		` && document.querySelector('[data-tool=tree]').getAttribute('aria-pressed') === 'false' && document.querySelector('[data-tool=details]').getAttribute('aria-pressed') === 'false'`)
	if w := measure(); !near(w.Center, w0.Page) {
		t.Fatalf("after a reload the main area is %v wide, the page %v", w.Center, w0.Page)
	}
	b.click(`[data-tool=tree]`)
	b.until("the tree back", `!!document.querySelector('.tree')`)
	if w := measure(); !near(w.Center+w.Tree, w0.Page) || w.Tree == 0 {
		t.Fatalf("with the tree back: %+v", w)
	}
}
