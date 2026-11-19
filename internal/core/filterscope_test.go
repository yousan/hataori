package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// A filter that names a thread of a scope the caller may not see, by its
// number or by a link, is answered as one that names a thread that does
// not exist, and the answer names no scope outside the caller's but the
// one the filter names (#469). The log line names the thread's scope.
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
	var hiddenLink control.LinkRef
	if err := f.core.Store.DB.QueryRow(`SELECT client, channel, thread FROM links WHERE thread_id = ?`, int64(hidden)).
		Scan(&hiddenLink.Client, &hiddenLink.Channel, &hiddenLink.Thread); err != nil {
		t.Fatal(err)
	}
	noLink := hiddenLink
	noLink.Thread = "no-such-conversation"

	callers := map[string]control.Caller{
		// A supervisor's or a plugin's: limited to one scope.
		"one scope": {Principal: "x", Scopes: []string{"s"}, May: control.OpRead},
		// A person's command in chat: the hidden scope is the caller's, but
		// its person may not read it.
		"a person's": {From: "chat:2", Scopes: []string{"s", hiddenScope}, May: control.OpRead, ReadAsPerson: true},
	}
	// The filters without the thread: one naming the caller's scope, one
	// naming none, and one naming a scope that is not the caller's.
	filters := map[string]control.Filter{
		"the caller's scope": {Scope: "s"},
		"no scope":           {},
		"another scope":      {Scope: "elsewhere"},
	}
	// How a filter names the thread: the hidden one, and one that does
	// not exist.
	names := map[string]func(control.Filter) (hidden, missing control.Filter){
		"by number": func(fl control.Filter) (control.Filter, control.Filter) {
			h, m := fl, fl
			h.Thread, m.Thread = hidden, hidden+1000
			return h, m
		},
		"by link": func(fl control.Filter) (control.Filter, control.Filter) {
			h, m := fl, fl
			h.Link, m.Link = &hiddenLink, &noLink
			return h, m
		},
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
	reads := map[string]func(control.API, control.Filter) string{"Threads": threads, "Events": events}

	// The thread exists, by its number and by its link, and each caller
	// reads its own scope's thread.
	for nname, name := range names {
		h, _ := name(control.Filter{})
		if got := threads(f.core.As(control.Caller{Principal: "owner", Scopes: []string{hiddenScope}, May: control.OpRead}), h); got != "1 threads, error <nil>" {
			t.Fatalf("the hidden scope's own caller, %s: %s", nname, got)
		}
	}
	for cname, c := range callers {
		if got := threads(f.core.As(c), control.Filter{Scope: "s", Thread: own}); got != "1 threads, error <nil>" {
			t.Fatalf("%s caller, its own scope's thread: %s", cname, got)
		}
	}

	for cname, c := range callers {
		a := f.core.As(c)
		for fname, fl := range filters {
			// The scopes the answer may not name: every scope that is not
			// the caller's, but the one the filter names; and the hidden
			// one, which even the person's caller does not learn of here.
			var unnamed []string
			for _, s := range []string{"s", "off", hiddenScope} {
				if s == hiddenScope || (!slices.Contains(c.Scopes, s) && s != fl.Scope) {
					unnamed = append(unnamed, s)
				}
			}
			for nname, name := range names {
				hf, mf := name(fl)
				for oname, read := range reads {
					what := fmt.Sprintf("%s by %s caller, %s, %s", oname, cname, fname, nname)
					f.log.Reset()
					gotHidden := read(a, hf)
					logged := f.log.String()
					gotMissing := read(a, mf)
					if gotHidden != gotMissing {
						t.Errorf("%s: a thread of the hidden scope: %s; no such thread: %s", what, gotHidden, gotMissing)
					}
					for _, s := range unnamed {
						if strings.Contains(gotHidden, strconv.Quote(s)) {
							t.Errorf("%s: the answer names scope %q: %s", what, s, gotHidden)
						}
					}
					if !strings.Contains(logged, "thread_scope="+hiddenScope) {
						t.Errorf("%s: the log does not name the thread's scope: %q", what, logged)
					}
				}
			}
		}
	}

	// A filter whose thread is in another of the caller's own scopes is
	// still refused.
	both := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s", hiddenScope}, May: control.OpRead})
	if _, err := both.Threads(ctx, control.Filter{Scope: "s", Thread: hidden}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a filter naming two of the caller's scopes: %v, want refused", err)
	}
}
