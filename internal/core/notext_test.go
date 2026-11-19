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

// A caller that may not read text is refused HostScreen of every pane,
// with one log line and before any pane is read: a pane in which none of
// hataori's processes runs may still show a session, as after `hataori
// attach` ends. A pane that does not exist is refused too, so the
// refusal tells nothing about the panes. The owner with the grant reads
// the pane.
func TestNoTextHostScreenIsRefused(t *testing.T) {
	ctx := context.Background()
	owner := control.Caller{Principal: "owner", May: control.OpHost}
	quiet := owner
	quiet.NoText = true
	for _, pane := range []string{"%1", "%9"} {
		f := newFixture(t, fakeOwner{})
		h := &fakeHostTerms{}
		f.core.HostTerms = h
		f.log.Reset()
		screen, err := f.core.As(quiet).HostScreen(ctx, pane)
		if !errors.Is(err, ErrForbidden) || screen != "" {
			t.Errorf("pane %s, without the grant: %q, %v", pane, screen, err)
		}
		if n := strings.Count(f.log.String(), "\n"); n != 1 || !strings.Contains(f.log.String(), "control call refused") {
			t.Errorf("pane %s: the refusal logged %d lines: %s", pane, n, f.log.String())
		}
		if len(h.owned) != 0 {
			t.Errorf("pane %s: a refused HostScreen read the host's terminals", pane)
		}
	}

	f := newFixture(t, fakeOwner{})
	f.core.HostTerms = &fakeHostTerms{}
	if screen, err := f.core.As(owner).HostScreen(ctx, "%1"); err != nil || screen != "screen" {
		t.Errorf("with the grant: %q, %v", screen, err)
	}
}
