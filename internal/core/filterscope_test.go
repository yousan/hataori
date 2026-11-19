package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// A filter that names a thread of a scope the caller may not see is
// answered as one that names a thread that does not exist, and the answer
// names no scope outside the caller's (#469). The log line names both.
func TestFilterOfAThreadOfAnotherScope(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	const hiddenScope = "hidden-scope"
	f.core.Scopes[hiddenScope] = Scope{Enabled: true, Owner: "chat"}
	f.core.People = people.New([]people.Person{
		{Name: "owner", IDs: []string{"chat:1"}},
		{Name: "grace", IDs: []string{"chat:2"}},
	}, "owner", map[string][]string{"s": {"grace"}})
	own, err := f.owner.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.core.As(control.Caller{Principal: "owner", Scopes: []string{hiddenScope}, May: all}).
		Start(ctx, control.StartRequest{Key: "k2", Scope: hiddenScope, Thread: "t2", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	missing := hidden + 1000

	callers := map[string]control.Caller{
		// A supervisor's or a plugin's: limited to one scope.
		"one scope": {Principal: "x", Scopes: []string{"s"}, May: control.OpRead},
		// A person's command in chat: the hidden scope is the caller's, but
		// its person may not read it.
		"a person's": {From: "chat:2", Scopes: []string{"s", hiddenScope}, May: control.OpRead, ReadAsPerson: true},
	}
	// The filters with the thread left out: one naming the caller's scope,
	// one naming none, and one naming a scope that is not the caller's.
	filters := map[string]control.Filter{
		"the caller's scope": {Scope: "s"},
		"no scope":           {},
		"another scope":      {Scope: "elsewhere"},
	}
	threads := func(a control.API, fl control.Filter) string {
		ts, err := a.Threads(ctx, fl)
		return fmt.Sprintf("%d threads, error %v", len(ts), err)
	}
	// events reads up to the caught_up mark, whose position is part of
	// the answer.
	events := func(a control.API, fl control.Filter) string {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		evs, err := a.Events(ctx, fl, 0)
		if err != nil {
			return fmt.Sprintf("error %v", err)
		}
		n := 0
		for e := range evs {
			if e.Kind == control.CaughtUp {
				return fmt.Sprintf("%d events, caught up at %d", n, e.Pos)
			}
			n++
		}
		return "no caught_up mark"
	}

	// The thread exists, and the caller reads its own scope's thread.
	if got := threads(f.core.As(control.Caller{Principal: "owner", Scopes: []string{hiddenScope}, May: control.OpRead}),
		control.Filter{Thread: hidden}); got != "1 threads, error <nil>" {
		t.Fatalf("the hidden scope's own caller: %s", got)
	}
	for cname, c := range callers {
		if got := threads(f.core.As(c), control.Filter{Scope: "s", Thread: own}); got != "1 threads, error <nil>" {
			t.Fatalf("%s caller, its own scope's thread: %s", cname, got)
		}
	}

	for cname, c := range callers {
		a := f.core.As(c)
		for fname, fl := range filters {
			for oname, read := range map[string]func(control.API, control.Filter) string{"Threads": threads, "Events": events} {
				hf, mf := fl, fl
				hf.Thread, mf.Thread = hidden, missing
				f.log.Reset()
				gotHidden := read(a, hf)
				logged := f.log.String()
				gotMissing := read(a, mf)
				if gotHidden != gotMissing {
					t.Errorf("%s by %s caller, %s: a thread of the hidden scope: %s; no such thread: %s", oname, cname, fname, gotHidden, gotMissing)
				}
				if strings.Contains(gotHidden, hiddenScope) {
					t.Errorf("%s by %s caller, %s: the answer names the hidden scope: %s", oname, cname, fname, gotHidden)
				}
				if !strings.Contains(logged, "thread_scope="+hiddenScope) {
					t.Errorf("%s by %s caller, %s: the log does not name the thread's scope: %q", oname, cname, fname, logged)
				}
			}
		}
	}
}
