package main

import (
	"context"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// A thread with two links: input that the owner gives through the
// dashboard's way (a Caller with Via "dashboard", as the daemon binds a
// dashboard session) reaches the agent once and appears once in each link
// as "<owner>: <text>", and its answer in both; input from the control
// socket, which came through no link and names no way, appears in none
// (#605).
func TestDashboardInputShowsInEveryLink(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch"}, "show_in": []string{fakeSurface + ":c3"}},
		}
	})
	var c *core.Core
	coreHook = func(got *core.Core) { c = got }
	t.Cleanup(func() { coreHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	one := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, one, "hello")
	n := numberOf(t, r.root, one)
	c3 := surface.Conversation{Channel: "c3"}
	r.driver.InjectCommand(c3, fakesurface.Webhook(e2eHook), "attach", surface.Arg{Name: "number", Value: n})
	var here surface.Conversation
	eventually(t, "the link in c3", 15*time.Second, func() bool {
		o := opened(r.driver, c3)
		if len(o) == 1 {
			here = o[0]
		}
		return len(o) == 1 && len(linksOf(t, r.root, n)) == 2
	})
	ctx := context.Background()
	ts, err := c.As(control.Caller{Principal: "ada", Scopes: []string{"c1"}, May: control.OpRead}).Threads(ctx, control.Filter{})
	if err != nil || len(ts) != 1 {
		t.Fatalf("threads: %v %v", ts, err)
	}
	id := ts[0].ID

	dash := c.As(control.Caller{Principal: "ada", Scopes: []string{"c1"}, May: control.OpRead | control.OpSend, Via: "dashboard"})
	if err := dash.Send(ctx, id, "k-dash", control.Input{Text: "reply from the page"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer in both links", 15*time.Second, func() bool {
		return len(postsLike(r.driver, one, "from the page")) == 1 && len(postsLike(r.driver, here, "from the page")) == 1
	})
	for _, conv := range []surface.Conversation{one, here} {
		if got := postsLike(r.driver, conv, "ada: reply from the page"); len(got) != 1 {
			t.Fatalf("%v shows the dashboard's input %d times, want once", conv, len(got))
		}
	}

	// The control socket's input: answered in both, shown as input in none.
	wc, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	if err := wc.Send(ctx, id, "k-socket", control.Input{Text: "reply from the socket"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the socket's answer in both links", 15*time.Second, func() bool {
		return len(postsLike(r.driver, one, "from the socket")) == 1 && len(postsLike(r.driver, here, "from the socket")) == 1
	})
	time.Sleep(300 * time.Millisecond)
	for _, conv := range []surface.Conversation{one, here} {
		if got := postsLike(r.driver, conv, "ada: reply from the socket"); len(got) != 0 {
			t.Fatalf("%v shows the socket's input %d times, want none", conv, len(got))
		}
	}
}
