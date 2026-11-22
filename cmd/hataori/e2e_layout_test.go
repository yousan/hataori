package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// A chat's layout reaches the control API and the dashboard's route, with
// the real daemon and the fake surface reporting one: two groups, a served
// and an unserved channel, a hataori thread and a plain open thread. The
// owner sees every channel, with the served one naming its scope and the
// hataori thread its number; a person other than the owner sees only the
// channels of the scopes they may read. A change (here a thread archived,
// so no longer in the layout) reaches it again, after a layout event.
func TestLayoutReachesTheControlAPI(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true},
			{"key": "c2", "client": fakeSurface, "enabled": true},
		}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	ld := r.driver.WithLayout()
	drivers.Store(r.root, ld)
	// The dashboard's person is not the owner, and may read c1 only.
	dashboardHook = func(c *control.Caller) { c.Principal, c.Scopes = "grace", []string{"c1"} }
	t.Cleanup(func() { dashboardHook = nil })
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "c1", Repo: bareRepo(t), Input: control.Input{Text: "reply x"}})
	if err != nil {
		t.Fatal(err)
	}
	var chatThread string
	eventually(t, "the thread's link", 20*time.Second, func() bool {
		ts, err := c.Threads(ctx, control.Filter{Thread: id})
		if err != nil || len(ts) != 1 || len(ts[0].Links) == 0 {
			return false
		}
		chatThread = ts[0].Links[0].Thread
		return chatThread != ""
	})
	ev, err := c.Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for e := range ev {
		if e.Kind == control.CaughtUp {
			break
		}
	}
	ld.SetLayout(surface.Layout{Groups: []surface.LayoutGroup{
		{Channels: []surface.LayoutChannel{{ID: "u1", Name: "#lobby", URL: "https://chat.example/u1"}}},
		{ID: "g1", Name: "Work", Channels: []surface.LayoutChannel{
			{ID: "c1", Name: "#builds", URL: "https://chat.example/c1", Threads: []surface.LayoutThread{
				{ID: chatThread, Name: "reply x", URL: "https://chat.example/" + chatThread},
				{ID: "p1", Name: "lunch", URL: "https://chat.example/p1"},
			}},
			{ID: "c2", Name: "#deploys", URL: "https://chat.example/c2"},
		}},
	}})
	builds := control.LayoutChannel{ID: "c1", Name: "#builds", URL: "https://chat.example/c1", Scope: "c1", Threads: []control.LayoutThread{
		{ID: chatThread, Name: "reply x", URL: "https://chat.example/" + chatThread, Thread: id},
		{ID: "p1", Name: "lunch", URL: "https://chat.example/p1"},
	}}
	deploys := control.LayoutChannel{ID: "c2", Name: "#deploys", URL: "https://chat.example/c2", Scope: "c2", Threads: []control.LayoutThread{}}
	lobby := control.LayoutChannel{ID: "u1", Name: "#lobby", URL: "https://chat.example/u1", Threads: []control.LayoutThread{}}
	owner := func(work ...control.LayoutChannel) []control.ChatLayout {
		return []control.ChatLayout{{Client: fakeSurface, Groups: []control.LayoutGroup{
			{Channels: []control.LayoutChannel{lobby}}, {ID: "g1", Name: "Work", Channels: work},
		}}}
	}
	untilLayout := func(what string, want []control.ChatLayout) {
		t.Helper()
		var got []control.ChatLayout
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err = c.Layout(ctx)
			if err == nil && reflect.DeepEqual(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the owner gets %+v (%v), want %+v", what, got, err, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	untilLayout("the first layout", owner(builds, deploys))
	layoutEvent := func(what string) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case e := <-ev:
				if e.Kind != "layout" {
					continue // the thread's own events
				}
				if e.Thread != 0 || e.Scope != "" || string(e.Data) != `{"client":"`+fakeSurface+`"}` {
					t.Fatalf("%s: the layout event is %+v, want one of %s and no thread", what, e, fakeSurface)
				}
				return
			case <-timeout:
				t.Fatalf("%s: no layout event", what)
			}
		}
	}
	layoutEvent("the first layout")

	base, session := dashSignIn(t, r.root)
	grace := func() []control.ChatLayout {
		t.Helper()
		code, body := dashGet(t, base, session, "/api/layout")
		var ls []control.ChatLayout
		if err := json.Unmarshal([]byte(body), &ls); code != 200 || err != nil {
			t.Fatalf("/api/layout: %d %s", code, body)
		}
		return ls
	}
	want := []control.ChatLayout{{Client: fakeSurface, Groups: []control.LayoutGroup{{ID: "g1", Name: "Work", Channels: []control.LayoutChannel{builds}}}}}
	if got := grace(); !reflect.DeepEqual(got, want) {
		t.Fatalf("a person of c1 gets\n%+v\nwant\n%+v", got, want)
	}

	// The plain thread is archived: the service's layout no longer has it.
	ld.SetLayout(surface.Layout{Groups: []surface.LayoutGroup{
		{Channels: []surface.LayoutChannel{{ID: "u1", Name: "#lobby", URL: "https://chat.example/u1"}}},
		{ID: "g1", Name: "Work", Channels: []surface.LayoutChannel{
			{ID: "c1", Name: "#builds", URL: "https://chat.example/c1", Threads: []surface.LayoutThread{
				{ID: chatThread, Name: "reply x", URL: "https://chat.example/" + chatThread},
			}},
			{ID: "c2", Name: "#deploys", URL: "https://chat.example/c2"},
		}},
	}})
	layoutEvent("a thread archived")
	builds.Threads = builds.Threads[:1]
	untilLayout("a thread archived", owner(builds, deploys))
	want[0].Groups[0].Channels = []control.LayoutChannel{builds}
	if got := grace(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the archive a person of c1 gets\n%+v\nwant\n%+v", got, want)
	}
}

// A surface without the call (as Slack has no channel list hataori reads)
// records no layout: Layout and the dashboard's route answer none, and the
// page keeps today's tree.
func TestNoLayoutWithoutTheCall(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(time.Second) // longer than the front end waits before it records a layout
	ls, err := c.Layout(context.Background())
	if err != nil || len(ls) != 0 || ls == nil {
		t.Fatalf("Layout: %#v, %v; want an empty list", ls, err)
	}
	base, session := dashSignIn(t, r.root)
	if code, body := dashGet(t, base, session, "/api/layout"); code != 200 || body != "[]\n" && body != "[]" {
		t.Fatalf("/api/layout: %d %q, want []", code, body)
	}
}
