package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// barLayout is where the tab bar's parts are, in CSS pixels.
type barLayout struct {
	Active                  string  // the active tab
	Shown, Width            float64 // of the active tab: the part in view, and the whole
	Hit                     bool    // a click in the middle of that part reaches the tab
	Tools                   int     // how many tools there are
	Missed                  string  // the tools that a click in their middle does not reach
	TabsTop, ToolsTop       float64
	TabsBottom, ToolsBottom float64
	TabsRight, ToolsLeft    float64
}

func (b *browser) barLayout() barLayout {
	b.t.Helper()
	var l barLayout
	b.eval(`(() => {
		const box = s => document.querySelector(s).getBoundingClientRect();
		const reaches = (e, x, y) => { const at = document.elementFromPoint(x, y); return !!at && e.contains(at); };
		const tabs = box('.tabbar .tabs'), tools = box('.tabbar .tools');
		const tab = document.querySelector('.tabbar .tab.active'), r = tab.getBoundingClientRect();
		const left = Math.max(r.left, tabs.left), right = Math.min(r.right, tabs.right);
		const all = [...document.querySelectorAll('.tabbar .tools button')];
		return {active: tab.dataset.tab, shown: Math.max(0, right - left), width: r.width, hit: reaches(tab, (left + right) / 2, (r.top + r.bottom) / 2),
			tools: all.length, missed: all.filter(e => { const t = e.getBoundingClientRect(); return !reaches(e, (t.left + t.right) / 2, (t.top + t.bottom) / 2); }).map(e => e.textContent).join(', '),
			tabsTop: tabs.top, toolsTop: tools.top, tabsBottom: tabs.bottom, toolsBottom: tools.bottom, tabsRight: tabs.right, toolsLeft: tools.left};
	})()`, &l)
	return l
}

// The tab bar in a 1280 px window (#714), with the tree and the details
// panel, both hidden counts, and more tabs than fit, in both languages:
// whichever tab becomes active is in view, the whole of it or at least 120
// px, also when the window or the panels change the bar's width; every
// other one is reached with the keys; every tool can be clicked. In a 1920
// px window the tabs and the tools stand on one line.
func TestDashboardTabBarNarrowWindow(t *testing.T) {
	rig := newNamesRig(t)
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	// width sets the window's width, and waits until the page was drawn at
	// it: a size that no frame was drawn at is none the page can see.
	width := func(w int) {
		b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": 900, "deviceScaleFactor": 1, "mobile": false})
		b.until("the width", fmt.Sprintf("window.innerWidth === %d", w))
		b.eval(`window.__drawn = false, requestAnimationFrame(() => requestAnimationFrame(() => { window.__drawn = true; })), true`, nil)
		b.until("a frame at that width", "window.__drawn")
	}
	width(1280)
	b.call("Page.navigate", map[string]any{"url": rig.link})
	b.until("the page", `!!document.querySelector('.tabbar')`)
	// inView waits until the active tab k is in view and can be clicked.
	inView := func(when, k string) {
		t.Helper()
		b.until(k+" in view "+when, fmt.Sprintf(`(() => {
			const tab = document.querySelector('.tabbar .tab.active'), tabs = document.querySelector('.tabbar .tabs').getBoundingClientRect();
			if (tab?.dataset.tab !== %q) return false;
			const r = tab.getBoundingClientRect(), shown = Math.min(r.right, tabs.right) - Math.max(r.left, tabs.left);
			return shown >= Math.min(r.width, 120) - 1;
		})()`, k))
		if l := b.barLayout(); !l.Hit {
			t.Fatalf("%s: a click on the active tab does not reach it: %+v", when, l)
		}
	}
	oneLine := func(when string) {
		t.Helper()
		if l := b.barLayout(); l.ToolsTop >= l.TabsBottom || l.ToolsBottom <= l.TabsTop || l.TabsRight > l.ToolsLeft {
			t.Fatalf("%s: the tabs and the tools are not on one line: %+v", when, l)
		}
	}
	for _, lang := range []string{"en", "ja"} {
		b.eval(`window.__tabBarTestPrevious = true`, nil)
		b.call("Page.navigate", map[string]any{"url": rig.base + "/?lang=" + lang})
		b.until("the page in "+lang, `!window.__tabBarTestPrevious && document.documentElement.lang === `+js(lang))
		b.showAll()
		openThread(t, b, rig.full)
		b.click(`.tools .show [data-choice=active]`)
		b.until("the panels and both counts", `!!document.querySelector('.tree') && !!document.querySelector('.details') &&
			!!document.querySelector('.tools .hidden-count') && !!document.querySelector('.tools [data-hidden-terminals]')`)
		full := fmt.Sprintf("thread:%d", rig.full)
		inView(lang+", two tabs", full)
		l := b.barLayout()
		t.Logf("%s, two tabs: %+v", lang, l)
		if l.Shown < l.Width-1 && l.Shown < 120 {
			t.Fatalf("%s: %v px of the active tab's %v are in view", lang, l.Shown, l.Width)
		}
		if l.Tools != 5 || l.Missed != "" {
			t.Fatalf("%s: %d tools, want 5; a click does not reach: %s", lang, l.Tools, l.Missed)
		}
		width(1920)
		inView(lang+", two tabs, 1920 px", full)
		oneLine(lang + ", two tabs, 1920 px")
		width(1280)

		// More tabs than fit.
		b.showAll()
		ids := []control.ThreadID{rig.asleep}
		for _, x := range rig.legacy {
			ids = append(ids, x.id)
		}
		for _, id := range ids {
			openThread(t, b, id)
			inView(lang+", opened", fmt.Sprintf("thread:%d", id))
		}
		b.click(`.tools .show [data-choice=active]`)
		open, _ := b.tabsOf()
		if len(open) != 2+len(ids) {
			t.Fatalf("%s: the tabs %v, want %d", lang, open, 2+len(ids))
		}
		b.eval(`document.querySelector('.tabbar .tab.active .tab-main').focus(), true`, nil)
		for _, k := range open {
			// From the last tab, the right arrow goes to the first.
			b.key("ArrowRight", "ArrowRight", 39, 0, "")
			inView(lang+", with the keys", k)
		}
		for _, k := range []string{open[0], open[len(open)-1]} {
			if k == open[0] {
				b.key("Home", "Home", 36, 0, "")
			} else {
				b.key("End", "End", 35, 0, "")
			}
			inView(lang+", with Home and End", k)
		}
		last := open[len(open)-1]
		// Another tab grows: the active one, the last, stays in view, but
		// is not pulled back after it was scrolled away.
		grow := func(px int) {
			b.eval(fmt.Sprintf(`document.querySelector('.tabbar .tab').style.minWidth = '%dpx', window.__drawn = false, requestAnimationFrame(() => requestAnimationFrame(() => { window.__drawn = true; })), true`, px), nil)
			b.until("a frame with the grown tab", "window.__drawn")
		}
		grow(500)
		inView(lang+", another tab grown", last)
		b.eval(`document.querySelector('.tabbar .tabs').scrollLeft = 0, true`, nil)
		grow(600)
		var left float64
		if b.eval(`document.querySelector('.tabbar .tabs').scrollLeft`, &left); left != 0 {
			t.Fatalf("%s: the active tab was pulled back to %v after it was scrolled away", lang, left)
		}
		grow(0)
		b.eval(`document.querySelector('.tabbar .tabs').scrollLeft = 10000, true`, nil)
		inView(lang+", scrolled back", last)
		width(1920)
		inView(lang+", many tabs, 1920 px", last)
		oneLine(lang + ", many tabs, 1920 px")
		width(1280)
		inView(lang+", many tabs, back to 1280 px", last)
		if l := b.barLayout(); l.Missed != "" {
			t.Fatalf("%s: a click does not reach: %s", lang, l.Missed)
		}
	}
}

// #990: real fake-agent turns provide running/waiting tabs, along with
// enough open tabs to overflow. Check both languages/themes, keyboard and
// pointer access to the list, and the one-line status and icon tools.
func TestDashboardCompactTabBar(t *testing.T) {
	rig := newNamesRig(t)
	var ids []control.ThreadID
	for i, text := range []string{"tool long 10m", "ask Which sample color?"} {
		id, err := rig.c.Start(context.Background(), control.StartRequest{Key: fmt.Sprintf("compact-%d", i), Scope: "c1", Repo: bareRepo(t), Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	b := newBrowser(t)
	// Use real pointer events here: element.click() neither focuses an
	// outside control nor dispatches the pointerdown that dismisses menus.
	click := func(selector string) {
		t.Helper()
		var at struct{ X, Y float64 }
		b.eval(fmt.Sprintf(`(() => {
			const e = document.querySelector(%q); e.scrollIntoView({block: 'nearest'});
			const r = e.getBoundingClientRect(); return {x: r.left + r.width/2, y: r.top + r.height/2};
		})()`, selector), &at)
		for _, kind := range []string{"mousePressed", "mouseReleased"} {
			b.call("Input.dispatchMouseEvent", map[string]any{"type": kind, "x": at.X, "y": at.Y, "button": "left", "clickCount": 1})
		}
	}
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": rig.link})
	b.until("the page", `!!document.querySelector('.tabbar')`)
	for _, lang := range []string{"ja", "en"} {
		b.eval(`window.__oldCompactPage = true`, nil)
		b.call("Page.navigate", map[string]any{"url": rig.base + "/?lang=" + lang})
		b.until("the language", `!window.__oldCompactPage && document.documentElement.lang === `+js(lang))
		b.showAll()
		for _, id := range append([]control.ThreadID{rig.full, rig.asleep}, ids...) {
			openThread(t, b, id)
		}
		for _, x := range rig.legacy {
			openThread(t, b, x.id)
		}
		b.until("running and waiting tabs", `!!document.querySelector('.tab .elapsed') && !!document.querySelector('.tab [data-badge=question]')`)
		b.click(`.tools [data-choice=active]`)
		for _, width := range []int{1024, 1280, 1920} {
			b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": 900, "deviceScaleFactor": 1, "mobile": false})
			for _, theme := range []string{"light", "dark"} {
				b.eval(fmt.Sprintf(`document.documentElement.dataset.theme = %q, true`, theme), nil)
				var got struct {
					Height, Room, Scroll float64
					Bad                  []string
				}
				b.eval(`(() => {
					const bar = document.querySelector('.tabbar'), tabs = bar.querySelector('.tabs');
					const bad = [];
					if (getComputedStyle(tabs).scrollbarWidth !== 'none' && getComputedStyle(tabs, '::-webkit-scrollbar').display !== 'none') bad.push('scrollbar');
					for (const el of bar.querySelectorAll('.tab-title, .elapsed, .badge, .tab-operating')) {
						if (getComputedStyle(el).whiteSpace !== 'nowrap') bad.push('wrap: ' + el.className);
					}
					for (const el of bar.querySelectorAll('.tools button, [data-tool=tab-list]')) {
						const r = el.getBoundingClientRect();
						if (!el.title || el.getAttribute('aria-label') !== el.title || !el.querySelector('svg') || r.width !== 28 || r.height !== 28) bad.push('icon tool');
					}
					return {height: bar.getBoundingClientRect().height, room: tabs.clientWidth, scroll: tabs.scrollWidth, bad};
				})()`, &got)
				if got.Height < 32 || got.Height > 36 || got.Room < 120 || got.Scroll <= got.Room || len(got.Bad) != 0 {
					t.Fatalf("%s %s %d: %+v", lang, theme, width, got)
				}
			}
		}
		open, _ := b.tabsOf()
		// Every overflowed tab is selectable from the list by pointer.
		for i, k := range open {
			click(`[data-tool=tab-list]`)
			b.until("all open tabs listed", fmt.Sprintf(`document.querySelectorAll('.tab-menu [role=menuitemradio]').length === %d`, len(open)))
			click(fmt.Sprintf(`.tab-menu button:nth-child(%d)`, i+1))
			b.until("chosen tab active and focused", fmt.Sprintf(`document.activeElement?.closest('.tab')?.dataset.tab === %q && document.querySelector('.tab.active')?.dataset.tab === %q && !document.querySelector('.tab-menu')`, k, k))
		}
		b.eval(`document.querySelector('[data-tool=tab-list]').focus(), true`, nil)
		b.key("ArrowDown", "ArrowDown", 40, 0, "")
		b.until("menu focus", `document.activeElement?.getAttribute('role') === 'menuitemradio'`)
		b.key("Home", "Home", 36, 0, "")
		b.until("menu Home", `document.activeElement === document.querySelector('.tab-menu button')`)
		b.key("End", "End", 35, 0, "")
		b.until("menu End", `document.activeElement === document.querySelector('.tab-menu button:last-child')`)
		b.key("ArrowDown", "ArrowDown", 40, 0, "")
		b.until("menu wrap", `document.activeElement === document.querySelector('.tab-menu button')`)
		b.key("Escape", "Escape", 27, 0, "")
		b.until("Escape returns focus", `!document.querySelector('.tab-menu') && document.activeElement?.dataset.tool === 'tab-list'`)
		click(`[data-tool=tab-list]`)
		click(`[data-tool=tree]`)
		b.until("outside closes menu", `!document.querySelector('.tab-menu')`)
		click(`[data-tool=tree]`)
		// Re-selecting the active tab must consume its focus request now,
		// rather than stealing focus from the person's next tree selection.
		click(`[data-tool=tab-list]`)
		click(`.tab-menu [aria-checked=true]`)
		click(fmt.Sprintf(`.tree [data-thread="%d"]`, rig.full))
		b.until("tree selection keeps focus", fmt.Sprintf(`document.querySelector('.tab.active')?.dataset.tab === 'thread:%d' && !!document.activeElement?.closest('.tree')`, rig.full))
	}
}
