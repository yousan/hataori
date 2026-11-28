package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// The child deliberately retains its resources after observing cancellation.
// It can still publish buffered chunks while unwinding; the API must drop
// those chunks and join it before closing its own stream or opening another.
type heldTerminalChild struct {
	sid                         int64
	run                         string
	after                       uint64
	ctx                         context.Context
	canceled, release, finished chan struct{}
	once                        sync.Once
}

func (c *heldTerminalChild) finish() { c.once.Do(func() { close(c.release) }) }

type heldTerminalRunner struct {
	Runner
	mu              sync.Mutex
	children        []*heldTerminalChild
	calls           chan *heldTerminalChild
	failReplacement bool
}

func (r *heldTerminalRunner) Terminal(ctx context.Context, sid int64, run string, after uint64) (<-chan control.TerminalChunk, error) {
	c := &heldTerminalChild{sid: sid, run: run, after: after, ctx: ctx, canceled: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	r.mu.Lock()
	r.children = append(r.children, c)
	fail := r.failReplacement && len(r.children) > 1
	r.mu.Unlock()
	r.calls <- c
	if fail {
		close(c.finished)
		return nil, errors.New("fixture replacement unavailable")
	}
	out := make(chan control.TerminalChunk, 2)
	go func() {
		<-ctx.Done()
		close(c.canceled)
		<-c.release
		out <- control.TerminalChunk{Run: "discarded", Pos: 1}
		close(c.finished)
		close(out)
	}()
	return out, nil
}

func (r *heldTerminalRunner) releaseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.children {
		c.finish()
	}
}

func terminalChild(t *testing.T, r *heldTerminalRunner) *heldTerminalChild {
	t.Helper()
	select {
	case c := <-r.calls:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("runner stream did not open")
		return nil
	}
}

func awaitTerminalSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(what)
	}
}

func terminalJoinFixture(t *testing.T, failReplacement bool) (*fixture, *heldTerminalRunner, context.Context, context.CancelFunc, control.ThreadID, int64) {
	t.Helper()
	f := newFixture(t, fakeOwner{key: "terminal-join"})
	id, sid := f.running(t, "terminal-join")
	r := &heldTerminalRunner{Runner: f.core.Runner, calls: make(chan *heldTerminalChild, 4), failReplacement: failReplacement}
	f.core.Runner = r
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); r.releaseAll() })
	return f, r, ctx, cancel, id, sid
}

func closedTerminal(t *testing.T, out <-chan control.TerminalChunk) {
	t.Helper()
	select {
	case chunk, ok := <-out:
		if ok {
			t.Fatalf("canceled child chunk escaped: %+v", chunk)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("API stream did not close after child finished")
	}
}

func TestTerminalJoinsChildBeforeClosing(t *testing.T) {
	f, r, ctx, cancel, id, _ := terminalJoinFixture(t, false)
	out, err := f.owner.Terminal(ctx, id, "original", 7)
	if err != nil {
		t.Fatal(err)
	}
	child := terminalChild(t, r)
	cancel()
	awaitTerminalSignal(t, child.canceled, "child did not observe cancellation")
	select {
	case <-out:
		t.Error("API stream closed before its child released resources")
	case <-time.After(30 * time.Millisecond):
	}
	child.finish()
	closedTerminal(t, out)
	select {
	case <-child.finished:
	default:
		t.Fatal("API stream outlived child join")
	}
}

func TestTerminalJoinsOldChildBeforeReplacement(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "open-error"
		}
		t.Run(name, func(t *testing.T) {
			f, r, ctx, cancel, id, sid := terminalJoinFixture(t, fail)
			out, err := f.owner.Terminal(ctx, id, "original", 7)
			if err != nil {
				t.Fatal(err)
			}
			old := terminalChild(t, r)
			// Add only disposable metadata, without starting a process/agent.
			result, err := f.core.Store.DB.Exec(`INSERT INTO sessions (thread_id,agent,state,created_at,updated_at,spec) SELECT thread_id,agent,state,created_at,updated_at,spec FROM sessions WHERE id=?`, sid)
			if err != nil {
				t.Fatal(err)
			}
			nextID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			awaitTerminalSignal(t, old.canceled, "old child not canceled after session replacement")
			select {
			case c := <-r.calls:
				c.finish()
				t.Errorf("replacement %d opened while old child still held resources", c.sid)
			case <-time.After(30 * time.Millisecond):
			}
			old.finish()
			next := terminalChild(t, r)
			if next.sid != nextID || next.run != "" || next.after != 0 {
				t.Fatalf("replacement binding: sid=%d run=%q after=%d", next.sid, next.run, next.after)
			}
			if fail {
				closedTerminal(t, out)
				if next.ctx.Err() == nil {
					t.Fatal("failed replacement retained its context")
				}
				return
			}
			cancel()
			awaitTerminalSignal(t, next.canceled, "replacement child not canceled")
			select {
			case <-out:
				t.Error("API stream closed before replacement child finished")
			case <-time.After(30 * time.Millisecond):
			}
			next.finish()
			closedTerminal(t, out)
		})
	}
}

func TestTerminalCancellationDuringReplacementJoinsOldChild(t *testing.T) {
	f, r, ctx, cancel, id, sid := terminalJoinFixture(t, false)
	out, err := f.owner.Terminal(ctx, id, "original", 7)
	if err != nil {
		t.Fatal(err)
	}
	old := terminalChild(t, r)
	if _, err := f.core.Store.DB.Exec(`INSERT INTO sessions (thread_id,agent,state,created_at,updated_at,spec) SELECT thread_id,agent,state,created_at,updated_at,spec FROM sessions WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, old.canceled, "old child not canceled for replacement")
	cancel()
	select {
	case <-out:
		t.Error("canceled API did not join held old child")
	case <-time.After(30 * time.Millisecond):
	}
	old.finish()
	select {
	case next := <-r.calls:
		next.finish()
		t.Error("replacement opened after request canceled during child join")
		closedTerminal(t, out)
	case _, ok := <-out:
		if ok {
			t.Fatal("canceled child chunk escaped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled replacement did not end after old child joined")
	}
}
