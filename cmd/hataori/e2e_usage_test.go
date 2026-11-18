package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// With the fake agent reporting usage in two turns, the thread's tokens are
// the last turn's total and the sum of both, its context the last reported,
// and the figures are the same after the daemon restarts.
func TestTokensFromTheFakeAgentSurviveARestart(t *testing.T) {
	root := newRoot(t)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runCtx(ctx, []string{"run", "--root", root}, io.Discard, io.Discard) }()
	t.Cleanup(func() { stop(); killSessions(root) })
	var c *wire.Client
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var err error
		if c, err = wire.Dial(root); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not come up")
		}
	}
	bg := context.Background()
	id, err := c.Start(bg, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "usage 10 5 100 0 1000 200000\nusage 20 10 100 20 1500 200000\nreply one"}})
	if err != nil {
		t.Fatal(err)
	}
	// info waits for the thread to be idle after turn n.
	info := func(c control.API, n int) control.ThreadInfo {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			ts, err := c.Threads(bg, control.Filter{})
			if err == nil && len(ts) == 1 && ts[0].State == control.Idle && ts[0].Turns == n {
				return ts[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("thread never idle after turn %d: %+v, %v", n, ts, err)
			}
		}
	}
	info(c, 1)
	if err := c.Send(bg, id, "k2", control.Input{Text: "usage 40 2 0 0 1600 200000\nreply two"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	got := info(c, 2)
	want := control.TokenCount{Turn: 42, Today: 150 + 42}
	if got.Tokens == nil || *got.Tokens != want || got.Context == nil || *got.Context != (control.Context{Used: 1600, Size: 200000}) {
		t.Fatalf("tokens %+v, context %+v; want %+v", got.Tokens, got.Context, want)
	}
	c.Close()

	stop()
	<-done
	startDaemon(t, root)
	c2, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	after := info(c2, 2)
	if after.Tokens == nil || *after.Tokens != want || after.Context == nil || *after.Context != *got.Context {
		t.Fatalf("after the restart: tokens %+v, context %+v", after.Tokens, after.Context)
	}
}
