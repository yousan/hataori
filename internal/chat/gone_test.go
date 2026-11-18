package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// goneDriver is the fake driver with one conversation that no longer
// exists on the service, and a count of Since calls per conversation.
type goneDriver struct {
	*fake.Driver
	mu    sync.Mutex
	gone  surface.Conversation
	since map[surface.Conversation]int
}

func (g *goneDriver) setGone(c surface.Conversation) {
	g.mu.Lock()
	g.gone = c
	g.mu.Unlock()
}

func (g *goneDriver) asked(c surface.Conversation) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.since[c]
}

func (g *goneDriver) Since(ctx context.Context, conv surface.Conversation, cursor string) ([]surface.Inbound, error) {
	g.mu.Lock()
	g.since[conv]++
	gone := conv == g.gone
	g.mu.Unlock()
	if gone {
		return nil, fmt.Errorf("fake surface: %w", surface.ErrGone)
	}
	return g.Driver.Since(ctx, conv, cursor)
}

// A thread deleted on the service is asked for once, logged once, and not
// asked for again, across retries and reconnects; the channel is still
// caught up, and the thread's session is left as it is.
func TestGoneConversationIsAskedForOnce(t *testing.T) {
	var g *goneDriver
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		g = &goneDriver{Driver: fe.Driver.(*fake.Driver), since: map[surface.Conversation]int{}}
		fe.Driver = g
	})
	id := f.runningThread(t)
	g.setGone(conv(id))
	api := f.fe.API(control.Caller{Principal: owner, Scopes: []string{channel}, May: control.OpRead})
	before, err := api.Threads(context.Background(), control.Filter{})
	if err != nil || len(before) != 1 {
		t.Fatalf("threads %+v %v", before, err)
	}

	f.connect(t)
	waitFor(t, "the first catch-up", func() bool { return g.asked(conv(id)) == 1 && g.asked(chanConv) >= 1 })
	// A reconnect catches up again, and the channel is still read.
	f.driver.Disconnect()
	f.driver.Inject(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git while down")
	f.driver.Reconnect()
	waitFor(t, "the trigger while down", func() bool { return len(f.runner.all()) == 2 })
	// Retries: something in each conversation fails again.
	for i := 0; i < 5; i++ {
		f.fe.inboundState().setBehind(conv(id), true)
		f.fe.inboundState().setBehind(chanConv, true)
		time.Sleep(20 * time.Millisecond) // four retry periods
	}
	waitFor(t, "the channel caught up", func() bool { return !f.fe.inboundState().isBehind(chanConv) })

	if n := g.asked(conv(id)); n != 1 {
		t.Errorf("the gone thread was asked for %d times, want 1", n)
	}
	var logged []string
	for _, l := range f.log.lines() {
		if strings.Contains(l, "thread="+id.Key) && strings.Contains(l, "catch-up") {
			logged = append(logged, l)
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "gone") {
		t.Errorf("logged for the gone thread: %q, want one line saying it is gone", logged)
	}
	after, err := api.Threads(context.Background(), control.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range after {
		if a.ID == id && a.State != before[0].State {
			t.Errorf("the gone thread is %s, was %s", a.State, before[0].State)
		}
	}
	if got := f.runner.handed(); len(got) != 1 || got[0] != "deliver first input" {
		t.Errorf("handed to the agent: %v", got)
	}
}
