package chat

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// heldEvents is an API whose Events subscriptions, once their context
// ends, stop only when release is closed: as the store's Follow may still
// be reading when its caller cancels.
type heldEvents struct {
	control.API
	send      []control.Event
	release   chan struct{}
	stopped   atomic.Bool
	onEvents  func()        // called when a subscription starts, if set
	cancelled chan struct{} // closed once a subscription saw its context end
	once      sync.Once
}

func (h *heldEvents) Events(ctx context.Context, f control.Filter, after uint64) (<-chan control.Event, error) {
	out := make(chan control.Event)
	if h.onEvents != nil {
		h.onEvents()
	}
	go func() {
		defer close(out)
		for _, e := range h.send {
			select {
			case out <- e:
			case <-ctx.Done():
			}
		}
		<-ctx.Done()
		h.once.Do(func() { close(h.cancelled) })
		<-h.release
		h.stopped.Store(true)
	}()
	return out, nil
}

// returnsAfterItsSubscription runs fn, which must not return before the
// subscription it started has stopped. Once the subscription has seen its
// context end, fn is on its way out: fn returning while the subscription is
// held fails the test; otherwise the subscription is let go after hold.
func returnsAfterItsSubscription(t *testing.T, h *heldEvents, fn func()) {
	t.Helper()
	h.cancelled = make(chan struct{})
	done := make(chan bool, 1)
	go func() { fn(); done <- h.stopped.Load() }()
	select {
	case <-done:
		t.Fatal("returned before its subscription saw its context end")
	case <-h.cancelled:
	}
	const hold = 200 * time.Millisecond
	select {
	case <-done:
		t.Fatal("returned while its subscription to the events was still running")
	case <-time.After(hold):
	}
	close(h.release)
	if stopped := <-done; !stopped {
		t.Fatal("returned before its subscription stopped")
	}
}

func TestStartOutcomeWaitsForItsSubscription(t *testing.T) {
	id := control.ThreadID{Scope: channel, Key: "t"}
	h := &heldEvents{release: make(chan struct{}), send: []control.Event{
		{Pos: 1, Thread: id, Kind: "thread_created"}, {Pos: 1, Kind: control.CaughtUp}}}
	returnsAfterItsSubscription(t, h, func() { startOutcome(context.Background(), h, id) })
}

func TestScanWaitsForItsSubscription(t *testing.T) {
	h := &heldEvents{release: make(chan struct{}), send: []control.Event{{Pos: 0, Kind: control.CaughtUp}}}
	r := &reader{api: h}
	returnsAfterItsSubscription(t, h, func() {
		r.scan(context.Background(), control.ThreadID{Scope: channel, Key: "t"}, 10, func(control.Event) {})
	})
}

func TestShowWaitsForItsSubscription(t *testing.T) {
	f := newFront(t, workspaces{})
	h := &heldEvents{release: make(chan struct{})}
	api := f.fe.API
	f.fe.API = func(c control.Caller) control.API { h.API = api(c); return h }
	ctx, cancel := context.WithCancel(context.Background())
	h.onEvents = cancel // show stops as soon as it has subscribed
	returnsAfterItsSubscription(t, h, func() { f.fe.show(ctx) })
}

// heldSince is the fake driver with a Since that waits until release is
// closed, as reading a channel's history can take a while.
type heldSince struct {
	*fake.Driver
	entered  chan struct{}
	once     sync.Once
	release  chan struct{}
	returned atomic.Bool
}

func (d *heldSince) Since(ctx context.Context, conv surface.Conversation, cursor string) ([]surface.Inbound, error) {
	d.once.Do(func() { close(d.entered) })
	<-d.release
	defer d.returned.Store(true)
	return d.Driver.Since(ctx, conv, cursor)
}

// Run returns only once the catch-up that the driver's Connected started
// has ended.
func TestRunWaitsForTheCatchUp(t *testing.T) {
	f := newFront(t, workspaces{})
	d := &heldSince{Driver: f.driver, entered: make(chan struct{}), release: make(chan struct{})}
	f.fe.Driver = d
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { f.fe.Run(ctx); done <- d.returned.Load() }()
	<-d.entered
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while its catch-up was still reading")
	case <-time.After(200 * time.Millisecond): // the catch-up is held this long
	}
	close(d.release)
	if caughtUp := <-done; !caughtUp {
		t.Fatal("Run returned before its catch-up did")
	}
}
