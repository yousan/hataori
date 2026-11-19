package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// unfoldHosts unfolds the tree's host terminals node, folded at first,
// as a person would.
func (b *browser) unfoldHosts() {
	b.t.Helper()
	b.until("the host terminals' node", `!!document.querySelector('.tree [data-node=host] .node-head')`)
	var open bool
	b.eval(`document.querySelector('.tree [data-node=host]').dataset.open === 'yes'`, &open)
	if !open {
		b.click(`.tree [data-node=host] .node-head`)
	}
	b.until("the host terminals unfolded", `document.querySelector('.tree [data-node=host]').dataset.open === 'yes'`)
}

// The tree reads like the chat it mirrors, driven by the real server with
// the fake surface and the fake agent: one node per scope, named as the
// surface names its channel (here "#builds"), by the key when it names
// none, and by the config's name when there is one, over the surface's;
// under each its threads, the newest first, with their count and one dot
// each; the host terminals last, folded at first; a node's fold kept
// across a reload; and the tree driven from the keyboard.
func TestDashboardTreeChannels(t *testing.T) {
	rig := newHostTerminalRig(t)
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = rig.socket }
	t.Cleanup(func() { hostTermsHook = nil })
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true},
			{"key": "c2", "client": fakeSurface, "enabled": true},
			{"key": "c3", "client": fakeSurface, "enabled": true, "name": "Release"},
			{"key": "c4", "client": fakeSurface, "enabled": true}, // no thread
		}
		cfg["host_terminals"] = true
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	r.driver.SetName(surface.Conversation{Channel: "c1"}, "#builds")
	r.driver.SetName(surface.Conversation{Channel: "c3"}, "#rel") // the config's name wins
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	repo := bareRepo(t)
	start := func(scope, key, text string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: key, Scope: scope, Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the thread "+key, 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
		return id
	}
	older := start("c1", "k1", "reply older")
	newer := start("c1", "k2", "reply newer")
	plain := start("c2", "k3", "reply plain")
	named := start("c3", "k4", "reply named")
	// The names reach the control API.
	eventually(t, "the scopes' names", 10*time.Second, func() bool {
		ss, err := c.Scopes(ctx)
		return err == nil && slices.Equal(ss, []control.ScopeInfo{{Key: "c1", Name: "#builds"}, {Key: "c2"}, {Key: "c3", Name: "Release"}, {Key: "c4"}})
	})
	b := newBrowser(t)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	type node struct {
		Node, Label, Open string
		Count             int
		Dots              int
		Rows              []int64
	}
	nodes := func() []node {
		var ns []node
		b.eval(`[...document.querySelectorAll('.tree [role=tree] .node')].map(n => ({
			node: n.dataset.node, label: n.querySelector('.node-head .scope-name').textContent, open: n.dataset.open,
			count: +n.querySelector('.node-count').dataset.count, dots: n.querySelectorAll('.node-head .dot').length,
			rows: [...n.querySelectorAll('.row[data-thread]')].map(r => +r.dataset.thread),
		}))`, &ns)
		return ns
	}
	want := []node{
		{Node: "scope:c1", Label: "#builds", Open: "yes", Count: 2, Dots: 2, Rows: []int64{int64(newer), int64(older)}},
		{Node: "scope:c2", Label: "c2", Open: "yes", Count: 1, Dots: 1, Rows: []int64{int64(plain)}},
		{Node: "scope:c4", Label: "c4", Open: "yes", Count: 0, Dots: 0, Rows: nil}, // a scope with no thread
		{Node: "scope:c3", Label: "Release", Open: "yes", Count: 1, Dots: 1, Rows: []int64{int64(named)}},
		{Node: "host", Label: "Host terminals", Open: "no", Count: -1},
	}
	matchesWant := func(got []node, want []node) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			w, g := want[i], got[i]
			if w.Count < 0 { // the host terminals: any count, no rows
				w.Count, w.Rows = g.Count, nil
			}
			if g.Node != w.Node || g.Label != w.Label || g.Open != w.Open || g.Count != w.Count || g.Dots != w.Dots || !slices.Equal(g.Rows, w.Rows) {
				return false
			}
		}
		return true
	}
	untilNodes := func(what string, want []node) {
		t.Helper()
		var got []node
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if got = nodes(); matchesWant(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the tree is\n%+v\nwant\n%+v", what, got, want)
			}
		}
	}
	// 1. Scopes by name, the key without one, the config's name over the
	// surface's; threads the newest first; the host terminals last, folded.
	untilNodes("the tree", want)
	// The older thread has a newer turn: it is the newest now, without a
	// reload.
	if err := c.Send(ctx, older, "k1-again", control.Input{Text: "reply again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the older thread's turn", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, older, 0, 0)
		n, _ := c.Thread(ctx, newer, 0, 0)
		return err == nil && d.Turns == 2 && d.State == control.Idle && d.Updated.After(n.Updated)
	})
	want[0].Rows = []int64{int64(older), int64(newer)}
	untilNodes("the thread with the newer turn first", want)
	var hostRows int
	b.eval(`document.querySelectorAll('.tree [data-node=host] [data-host]').length`, &hostRows)
	if hostRows != 0 {
		t.Fatal("the folded host terminals show their rows")
	}

	// 2. A fold is kept across a reload; the host terminals stay folded.
	reload := func() {
		t.Helper()
		b.eval(`document.body.dataset.reloading = 'yes', true`, nil)
		b.call("Page.reload", map[string]any{})
		b.until("the page after the reload", `document.body.dataset.reloading !== 'yes' && document.querySelectorAll('.tree .node').length === 5`)
	}
	b.click(`.tree [data-node="scope:c1"] .node-head`)
	folded := slices.Clone(want)
	folded[0].Open, folded[0].Rows = "no", nil
	untilNodes("c1 folded", folded)
	reload()
	untilNodes("c1 folded after a reload", folded)

	// 3. The keyboard: right unfolds, right again goes to the first thread,
	// down and up move, left goes to the node, left again folds, down goes
	// to the next node, Enter opens a thread's tab.
	focused := func() string {
		var s string
		b.eval(`document.activeElement?.dataset.treeItem ?? ''`, &s)
		return s
	}
	key := func(k string, code int) {
		t.Helper()
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": k, "code": k, "windowsVirtualKeyCode": code})
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k, "code": k, "windowsVirtualKeyCode": code})
	}
	expectFocus := func(what, want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); focused() != want; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the focus is on %q, want %q", what, focused(), want)
			}
		}
	}
	b.eval(`document.querySelector('.tree [data-node="scope:c1"]').focus(), true`, nil)
	expectFocus("the first node", "scope:c1")
	key("ArrowRight", 39)
	untilNodes("c1 unfolded from the keyboard", want)
	expectFocus("right unfolds and stays", "scope:c1")
	key("ArrowRight", 39)
	expectFocus("right goes to the first thread", fmt.Sprintf("thread:%d", older))
	key("ArrowDown", 40)
	expectFocus("down", fmt.Sprintf("thread:%d", newer))
	key("ArrowUp", 38)
	expectFocus("up", fmt.Sprintf("thread:%d", older))
	key("ArrowLeft", 37)
	expectFocus("left goes to the node", "scope:c1")
	key("ArrowLeft", 37)
	untilNodes("c1 folded from the keyboard", folded)
	key("ArrowDown", 40)
	expectFocus("down from a folded node", "scope:c2")
	key("ArrowDown", 40)
	expectFocus("down into c2", fmt.Sprintf("thread:%d", plain))
	key("Enter", 13)
	b.until("the thread's tab", fmt.Sprintf(`!!document.querySelector('.tabbar .tab[data-tab="thread:%d"] .tab-main[aria-selected=true]')`, plain))

	// 4. While filtering, a node with a match is open and shows only the
	// scopes with one; a fold then lasts until the filter is cleared, and
	// leaves the node's own fold as it was.
	b.click(`.tree [data-node="scope:c1"] .node-head`)
	untilNodes("c1 unfolded again", want)
	b.filter("older")
	b.until("only c1 while filtering", `JSON.stringify([...document.querySelectorAll('.tree [role=tree] [data-scope]')].map(n => n.dataset.scope + ':' + n.dataset.open)) === '["c1:yes"]'`)
	b.click(`.tree [data-node="scope:c1"] .node-head`)
	b.until("c1 folded while filtering", `document.querySelector('.tree [data-node="scope:c1"]').dataset.open === 'no'`)
	b.filter("")
	untilNodes("c1 open again once the filter is cleared", want)

	// 5. The host terminals: unfolded, tmux session, then window and pane,
	// as before; kept unfolded across a reload.
	b.unfoldHosts()
	b.until("the pane under its session", fmt.Sprintf(`!!document.querySelector('.tree [data-node=host] [data-session=work] [data-host=%q]')`, rig.device))
	// Each tree item is in the tree or in the group of the item it is under.
	var misplaced []string
	b.eval(`[...document.querySelectorAll('.tree [role=tree] [role=treeitem]')].filter(i => {
		let p = i.parentElement;
		while (p && p.getAttribute('role') === 'none') p = p.parentElement;
		if (!p) return true;
		if (p.getAttribute('role') === 'tree') return false;
		if (p.getAttribute('role') !== 'group') return true;
		const owner = p.parentElement;
		return owner?.getAttribute('role') !== 'treeitem' || owner.dataset.treeItem !== i.dataset.parent;
	}).map(i => i.dataset.treeItem)`, &misplaced)
	if len(misplaced) > 0 {
		t.Fatalf("tree items outside the tree or the group of their parent: %q", misplaced)
	}
	reload()
	b.until("the host terminals unfolded after a reload", `document.querySelector('.tree [data-node=host]').dataset.open === 'yes'`)
}

// A caller that may not read text gets no channel name from the chat
// service, which people write, and still gets the config's name.
func TestScopeNameWithheld(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true},
			{"key": "c3", "client": fakeSurface, "enabled": true, "name": "Release"},
		}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	r.driver.SetName(surface.Conversation{Channel: "c1"}, "#builds")
	dashboardHook = func(c *control.Caller) { c.NoText = true }
	t.Cleanup(func() { dashboardHook = nil })
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	for _, scope := range []string{"c1", "c3"} {
		id, err := c.Start(ctx, control.StartRequest{Key: "k-" + scope, Scope: scope, Repo: bareRepo(t), Input: control.Input{Text: "reply x"}})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the thread", 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
	}
	eventually(t, "the owner sees the channel's name", 10*time.Second, func() bool {
		ss, _ := c.Scopes(ctx)
		return slices.Equal(ss, []control.ScopeInfo{{Key: "c1", Name: "#builds"}, {Key: "c3", Name: "Release"}})
	})
	base, cookie := dashSignIn(t, r.root)
	code, body := dashGet(t, base, cookie, "/api/scopes")
	var ss []control.ScopeInfo
	if err := json.Unmarshal([]byte(body), &ss); code != 200 || err != nil {
		t.Fatalf("/api/scopes: %d %s", code, body)
	}
	if want := []control.ScopeInfo{{Key: "c1"}, {Key: "c3", Name: "Release"}}; !slices.Equal(ss, want) {
		t.Fatalf("a caller that may not read text gets %+v, want %+v", ss, want)
	}
}
