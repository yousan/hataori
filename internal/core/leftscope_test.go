package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// The owner reaches the threads of a scope that left the config: they are
// listed, read, stopped, closed and removed. Nothing starts in it: a send,
// a reopen, a choice for a blocked start and an attached terminal, where a
// person would type, are refused as disabled, logged once each, and change nothing (#565).
func TestNothingStartsInAScopeThatLeftTheConfig(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	rm := &fakeRemover{}
	f.core.Remover = rm
	idle, _ := f.idle(t, "idle")
	asleep, _ := f.asleep(t, "asleep")
	blocked, blockedSid := f.blocked(t, "blocked", ".mcp.json")
	running, _ := f.running(t, "running")
	closed, _ := f.idle(t, "closed")
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id = 'conv', spec = '{}'`); err != nil { // as a launch and the agent give them
		t.Fatal(err)
	}
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"off", "s"},
		May: all | control.OpClose | control.OpRemove | control.OpReopen | control.OpTrust | control.OpAttach})
	if err := owner.CloseThread(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if left, err := f.core.LeftScopes(ctx); err != nil || len(left) != 0 {
		t.Fatalf("scopes that left the config before one did: %v, %v", left, err)
	}

	s := f.core.Scopes["s"]
	delete(f.core.Scopes, "s")
	if left, err := f.core.LeftScopes(ctx); err != nil || !slices.Equal(left, []string{"s"}) {
		t.Fatalf("scopes that left the config: %v, %v", left, err)
	}
	in := control.Input{Text: "more"}
	for name, call := range map[string]func() error{
		"start": func() error {
			_, err := owner.Start(ctx, control.StartRequest{Key: "n", Scope: "s", Input: in})
			return err
		},
		"send to an idle thread":     func() error { return owner.Send(ctx, idle, "k1", in, control.Deliver) },
		"send to an asleep thread":   func() error { return owner.Send(ctx, asleep, "k2", in, control.Deliver) },
		"reopen":                     func() error { _, err := owner.ReopenThread(ctx, closed); return err },
		"trust":                      func() error { return owner.Trust(ctx, blocked, blockedSid, []string{".mcp.json"}) },
		"start without settings":     func() error { return owner.StartWithoutSettings(ctx, blocked, blockedSid) },
		"attach to an asleep thread": func() error { _, err := owner.AttachTerminal(ctx, asleep); return err },
		"attach to an idle thread":   func() error { _, err := owner.AttachTerminal(ctx, idle); return err },
	} {
		events, threads, calls := count(t, f.core.Store, "events"), count(t, f.core.Store, "threads"), f.runner.Calls()
		f.log.Reset()
		if err := call(); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: %v, want disabled", name, err)
		}
		if count(t, f.core.Store, "events") != events || count(t, f.core.Store, "threads") != threads || f.runner.Calls() != calls {
			t.Errorf("%s: the refused call changed something: %q after %q", name, f.runner.Calls(), calls)
		}
		if n := strings.Count(f.log.String(), "not in the config"); n != 1 && name != "start" {
			t.Errorf("%s: %d log lines: %s", name, n, f.log)
		}
	}
	if files, err := f.core.TrustedFiles(ctx, "s", trustRepo); err != nil || len(files) != 0 {
		t.Errorf("a refused trust kept %v, %v", files, err)
	}

	if ts, err := owner.Threads(ctx, control.Filter{}); err != nil || len(ts) != 5 {
		t.Fatalf("threads: %d, %v, want all 5", len(ts), err)
	}
	if d, err := owner.Thread(ctx, idle, 0, 0); err != nil || d.Scope != "s" {
		t.Fatalf("thread: %+v, %v", d, err)
	}
	if _, err := owner.Terminal(ctx, idle, "", 0); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	if err := owner.Stop(ctx, running, 0); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := owner.CloseThread(ctx, idle); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := owner.RemoveThread(ctx, closed); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Back in the config, all of it works again.
	f.core.Scopes["s"] = s
	if err := owner.Send(ctx, asleep, "k2", in, control.Deliver); err != nil {
		t.Fatalf("send after the scope is back: %v", err)
	}
}

// A start that a daemon accepted and did not make before it ended is not
// made by the next when the scope left the config in between: it fails, and
// the thread says why (#565).
func TestRecoverStartsNothingInAScopeThatLeftTheConfig(t *testing.T) {
	ctx := context.Background()
	for name, put := range map[string]func(f *fixture) (control.ThreadID, int64){
		"a first start": func(f *fixture) (control.ThreadID, int64) {
			id, err := f.owner.Start(ctx, control.StartRequest{Key: "k", Scope: "s", Thread: "k", Input: control.Input{Text: "go"}})
			if err != nil {
				t.Fatal(err)
			}
			sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
			if err != nil {
				t.Fatal(err)
			}
			return id, sid
		},
		"a wake": func(f *fixture) (control.ThreadID, int64) {
			id, sid := f.asleep(t, "k")
			if err := f.owner.Send(ctx, id, "k2", control.Input{Text: "more"}, control.Deliver); err != nil {
				t.Fatal(err)
			}
			return id, sid
		},
	} {
		for _, left := range []bool{false, true} {
			f := newFixture(t, fakeOwner{})
			id, sid := put(f)
			if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET launch = 1 WHERE id = ?`, sid); err != nil { // accepted, not handed over
				t.Fatal(err)
			}
			if left {
				delete(f.core.Scopes, "s")
			}
			calls := f.runner.Calls()
			if err := f.core.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if started := f.runner.Calls() != calls; started == left {
				t.Errorf("%s, scope left %v: runner %q after %q", name, left, f.runner.Calls(), calls)
			}
			want := control.Creating
			if left {
				want = control.Failed
			}
			if got := f.state(t, sid); got != want {
				t.Errorf("%s, scope left %v: state %s, want %s", name, left, got, want)
			}
			if !left {
				continue
			}
			evs, err := f.owner.ThreadEvents(ctx, id, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if e := evs[len(evs)-1]; !strings.Contains(string(e.Data), "the scope is not in the config") {
				t.Errorf("%s: the last event %s %s does not say why", name, e.Kind, e.Data)
			}
		}
	}
}
