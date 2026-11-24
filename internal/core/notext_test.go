package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
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

// A caller that may not read text is refused HostScreen and HostView of
// every pane, with one log line and before any pane is read: a pane in
// which none of hataori's processes runs may still show a session, as
// after `hataori attach` ends. A pane that does not exist is refused too,
// so the refusal tells nothing about the panes. The owner with the grant
// reads the pane, and HostView gives its cursor.
func TestNoTextHostScreenIsRefused(t *testing.T) {
	ctx := context.Background()
	owner := control.Caller{Principal: "owner", May: control.OpHost}
	quiet := owner
	quiet.NoText = true
	reads := map[string]func(a control.API, pane string) (string, error){
		"HostScreen": func(a control.API, pane string) (string, error) { return a.HostScreen(ctx, pane) },
		"HostView": func(a control.API, pane string) (string, error) {
			v, err := a.HostView(ctx, pane)
			if err == nil && (v.CursorX != 2 || v.CursorY != 1) {
				return "", errors.New("no cursor")
			}
			return v.Screen, err
		},
	}
	for op, read := range reads {
		for _, pane := range []string{"%1", "%9"} {
			f := newFixture(t, fakeOwner{})
			h := &fakeHostTerms{}
			f.core.HostTerms = h
			f.log.Reset()
			screen, err := read(f.core.As(quiet), pane)
			if !errors.Is(err, ErrForbidden) || screen != "" {
				t.Errorf("%s of pane %s, without the grant: %q, %v", op, pane, screen, err)
			}
			if n := strings.Count(f.log.String(), "\n"); n != 1 || !strings.Contains(f.log.String(), "control call refused") {
				t.Errorf("%s of pane %s: the refusal logged %d lines: %s", op, pane, n, f.log.String())
			}
			if len(h.owned) != 0 {
				t.Errorf("%s of pane %s: a refused read read the host's terminals", op, pane)
			}
		}
		f := newFixture(t, fakeOwner{})
		f.core.HostTerms = &fakeHostTerms{}
		if screen, err := read(f.core.As(owner), "%1"); err != nil || screen != "screen" {
			t.Errorf("%s with the grant: %q, %v", op, screen, err)
		}
	}
}

// Thread lists the posts the chat service refused, newest last, from their
// post_refused events, and without their reasons for a caller that may not
// read text.
func TestRefusedPosts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, _ := f.running(t, "t1")
	for i := range control.RefusedMax + 2 {
		data := fmt.Sprintf(`{"link":{"client":"c","channel":"s","thread":"t1"},"event":%d,"part":0,"reason":"it says words"}`, i)
		if err := f.core.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.Append(ctx, control.Event{Thread: id, Scope: "s", Time: time.Now(), Kind: "post_refused", Data: []byte(data)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := f.owner.Thread(ctx, id, 0, 0)
	if err != nil || len(d.Refused) != control.RefusedMax || d.Refused[0].Event != 2 || d.Refused[0].Reason != "it says words" || d.Refused[0].Time.IsZero() {
		t.Fatalf("refused posts %+v, %v", d.Refused, err)
	}
	quiet := f.core.As(control.Caller{Principal: "plugin", Scopes: []string{"s"}, May: control.OpRead, NoText: true})
	d, err = quiet.Thread(ctx, id, 0, control.MaxEvents)
	if err != nil || len(d.Refused) != control.RefusedMax {
		t.Fatalf("refused posts %+v, %v", d.Refused, err)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "words") {
		t.Fatalf("a caller that may not read text got %s", b)
	}
}
