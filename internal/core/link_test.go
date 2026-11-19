package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// clientOwner opens conversations of the client name and counts them.
type clientOwner struct {
	name string
	n    int
}

func (o *clientOwner) NewThread(_ context.Context, channel, _, _ string) (control.LinkRef, error) {
	o.n++
	return control.LinkRef{Client: o.name, Channel: channel, Thread: fmt.Sprint("opened-", o.n)}, nil
}

func links(t *testing.T, f *fixture, id control.ThreadID) []control.LinkRef {
	t.Helper()
	ts, err := f.owner.Threads(context.Background(), control.Filter{Thread: id})
	if err != nil || len(ts) != 1 {
		t.Fatalf("thread %d: %v, %v", id, ts, err)
	}
	return ts[0].Links
}

// A link is made in the scope's own channel or in one its show_in names,
// once per conversation; a conversation of another thread, and a channel
// not in show_in, are refused, make nothing and write one log line.
func TestLinkOnlyWhereShowInAllows(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	owner := &clientOwner{name: "two"}
	f.core.Owners["two"] = owner
	sc := f.core.Scopes["s"]
	sc.ShowIn = []string{"two:d1"}
	f.core.Scopes["s"] = sc
	a, _ := f.running(t, "t1")
	b, _ := f.running(t, "t2")
	opened := owner.n

	// An existing conversation in the scope's own channel, twice.
	l := control.LinkRef{Client: "chat", Channel: "s", Thread: "c-extra"}
	for range 2 {
		if got, err := f.owner.Link(ctx, a, l); err != nil || got != l {
			t.Fatalf("Link %v: %v, %v", l, got, err)
		}
	}
	if n := len(links(t, f, a)); n != 2 {
		t.Fatalf("%d links after linking one conversation twice, want 2", n)
	}

	// A conversation another thread shows.
	if _, err := f.owner.Link(ctx, b, l); !errors.Is(err, control.ErrLinkedElsewhere) {
		t.Fatalf("Link of thread %d's conversation to %d: %v", a, b, err)
	}

	// A new conversation in a channel show_in names, opened once.
	got, err := f.owner.Link(ctx, a, control.LinkRef{Client: "two", Channel: "d1"})
	if err != nil || got.Client != "two" || got.Channel != "d1" || got.Thread == "" || owner.n != opened+1 {
		t.Fatalf("Link in two:d1: %v, %v; opened %d", got, err, owner.n-opened)
	}
	again, err := f.owner.Link(ctx, a, control.LinkRef{Client: "two", Channel: "d1"})
	if err != nil || again != got || owner.n != opened+1 {
		t.Fatalf("Link in two:d1 again: %v, %v; opened %d", again, err, owner.n-opened)
	}

	// A channel not in show_in: nothing is made or opened, one line.
	before := len(links(t, f, a))
	f.log.Reset()
	for _, l := range []control.LinkRef{{Client: "two", Channel: "d2"}, {Client: "chat", Channel: "d1"}, {Client: "two", Channel: "s", Thread: "x"}} {
		if _, err := f.owner.Link(ctx, a, l); !errors.Is(err, ErrForbidden) {
			t.Fatalf("Link in %s:%s: %v, want refused", l.Client, l.Channel, err)
		}
	}
	if got := f.log.String(); strings.Count(got, "link refused") != 3 || !strings.Contains(got, "client=two channel=d2") ||
		!strings.Contains(got, "thread=1") {
		t.Fatalf("logged %q", got)
	}
	// Also for a caller that may not see the thread's scope.
	f.log.Reset()
	stranger := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"elsewhere"}, May: all})
	if _, err := stranger.Link(ctx, a, control.LinkRef{Client: "two", Channel: "d2"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Link by a caller of another scope: %v", err)
	}
	if got := f.log.String(); !strings.Contains(got, "link refused") || !strings.Contains(got, "thread=1 client=two channel=d2") {
		t.Fatalf("logged %q", got)
	}
	if n := len(links(t, f, a)); n != before || owner.n != opened+1 {
		t.Fatalf("refused links made %d links and opened %d conversations", n-before, owner.n-opened-1)
	}
}
