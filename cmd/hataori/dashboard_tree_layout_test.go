package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// treeItem is one item of the dashboard's tree, in the order the page
// shows them: its key, whether it is dimmed, and where its link goes.
type treeItem struct {
	Item string
	Dim  bool
	Href string
}

// treeItems are the tree's items, in order.
func (b *browser) treeItems() []treeItem {
	b.t.Helper()
	var items []treeItem
	b.eval(`[...document.querySelectorAll('.tree [role=tree] [data-tree-item]')].map(i => ({
		item: i.dataset.treeItem,
		dim: i.classList.contains('chat-only'),
		href: i.querySelector(':scope > a.chat-link, :scope > .node-head a.chat-link')?.getAttribute('href') ?? '',
	}))`, &items)
	return items
}

// untilTree waits until the tree's items are want.
func (b *browser) untilTree(what string, want []treeItem) {
	b.t.Helper()
	var got []treeItem
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got = b.treeItems(); slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: the tree is\n%+v\nwant\n%+v", what, got, want)
		}
	}
}

// layoutRig is a daemon whose fake surface reports a layout: a group with
// no name holding an unserved channel u1 with a plain open thread l1, and
// the group "Work" holding the served channel c1 with a hataori thread
// (open) and a plain open thread p1, then the unserved channel u2. A second hataori thread of c1 has its
// chat thread archived: it is not in the layout. Both hataori threads are
// asleep.
type layoutRig struct {
	c              *wire.Client
	open, archived control.ThreadID
}

func newLayoutRig(t *testing.T, change func(cfg map[string]any)) (*chatRoot, layoutRig) {
	t.Helper()
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "2s"
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": fakeSurface, "enabled": true}}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		if change != nil {
			change(cfg)
		}
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
	start := func(key, text string) (control.ThreadID, string) {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: key, Scope: "c1", Repo: bareRepo(t), Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		var chat string
		eventually(t, "the thread "+key+" asleep", 30*time.Second, func() bool {
			ts, err := c.Threads(ctx, control.Filter{Thread: id})
			if err != nil || len(ts) != 1 || len(ts[0].Links) == 0 {
				return false
			}
			chat = ts[0].Links[0].Thread
			return ts[0].State == control.Asleep
		})
		return id, chat
	}
	archived, _ := start("k1", "reply archived")
	open, chat := start("k2", "reply open")
	ld.SetLayout(surface.Layout{Groups: []surface.LayoutGroup{
		{Channels: []surface.LayoutChannel{{ID: "u1", Name: "#lobby", URL: "https://chat.example/u1",
			Threads: []surface.LayoutThread{{ID: "l1", Name: "lunch", URL: "https://chat.example/l1"}}}}},
		{ID: "g1", Name: "Work", Channels: []surface.LayoutChannel{
			{ID: "c1", Name: "#builds", URL: "https://chat.example/c1", Threads: []surface.LayoutThread{
				{ID: chat, Name: "reply open", URL: "https://chat.example/" + chat},
				{ID: "p1", Name: "a question", URL: "https://chat.example/p1"},
			}},
			{ID: "u2", Name: "#deploys", URL: "https://chat.example/u2"},
		}},
	}})
	eventually(t, "the layout", 10*time.Second, func() bool {
		ls, err := c.Layout(ctx)
		return err == nil && len(ls) == 1 && len(ls[0].Groups) > 0
	})
	return r, layoutRig{c: c, open: open, archived: archived}
}

// The tree mirrors the chat, driven by the real server with the fake
// surface reporting a layout: group, then channel, then thread, in the
// chat's order. The served channel is today's scope node with today's row
// for its hataori thread open in the chat, asleep or not; the unserved
// channel and the plain threads are dimmed links into the chat, which open
// no tab. The hataori thread whose chat thread is archived is not in the
// tree, and "All" shows it under its channel. The host terminals stay one
// folded node at the bottom. A surface without the call keeps today's tree
// (TestDashboardTreeChannels).
func TestDashboardTreeMirrorsTheChat(t *testing.T) {
	hrig := newHostTerminalRig(t)
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = hrig.socket }
	t.Cleanup(func() { hostTermsHook = nil })
	_, rig := newLayoutRig(t, func(cfg map[string]any) { cfg["host_terminals"] = true })
	b := newBrowser(t)
	link, err := rig.c.SignInLink(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	client := fakeSurface + "/"
	want := []treeItem{
		{Item: "chan:" + client + "u1", Dim: true, Href: "https://chat.example/u1"},
		{Item: "chat:" + client + "l1", Dim: true, Href: "https://chat.example/l1"},
		{Item: "group:" + client + "g1"},
		{Item: "scope:c1"},
		{Item: fmt.Sprintf("thread:%d", rig.open)},
		{Item: "chat:" + client + "p1", Dim: true, Href: "https://chat.example/p1"},
		{Item: "chan:" + client + "u2", Dim: true, Href: "https://chat.example/u2"},
		{Item: "host"},
	}
	b.untilTree("the tree as the chat shows it", want)
	var label string
	b.eval(`document.querySelector('.tree [data-node="scope:c1"] .node-head .scope-name').textContent`, &label)
	if label != "#builds" {
		t.Fatalf("the served channel is named %q, want #builds", label)
	}
	var hostOpen string
	b.eval(`document.querySelector('.tree [data-node=host]').dataset.open`, &hostOpen)
	if hostOpen != "no" {
		t.Fatal("the host terminals' node is not folded at first")
	}
	// A dimmed item is a link into the chat, in a new window: it opens no
	// tab.
	var target, rel string
	b.eval(`document.querySelector('.tree [data-tree-item="chat:`+client+`p1"] a.chat-link').target`, &target)
	b.eval(`document.querySelector('.tree [data-tree-item="chat:`+client+`p1"] a.chat-link').rel`, &rel)
	if target != "_blank" || rel != "noopener noreferrer" {
		t.Fatalf("a chat link opens with target %q, rel %q", target, rel)
	}
	tabs := func() int {
		var n int
		b.eval(`document.querySelectorAll('.tabbar .tab').length`, &n)
		return n
	}
	before := tabs()
	// The click is stopped before it navigates, so that the test's page
	// stays; what a click does besides is the page's.
	b.eval(`document.addEventListener('click', e => { if (e.target.closest('a.chat-link')) e.preventDefault(); }), true`, nil)
	b.click(`.tree [data-tree-item="chat:` + client + `p1"] a.chat-link`)
	b.click(`.tree [data-tree-item="chan:` + client + `u1"] a.chat-link`)
	time.Sleep(300 * time.Millisecond)
	if n := tabs(); n != before {
		t.Fatalf("a chat link opened a tab: %d tabs, were %d", n, before)
	}

	// "All": the hataori thread whose chat thread is archived, under its
	// channel, after the threads open in the chat.
	b.showAll()
	all := slices.Clone(want)
	all = slices.Insert(all, 6, treeItem{Item: fmt.Sprintf("thread:%d", rig.archived)})
	b.untilTree("All", all)
}

// A person other than the owner sees only the channels of the scopes they
// may read, and the groups that hold them: not the unserved channel.
func TestDashboardTreeMirrorsTheChatForAPerson(t *testing.T) {
	dashboardHook = func(c *control.Caller) { c.Principal = "grace" }
	t.Cleanup(func() { dashboardHook = nil })
	_, rig := newLayoutRig(t, nil)
	b := newBrowser(t)
	link, err := rig.c.SignInLink(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	client := fakeSurface + "/"
	b.untilTree("a person's tree", []treeItem{
		{Item: "group:" + client + "g1"},
		{Item: "scope:c1"},
		{Item: fmt.Sprintf("thread:%d", rig.open)},
		{Item: "chat:" + client + "p1", Dim: true, Href: "https://chat.example/p1"},
	})
}
