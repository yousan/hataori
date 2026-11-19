package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// A caller that may not read text is refused a thread's terminal, which
// shows the prompts and the agent's output, with one log line and no
// call to the runner; the same caller with the grant reads it.
func TestNoTextTerminalIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, _ := f.running(t, "t1")
	plugin := control.Caller{Principal: "plugin", Scopes: []string{"s"}, May: control.OpRead}

	quiet := plugin
	quiet.NoText = true
	calls := f.runner.Calls()
	f.log.Reset()
	if _, err := f.core.As(quiet).Terminal(ctx, id, "", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Terminal by a caller that may not read text: %v", err)
	}
	if n := strings.Count(f.log.String(), "\n"); n != 1 || !strings.Contains(f.log.String(), "control call refused") {
		t.Errorf("the refusal logged %d lines: %s", n, f.log.String())
	}
	if f.runner.Calls() != calls {
		t.Errorf("a refused Terminal reached the runner: %s", f.runner.Calls())
	}

	ch, err := f.core.As(plugin).Terminal(ctx, id, "", 0)
	if err != nil {
		t.Fatalf("Terminal by the same caller with the grant: %v", err)
	}
	for range ch {
	}
	if !strings.Contains(f.runner.Calls(), "terminal ") {
		t.Fatalf("runner calls: %s", f.runner.Calls())
	}
}

// For a caller that may not read text, HostScreen refuses a pane that
// shows one of hataori's sessions: one in which a hataori process runs
// when it is called, one in which it starts while the screen is read, and
// one about which that cannot be told. Every refusal is one log line.
// Other panes are read, and a caller with the grant reads them all.
func TestNoTextHostScreenOfASession(t *testing.T) {
	ctx := context.Background()
	owner := control.Caller{Principal: "owner", May: control.OpHost}
	quiet := owner
	quiet.NoText = true
	for name, tc := range map[string]struct {
		h       *fakeHostTerms
		refused bool
	}{
		"a pane that shows no session":       {&fakeHostTerms{}, false},
		"a pane that shows a session":        {&fakeHostTerms{own: map[string]bool{"%1": true}}, true},
		"a session started while it is read": {&fakeHostTerms{ownAfter: map[string]bool{"%1": true}}, true},
		"a pane that cannot be looked at":    {&fakeHostTerms{checkErr: errors.New("ps failed")}, true},
	} {
		f := newFixture(t, fakeOwner{})
		f.core.HostTerms = tc.h
		f.log.Reset()
		screen, err := f.core.As(quiet).HostScreen(ctx, "%1")
		if !tc.refused {
			if err != nil || screen != "screen" {
				t.Errorf("%s, without the grant: %q, %v", name, screen, err)
			}
			continue
		}
		if !errors.Is(err, ErrForbidden) || screen != "" {
			t.Errorf("%s, without the grant: %q, %v", name, screen, err)
		}
		if n := strings.Count(f.log.String(), "\n"); n != 1 || !strings.Contains(f.log.String(), "control call refused") {
			t.Errorf("%s: the refusal logged %d lines: %s", name, n, f.log.String())
		}
		if tc.h.checkErr != nil {
			continue
		}
		tc.h.screened = false
		if screen, err := f.core.As(owner).HostScreen(ctx, "%1"); err != nil || screen != "screen" {
			t.Errorf("%s, with the grant: %q, %v", name, screen, err)
		}
	}
}
