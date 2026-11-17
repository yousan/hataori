package chat

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// connect runs the fake driver for the front end, with catch-up, until
// the test ends: the daemon is up.
func (f *front) connect(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	up := make(chan struct{}, 1)
	go func() { defer wg.Done(); f.driver.Run(ctx, signalling{f.fe, up}) }()
	go func() { defer wg.Done(); f.fe.runCatchUp(ctx) }()
	<-up
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); wg.Wait() }) }
	t.Cleanup(stop)
	return stop
}

// signalling is the front end as the driver's sink, telling the test when
// the driver has connected.
type signalling struct {
	*FrontEnd
	up chan struct{}
}

func (s signalling) Connected() {
	s.FrontEnd.Connected()
	select {
	case s.up <- struct{}{}:
	default:
	}
}

func (f *front) storedCursor(conv surface.Conversation) string {
	c, _, _ := f.fe.cursor(context.Background(), conv)
	return c
}

func TestThreeMessagesWhileDownRunOnceEachInOrder(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	for _, s := range []string{"one", "two", "three"} {
		f.driver.Inject(conv(id), fake.Webhook(hook), s) // nobody is connected
	}
	f.connect(t)
	want := "deliver first input; deliver one; deliver two; deliver three"
	waitFor(t, "the three", func() bool { return strings.Join(f.runner.handed(), "; ") == want })
}

func TestTriggerWhileDownStartsItsThreadOnce(t *testing.T) {
	f := newFront(t, workspaces{})
	f.connect(t)
	f.driver.Inject(chanConv, fake.Person(owner), "good morning") // read live: the channel's cursor
	f.driver.Disconnect()
	trigger := f.driver.Inject(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git while down")
	f.driver.Reconnect()
	waitFor(t, "the thread", func() bool { return len(f.runner.all()) == 1 })
	f.fe.Message(trigger) // and live as well
	f.fe.catchUp(context.Background(), chanConv)
	if n, l := len(f.threads()), f.runner.all(); n != 1 || len(l) != 1 || l[0].Input.Text != "while down" {
		t.Fatalf("%d threads, started %+v", n, l)
	}
}

func TestMessageLiveAndCaughtUpIsOneInput(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	f.connect(t)
	f.driver.Disconnect()
	in := f.driver.Inject(conv(id), fake.Webhook(hook), "once")
	f.driver.Reconnect()
	f.fe.catchUp(context.Background(), conv(id))
	f.fe.Message(in) // it comes live as well, late
	if got := strings.Join(f.runner.handed(), "; "); got != "deliver first input; deliver once" {
		t.Fatalf("handed %s", got)
	}
}

func TestCursorStaysWhenAFailureComesBetween(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	var ins []surface.Inbound
	for _, s := range []string{"one", "two", "three"} {
		ins = append(ins, f.driver.Inject(conv(id), fake.Webhook(hook), s))
	}
	db := f.fe.Outbox.Store.DB
	// The second message is accepted, then storing its cursor fails.
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := db.Exec(`CREATE TRIGGER fail_` + op + ` BEFORE ` + op + ` ON chat_inbound WHEN NEW.cursor = '` +
			ins[1].Cursor + `' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
			t.Fatal(err)
		}
	}
	f.fe.catchUp(context.Background(), conv(id))
	if c := f.storedCursor(conv(id)); c != ins[0].Cursor {
		t.Fatalf("cursor %q after the failure, want %q (the first message's)", c, ins[0].Cursor)
	}
	if got := strings.Join(f.runner.handed(), "; "); got != "deliver first input; deliver one; deliver two" {
		t.Fatalf("handed %s", got)
	}
	db.Exec(`DROP TRIGGER fail_INSERT`)
	db.Exec(`DROP TRIGGER fail_UPDATE`)
	f.connect(t) // catches up again
	waitFor(t, "the cursor at the end", func() bool { return f.storedCursor(conv(id)) == ins[2].Cursor })
	if got := strings.Join(f.runner.handed(), "; "); got != "deliver first input; deliver one; deliver two; deliver three" {
		t.Fatalf("handed %s", got)
	}
}
