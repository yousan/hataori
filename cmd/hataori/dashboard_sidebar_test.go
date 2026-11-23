package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// sidebarRig is a daemon whose fake surface reports a layout like a chat
// client's sidebar: the unserved channel #lobby with a plain thread, then
// the group "Work" with the served channel #builds. #builds holds, open in
// the chat, a running, an idle, a waiting and a failed hataori thread and
// a plain thread hataori does not hold; two more hataori threads are
// asleep, with their chat threads archived.
type sidebarRig struct {
	c                              *wire.Client
	running, idle, waiting, failed control.ThreadID
	asleepOld, asleepNew           control.ThreadID
	sidebar                        *browser
}

func newSidebarRig(t *testing.T) sidebarRig {
	t.Helper()
	once := filepath.Join(t.TempDir(), "fail-once")
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": fakeSurface, "enabled": true}}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		// The first start fails: the agent's command exits at once.
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{
			Command: []string{"sh", "-c", `once=$1; shift; if [ -e "$once" ]; then rm -f "$once"; exit 3; fi; exec "$@"`, "sh", once, os.Args[0], "fake"},
			Env:     []string{"PATH=" + os.Getenv("PATH")}}}
	})
	ld := r.driver.WithLayout()
	drivers.Store(r.root, ld)
	r.driver.SetName(surface.Conversation{Channel: "c1"}, "#builds")
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	repo := bareRepo(t)
	start := func(key, text string, state control.State) (control.ThreadID, string) {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: key, Scope: "c1", Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		var chat string
		eventually(t, "the thread "+key+" "+string(state), 30*time.Second, func() bool {
			ts, err := c.Threads(ctx, control.Filter{Thread: id})
			if err != nil || len(ts) != 1 || len(ts[0].Links) == 0 {
				return false
			}
			chat = ts[0].Links[0].Thread
			return ts[0].State == state
		})
		return id, chat
	}
	os.WriteFile(once, nil, 0o600)
	var rig sidebarRig
	var failedChat, runChat, idleChat, waitChat string
	rig.failed, failedChat = start("k-fail", "reply never", control.Failed)
	rig.asleepOld, _ = start("k-old", "exit", control.Asleep)
	rig.asleepNew, _ = start("k-new", "exit", control.Asleep)
	rig.idle, idleChat = start("k-idle", "limits 0 300 seven_day:10080:21:1795161600\nreply idle", control.Idle)
	rig.waiting, waitChat = start("k-wait", "ask Proceed with the deploy?", control.Waiting)
	rig.running, runChat = start("k-run", "tool long 10m", control.Running)
	rig.c = c
	ld.SetLayout(surface.Layout{Groups: []surface.LayoutGroup{
		{Channels: []surface.LayoutChannel{{ID: "u1", Name: "#lobby", URL: "https://chat.example/u1",
			Threads: []surface.LayoutThread{{ID: "l1", Name: "lunch", URL: "https://chat.example/l1"}}}}},
		{ID: "g1", Name: "Work", Channels: []surface.LayoutChannel{
			{ID: "c1", Name: "#builds", URL: "https://chat.example/c1", Threads: []surface.LayoutThread{
				{ID: runChat, Name: "tool long 10m"},
				{ID: idleChat, Name: "reply idle"},
				{ID: waitChat, Name: "ask Proceed with the deploy?"},
				{ID: failedChat, Name: "reply never"},
				{ID: "p1", Name: "a plain thread", URL: "https://chat.example/p1"},
			}},
		}},
	}})
	eventually(t, "the layout", 10*time.Second, func() bool {
		ls, err := c.Layout(ctx)
		return err == nil && len(ls) == 1 && len(ls[0].Groups) > 0
	})
	b := newBrowser(t)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the channel #builds", `!!document.querySelector('.tree [data-node="scope:c1"] .on-rail')`)
	rig.sidebar = b
	return rig
}

// railItem is one item on a channel's rail, as the page lays it out: its
// box, the part of the rail it draws (its ::before), and the middle of its
// first line.
type railItem struct {
	Item          string
	Top, Bottom   float64
	X             float64 // the middle of the rail's line
	RailTop       float64
	RailBottom    float64
	Mid           float64
	End           bool
	Style, Color  string
	Elbow         string // the colour of its line into the thread (::after)
	Background    string // the item's own background
	BoxBackground string
}

type railLayout struct {
	HashX, HeadBottom float64
	Items             []railItem
	AfterArchived     string // what the archived row draws of a rail: "none"
}

// rail measures the rail of channel scope:c1.
func (b *browser) rail() railLayout {
	b.t.Helper()
	var l railLayout
	b.eval(`(() => {
		const node = document.querySelector('.tree [data-node="scope:c1"]');
		const head = node.querySelector(':scope > .node-head');
		const hash = head.querySelector('.hash').getBoundingClientRect();
		const items = [...node.querySelectorAll(':scope > .node-body > .on-rail')].map(li => {
			const b = li.getBoundingClientRect();
			const cs = getComputedStyle(li, '::before');
			const first = (li.querySelector('.line1') ?? li.querySelector('.box')).getBoundingClientRect();
			const top = b.top + parseFloat(cs.top);
			return {
				item: li.dataset.treeItem, top: b.top, bottom: b.bottom,
				x: b.left + parseFloat(cs.left) + parseFloat(cs.borderLeftWidth) / 2,
				railTop: top, railBottom: top + parseFloat(cs.height), mid: first.top + first.height / 2,
				end: li.classList.contains('rail-end'), style: cs.borderLeftStyle, color: cs.borderLeftColor,
				elbow: getComputedStyle(li, '::after').borderBottomColor,
				background: getComputedStyle(li).backgroundColor,
				boxBackground: getComputedStyle(li.querySelector('.box')).backgroundColor,
			};
		});
		const archived = node.querySelector('.archived-row');
		return {
			hashX: (hash.left + hash.right) / 2, headBottom: head.getBoundingClientRect().bottom, items,
			afterArchived: archived ? getComputedStyle(archived, '::before').content : 'none',
		};
	})()`, &l)
	return l
}

// near reports whether a and b are within half a pixel and a bit.
func near(a, b float64) bool { return math.Abs(a-b) <= 1.5 }

// checkRail checks that the rail is one straight line under the channel's
// "#", from under its head to the middle of the first line of its last
// item, through items that touch, and that it is items, in this order.
func (b *browser) checkRail(what string, items []string) railLayout {
	b.t.Helper()
	var l railLayout
	var why string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		l = b.rail()
		if why = railProblem(l, items); why == "" {
			return l
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: %s\n%+v", what, why, l)
		}
	}
}

func railProblem(l railLayout, items []string) string {
	if len(l.Items) != len(items) {
		return fmt.Sprintf("%d items on the rail, want %d", len(l.Items), len(items))
	}
	if len(items) == 0 {
		return ""
	}
	if !near(l.Items[0].RailTop, l.HeadBottom) {
		return fmt.Sprintf("the rail starts at %v, the channel's head ends at %v", l.Items[0].RailTop, l.HeadBottom)
	}
	for i, it := range l.Items {
		if it.Item != items[i] {
			return fmt.Sprintf("item %d is %s, want %s", i, it.Item, items[i])
		}
		if it.Style != "solid" || it.Color != l.Items[0].Color {
			return fmt.Sprintf("%s draws a %s rail in %s", it.Item, it.Style, it.Color)
		}
		if !near(it.X, l.HashX) {
			return fmt.Sprintf("%s's rail is at x %v, the channel's # at %v", it.Item, it.X, l.HashX)
		}
		if !near(it.RailTop, it.Top) {
			return fmt.Sprintf("%s's rail starts at %v, the item at %v", it.Item, it.RailTop, it.Top)
		}
		last := i == len(l.Items)-1
		if it.End != last {
			return fmt.Sprintf("%s ends the rail: %v", it.Item, it.End)
		}
		if last {
			if !near(it.RailBottom, it.Mid) {
				return fmt.Sprintf("the rail ends at %v, the last item's first line's middle is %v", it.RailBottom, it.Mid)
			}
			continue
		}
		if !near(it.RailBottom, it.Bottom) || !near(l.Items[i+1].Top, it.Bottom) {
			return fmt.Sprintf("a gap in the rail after %s: it ends at %v, the item at %v, the next starts at %v",
				it.Item, it.RailBottom, it.Bottom, l.Items[i+1].Top)
		}
	}
	if l.AfterArchived != "none" {
		return "the archived row draws a rail: " + l.AfterArchived
	}
	return ""
}

// The tree as the chat client's sidebar (#635), driven by the real server
// with the fake surface's layout and the fake agent: the bar with the chat
// on top, the group label, the channel's "#" and count, the threads on one
// straight rail that follows folding, the dimmed thread hataori does not
// hold, the archived row, the attention block, and the bar with who signed
// in at the bottom; in both themes.
func TestDashboardSidebar(t *testing.T) {
	rig := newSidebarRig(t)
	b := rig.sidebar
	thread := func(id control.ThreadID) string { return fmt.Sprintf("thread:%d", id) }
	p1 := "chat:" + fakeSurface + "/p1"
	open := []string{thread(rig.running), thread(rig.idle), thread(rig.waiting), thread(rig.failed), p1}

	// The rail: one line under the "#", through each thread, ending at the
	// last one; the archived row hangs from none of it.
	b.checkRail("the rail", open)

	// Folding the channel takes the rail with its threads; unfolding brings
	// both back.
	b.click(`.tree [data-node="scope:c1"] > .node-head`)
	b.checkRail("the channel folded", nil)
	b.click(`.tree [data-node="scope:c1"] > .node-head`)
	b.checkRail("the channel unfolded", open)

	// The archived row: the two asleep threads, folded; opened, they are
	// listed in place, the newest first, and the rail goes on to the last;
	// folded again, it ends where it did.
	b.until("the archived row", `(() => {
		const a = document.querySelector('.tree [data-tree-item="archived:c1"]');
		return !!a && a.dataset.archived === '2' && a.textContent === '2 archived ›' && a.getAttribute('aria-expanded') === 'false';
	})()`)
	b.click(`.tree [data-tree-item="archived:c1"]`)
	withAsleep := append(append([]string{}, open...), thread(rig.asleepNew), thread(rig.asleepOld))
	b.checkRail("the asleep threads listed in place", withAsleep)
	b.until("no state dot on an asleep thread, one on each other", fmt.Sprintf(`(() => {
		const dot = n => document.querySelector('.tree .row[data-thread="' + n + '"] .line1 .dot');
		return !dot(%d) && !dot(%d) && [%d, %d, %d, %d].every(n => !!dot(n));
	})()`, rig.asleepNew, rig.asleepOld, rig.running, rig.idle, rig.waiting, rig.failed))
	b.click(`.tree [data-tree-item="archived:c1"]`)
	b.checkRail("the asleep threads folded again", open)

	// Each state has its own colour.
	var colours []string
	b.eval(fmt.Sprintf(`[%d, %d, %d, %d].map(n => getComputedStyle(document.querySelector('.tree .row[data-thread="' + n + '"] .line1 .dot')).backgroundColor)`,
		rig.running, rig.idle, rig.waiting, rig.failed), &colours)
	if len(colours) != 4 || colours[0] == colours[1] || colours[0] == colours[2] || colours[0] == colours[3] ||
		colours[1] == colours[2] || colours[1] == colours[3] || colours[2] == colours[3] {
		t.Fatalf("the state dots' colours: %v", colours)
	}

	// The selected thread's line into it is in the accent colour; the rail
	// stays as it is, and the row's box, not the item with its rail, takes
	// the selection.
	b.click(fmt.Sprintf(`.tree .row[data-thread="%d"]`, rig.idle))
	var accent string
	b.eval(`(() => { const s = document.createElement('span'); s.style.color = 'var(--accent)'; document.querySelector('.tree').append(s);
		const c = getComputedStyle(s).color; s.remove(); return c; })()`, &accent)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		l := b.rail()
		ok := len(l.Items) == len(open)
		for _, it := range l.Items {
			selected := it.Item == thread(rig.idle)
			ok = ok && (it.Elbow == accent) == selected && it.Color == l.Items[0].Color && it.Color != accent &&
				it.Background == "rgba(0, 0, 0, 0)" && (it.BoxBackground != "rgba(0, 0, 0, 0)") == selected
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the selected thread's line and box, accent %s: %+v", accent, l)
		}
	}

	// The thread hataori does not hold and the unserved channel are dimmed,
	// marked as opening in the chat, links into it; the served channel and
	// hataori's threads are not dimmed.
	b.until("the dimming", fmt.Sprintf(`(() => {
		const op = s => +getComputedStyle(document.querySelector(s)).opacity;
		const p1 = document.querySelector('.tree [data-tree-item=%q]');
		return op('.tree [data-tree-item=%q]') < 1 && op('.tree [data-tree-item="chan:%s/u1"]') < 1 &&
			p1.querySelector('.opens-in-chat')?.textContent === '↗' && p1.querySelector('a.chat-link')?.getAttribute('href') === 'https://chat.example/p1' &&
			op('.tree [data-node="scope:c1"]') === 1 && op('.tree .row[data-thread="%d"]') === 1;
	})()`, p1, p1, fakeSurface, rig.running))

	// Anywhere on that row opens the chat thread, its mark too. The click
	// is stopped before it navigates, so that the test's page stays.
	b.eval(`window.opened = []; document.addEventListener('click', e => {
		const a = e.target.closest('a.chat-link'); if (a) { e.preventDefault(); window.opened.push(a.getAttribute('href')); }
	}, true), true`, nil)
	b.click(`.tree [data-tree-item="` + p1 + `"] .opens-in-chat`)
	b.until("the chat thread opened from its mark", `JSON.stringify(window.opened) === '["https://chat.example/p1"]'`)

	// The group is an upper-case label; the channel has its "#" and the
	// count of its threads that are not asleep, whatever the filter shows.
	b.until("the group and the channel heads", `(() => {
		const g = document.querySelector('.tree [data-group] > .node-head');
		const c = document.querySelector('.tree [data-node="scope:c1"] > .node-head');
		return getComputedStyle(g).textTransform === 'uppercase' && !!g.querySelector('.fold-mark') &&
			c.querySelector('.hash')?.textContent === '#' && c.querySelector('.scope-name')?.textContent === 'builds' &&
			c.querySelector('.awake')?.dataset.awake === '4';
	})()`)
	b.filter("tool long")
	b.checkRail("filtered", []string{thread(rig.running)})
	b.until("the count while filtered", `document.querySelector('.tree [data-node="scope:c1"] > .node-head .awake')?.dataset.awake === '4'`)
	b.filter("")
	b.checkRail("the filter cleared", open)

	// Needs attention: the waiting and the failed thread, each with what it
	// waits on, above the groups.
	b.until("the attention block", fmt.Sprintf(`(() => {
		const a = document.querySelector('.tree [data-section=attention]');
		const why = n => a?.querySelector('.row[data-thread="' + n + '"] .why')?.textContent ?? '';
		const rows = [...(a?.querySelectorAll('.row') ?? [])].map(r => +r.dataset.thread).sort((x, y) => x - y);
		return !!a && a.compareDocumentPosition(document.querySelector('.tree [data-group]')) & Node.DOCUMENT_POSITION_FOLLOWING &&
			JSON.stringify(rows) === JSON.stringify([%d, %d].sort((x, y) => x - y)) &&
			why(%d) === 'Question: Proceed with the deploy?' && why(%d) !== '';
	})()`, rig.waiting, rig.failed, rig.waiting, rig.failed))

	// The bars: the chat's name with its connection up on top; at the
	// bottom, who signed in and how, and the remaining usage with its reset.
	b.until("the bar on top", `(() => {
		const s = document.querySelector('.tree > .server-bar .server[data-client="`+fakeSurface+`"]');
		return !!s && s.dataset.connected === 'yes' && s.textContent === '`+fakeSurface+`';
	})()`)
	b.until("the bar at the bottom", `(() => {
		const m = document.querySelector('.tree > .me-bar');
		const u = m?.querySelector('.usage[data-window="seven_day"]');
		return !!m && m.querySelector('.me-name')?.textContent === 'owner' &&
			m.querySelector('.me-how')?.textContent === 'sign-in link, this machine' &&
			!!u && u.textContent.startsWith('fake 7d 21% used') && !!u.querySelector('.usage-resets');
	})()`)

	// Both themes: the rail, the dots and the bars are drawn in each, in
	// colours of their own against the tree's background.
	for _, theme := range []string{"light", "dark"} {
		b.eval(`document.documentElement.dataset.theme = '`+theme+`', true`, nil)
		var got struct {
			Rail, Tree, Accent, Dot string
		}
		b.eval(fmt.Sprintf(`(() => {
			const li = document.querySelector('.tree [data-node="scope:c1"] .on-rail');
			return {rail: getComputedStyle(li, '::before').borderLeftColor, tree: getComputedStyle(document.querySelector('.tree')).backgroundColor,
				accent: getComputedStyle(document.querySelector('.tree .on-rail.selected'), '::after').borderBottomColor,
				dot: getComputedStyle(document.querySelector('.tree .row[data-thread="%d"] .line1 .dot')).backgroundColor};
		})()`, rig.running), &got)
		if got.Rail == got.Tree || got.Accent == got.Rail || got.Dot == got.Tree || got.Rail == "" {
			t.Fatalf("%s theme: %+v", theme, got)
		}
	}
}
