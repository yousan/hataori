package wire

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// hostFake is one tmux pane, %1, that records what it is sent.
type hostFake struct {
	mu      sync.Mutex
	keys    []control.HostKey
	dropped int
	hold    chan struct{} // Keys says so here, then waits until its context ends
}

func (h *hostFake) List(context.Context, []int) ([]control.HostTerminal, error) { return nil, nil }
func (h *hostFake) Screen(context.Context, string, []int) (string, bool, error) {
	return "", false, nil
}
func (h *hostFake) View(context.Context, string, []int) (control.HostView, bool, error) {
	return control.HostView{}, false, nil
}
func (h *hostFake) Check(_ context.Context, pane string, _ []int, _ bool) (*control.TmuxPane, bool, error) {
	if pane != "%1" {
		return nil, false, nil
	}
	return &control.TmuxPane{Pane: "%1", Session: "work", Window: "0", Index: "0"}, false, nil
}
func (h *hostFake) Keys(ctx context.Context, _, _ string, keys []control.HostKey) error {
	h.mu.Lock()
	h.keys = append(h.keys, keys...)
	hold := h.hold
	h.mu.Unlock()
	if hold != nil {
		hold <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (h *hostFake) NewBuffer() (string, error) { return "b", nil }
func (h *hostFake) DropBuffer(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropped++
	return nil
}

func (h *hostFake) sent() []control.HostKey {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]control.HostKey(nil), h.keys...)
}

// OperateHost over the socket: keys arrive in order, a refused batch is an
// error and the operation goes on, End ends it with its reason, a second
// operation is busy, and a client that goes away ends the operation.
func TestOperateHostOverTheSocket(t *testing.T) {
	root := t.TempDir()
	c, _ := newCore(t, root)
	h := &hostFake{}
	c.HostTerms, c.HostInput, c.HostInputIdle = h, h, 90*time.Second
	srv := serve(t, root, c.As(control.Caller{Principal: "owner", May: control.OpHost | control.OpHostInput}))
	cl, err := dialPath(srv.Path(), Version)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx := context.Background()

	op, err := cl.OperateHost(ctx, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if op.Idle() != 90*time.Second {
		t.Errorf("idle %v", op.Idle())
	}
	keys := []control.HostKey{{Key: "Up"}, {Text: "ls\x7f -l"}, {Text: "pasted", Paste: true}, {Key: "Enter"}}
	if err := op.Send(ctx, keys[:2]); err != nil {
		t.Fatal(err)
	}
	if err := op.Send(ctx, []control.HostKey{{Key: "Space"}}); err == nil || errors.Is(err, control.ErrEnded) {
		t.Errorf("a key off the list: %v", err)
	}
	if err := op.Send(ctx, keys[2:]); err != nil {
		t.Fatal(err)
	}
	if got := h.sent(); len(got) != len(keys) || got[1] != keys[1] || got[2] != keys[2] || got[3] != keys[3] {
		t.Errorf("sent %+v", got)
	}
	if _, err := cl.OperateHost(ctx, "%1"); !errors.Is(err, core.ErrBusy) || !strings.Contains(err.Error(), "owner") {
		t.Errorf("a second operation: %v", err)
	}
	op.End()
	select {
	case <-op.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("End did not end the operation")
	}
	if op.Reason() != control.HostEnded {
		t.Errorf("reason %q", op.Reason())
	}
	if err := op.Send(ctx, keys[:1]); !errors.Is(err, control.ErrEnded) {
		t.Errorf("Send after the end: %v", err)
	}

	// A client whose context ends closes the connection: the daemon's
	// operation ends, and the pane is free again.
	cctx, cancel := context.WithCancel(ctx)
	op, err = cl.OperateHost(cctx, "%1")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-op.Done()
	if op.Reason() != "disconnected" {
		t.Errorf("the client's reason %q", op.Reason())
	}
	var again control.HostOperation
	deadline := time.Now().Add(10 * time.Second)
	for again, err = cl.OperateHost(ctx, "%1"); errors.Is(err, core.ErrBusy) && time.Now().Before(deadline); again, err = cl.OperateHost(ctx, "%1") {
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the pane is not free after the client went: %v", err)
	}
	again.End()
	<-again.Done()

	if _, err := cl.OperateHost(ctx, "%2"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("a pane that is not listed: %v", err)
	}
	// HostKeys and End are no operations outside a stream.
	if err := cl.call(ctx, "HostKeys", hostKeysArgs{Keys: keys}, nil); !errors.Is(err, ErrNotSupported) {
		t.Errorf("HostKeys outside a stream: %v", err)
	}
	if n := len(h.sent()); n != len(keys) {
		t.Errorf("%d keys sent", n)
	}
}

// The owner without OpHostInput is refused over the socket too.
func TestOperateHostForbiddenOverTheSocket(t *testing.T) {
	root := t.TempDir()
	c, _ := newCore(t, root)
	h := &hostFake{}
	c.HostTerms, c.HostInput = h, h
	srv := serve(t, root, c.As(control.Caller{Principal: "owner", May: control.OpHost}))
	cl, err := dialPath(srv.Path(), Version)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.OperateHost(context.Background(), "%1"); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("%v", err)
	}
}

// A daemon older than 3.11 is not asked.
func TestOperateHostNeedsANewerDaemon(t *testing.T) {
	asked := make(chan string, 1)
	path := fakeDaemon(t, "3.9", func(m message) []string {
		asked <- m.Op
		return nil
	})
	cl, err := dialPath(path, "3.0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.OperateHost(context.Background(), "%1"); err == nil || !strings.Contains(err.Error(), "3.11") {
		t.Errorf("%v", err)
	}
	select {
	case op := <-asked:
		t.Errorf("the old daemon was sent %s", op)
	case <-time.After(100 * time.Millisecond):
	}
}

// An End, and a client that goes away, take effect while a batch is being
// sent: the daemon keeps reading the connection during a batch.
func TestOperateHostEndDuringABatch(t *testing.T) {
	root := t.TempDir()
	c, _ := newCore(t, root)
	h := &hostFake{hold: make(chan struct{})}
	c.HostTerms, c.HostInput = h, h
	srv := serve(t, root, c.As(control.Caller{Principal: "owner", May: control.OpHost | control.OpHostInput}))
	cl, err := dialPath(srv.Path(), Version)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx := context.Background()
	for _, how := range []string{"End", "the client goes"} {
		cctx, cancel := context.WithCancel(ctx)
		op, err := cl.OperateHost(cctx, "%1")
		if err != nil {
			t.Fatal(err)
		}
		sent := make(chan error, 1)
		go func() { sent <- op.Send(cctx, []control.HostKey{{Key: "Up"}}) }()
		<-h.hold
		start := time.Now()
		if how == "End" {
			go op.End()
		} else {
			cancel()
		}
		select {
		case <-op.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the operation did not end during the batch", how)
		}
		if err := <-sent; err == nil {
			t.Errorf("%s: the stopped batch is no error", how)
		}
		// The daemon's side has ended too: the pane is free.
		var again control.HostOperation
		for again, err = cl.OperateHost(ctx, "%1"); errors.Is(err, core.ErrBusy) && time.Since(start) < 5*time.Second; again, err = cl.OperateHost(ctx, "%1") {
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v", how, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: took %v", how, d)
		}
		h.mu.Lock()
		h.hold = nil
		h.mu.Unlock()
		again.End()
		<-again.Done()
		h.mu.Lock()
		h.hold = make(chan struct{})
		h.mu.Unlock()
		cancel()
	}
}
