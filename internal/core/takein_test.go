package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/window"
)

// takeInRunner is the fake runner that also takes windows in.
type takeInRunner struct {
	*fakeRunner
	taken    map[string]bool // by pane
	checkErr error
	took     []window.Report
}

func (r *takeInRunner) CheckTakeIn(_ context.Context, rep window.Report) error { return r.checkErr }
func (r *takeInRunner) TakenIn(rep window.Report) (bool, error)                { return r.taken[rep.Pane], nil }
func (r *takeInRunner) TakeIn(_ context.Context, l Launch, rep window.Report) error {
	r.took = append(r.took, rep)
	r.taken[rep.Pane] = true
	return r.record("take in " + rep.Session)
}
func (r *takeInRunner) Release(_ context.Context, sid int64) error { return r.record("release") }
func (r *takeInRunner) RemoveState(_ context.Context, sid int64) error {
	return r.record(fmt.Sprint("remove state ", sid))
}

// windows is a fixed set of reports, by window name.
type windows map[string]window.Report

func (w windows) Report(name string) (window.Report, bool) { r, ok := w[name]; return r, ok }

const takeWindow = "tmux:/tmp/tmux-1000/default,4242,%7"

func takeInFixture(t *testing.T) (*fixture, *takeInRunner, control.API) {
	t.Helper()
	f := newFixture(t, fakeOwner{key: "took"})
	tr := &takeInRunner{fakeRunner: f.runner, taken: map[string]bool{}}
	f.core.Runner = tr
	rep := window.Report{Holder: window.Tmux, Server: "/tmp/tmux-1000/default,4242", Pane: "%7", Device: "/dev/pts/9", PID: 4300,
		Agent: "fake", Session: "sess-1", Dir: "/home/alice/src/api", Event: "SessionStart", Model: "m1", Interactive: true}
	dash := rep
	dash.Pane, dash.Interactive, dash.Session = "%8", false, "sess-p"
	f.core.Windows = windows{takeWindow: rep, "tmux:/tmp/tmux-1000/default,4242,%8": dash}
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s", "off"}, May: all | control.OpTakeIn})
	return f, tr, owner
}

// Every refusal changes nothing: a scope not configured, a window with no
// report, a session that is not interactive, a window taken in already,
// and one whose check fails. Each is logged.
func TestTakeInRefusesAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	f, tr, owner := takeInFixture(t)
	events, threads := count(t, f.core.Store, "events"), count(t, f.core.Store, "threads")
	refused := func(what string, req control.TakeInRequest, want error) {
		t.Helper()
		f.log.Reset()
		if _, err := owner.TakeIn(ctx, req); !errors.Is(err, want) {
			t.Fatalf("%s: %v, want %v", what, err, want)
		}
		if !strings.Contains(f.log.String(), "take-in refused") && !strings.Contains(f.log.String(), "control call refused") {
			t.Fatalf("%s: not logged: %s", what, f.log.String())
		}
		if count(t, f.core.Store, "events") != events || count(t, f.core.Store, "threads") != threads || len(tr.took) != 0 {
			t.Fatalf("%s changed something", what)
		}
	}
	refused("a scope that is not configured", control.TakeInRequest{Window: takeWindow, Scope: "nowhere", Key: "k1"}, control.ErrForbidden)
	refused("a scope that is not enabled", control.TakeInRequest{Window: takeWindow, Scope: "off", Key: "k1"}, ErrDisabled)
	refused("no report", control.TakeInRequest{Window: "tmux:/tmp/tmux-1000/default,4242,%9", Scope: "s", Key: "k1"}, ErrNotFound)
	refused("not interactive", control.TakeInRequest{Window: "tmux:/tmp/tmux-1000/default,4242,%8", Scope: "s", Key: "k1"}, session.ErrRefused)
	tr.checkErr = errors.New("the process that reported is not the window's foreground process")
	refused("the check fails", control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k1"}, session.ErrRefused)
	tr.checkErr = nil
	tr.taken["%7"] = true
	refused("taken in already", control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k1"}, session.ErrRefused)
	if f.runner.Calls() != "" {
		t.Fatalf("the runner was asked: %s", f.runner.Calls())
	}
}

// A take-in makes the thread as Start does, with its first link and the
// window's session, its workspace recorded as not hataori's, which Remove
// refuses. The same key returns the same thread; another is refused once
// the window is taken in.
func TestTakeInMakesTheThread(t *testing.T) {
	ctx := context.Background()
	f, tr, owner := takeInFixture(t)
	id, err := owner.TakeIn(ctx, control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	var (
		scope, workspace, agentName, sid, title string
		foreign                                 bool
	)
	if err := f.core.Store.DB.QueryRow(`SELECT t.scope, t.workspace, t.foreign_workspace, t.title, s.agent, s.agent_session_id
		FROM threads t JOIN sessions s ON s.thread_id = t.id WHERE t.id = ?`, int64(id)).
		Scan(&scope, &workspace, &foreign, &title, &agentName, &sid); err != nil {
		t.Fatal(err)
	}
	if scope != "s" || workspace != "/home/alice/src/api" || !foreign || title != "api" || agentName != "fake" || sid != "sess-1" {
		t.Fatalf("thread %q %q %v %q, session %q %q", scope, workspace, foreign, title, agentName, sid)
	}
	var links int
	f.core.Store.DB.QueryRow(`SELECT count(*) FROM links WHERE thread_id = ? AND client = 'chat' AND channel = 's' AND thread = 'took'`, int64(id)).Scan(&links)
	if links != 1 || f.runner.Calls() != "take in sess-1" {
		t.Fatalf("%d first links; runner: %s", links, f.runner.Calls())
	}
	if again, err := owner.TakeIn(ctx, control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k1"}); err != nil || again != id {
		t.Fatalf("the same key again: %v, %v", again, err)
	}
	if _, err := owner.TakeIn(ctx, control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k2"}); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("another key for a window taken in: %v", err)
	}
	if len(tr.took) != 1 {
		t.Fatalf("%d take-ins", len(tr.took))
	}

	// Closed, it is removed without its workspace, which never reaches the
	// remover: hataori did not make it. Its session's state goes.
	sidN, _, _ := f.core.current(ctx, f.core.Store.DB, id)
	f.agentEvent(t, sidN, 1, agent.Event{Kind: agent.SessionReady})
	if err := f.core.Machine.Apply(ctx, sidN, session.Input{Kind: session.Close, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	f.core.Remover = refuseAll{}
	if err := f.core.remove(ctx, id, "owner", ByCommand); err != nil {
		t.Fatalf("removing a thread taken in: %v", err)
	}
	var state string
	f.core.Store.DB.QueryRow(`SELECT state FROM threads WHERE id = ?`, int64(id)).Scan(&state)
	if state != "removed" || !strings.Contains(f.runner.Calls(), fmt.Sprint("remove state ", sidN)) {
		t.Fatalf("thread %s; runner: %s", state, f.runner.Calls())
	}
}

// The removal pass of closed threads skips one taken in: its directory is
// the person's.
func TestRemovalPassSkipsATakenInThread(t *testing.T) {
	ctx := context.Background()
	f, _, owner := takeInFixture(t)
	id, err := owner.TakeIn(ctx, control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, _ := f.core.current(ctx, f.core.Store.DB, id)
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
	if err := f.core.Machine.Apply(ctx, sid, session.Input{Kind: session.Close, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	f.core.Store.DB.Exec(`UPDATE threads SET updated_at = 0 WHERE id = ?`, int64(id)) // closed long ago
	f.core.Remover = refuseAll{}
	if _, err := f.core.RemoveClosed(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	var state string
	f.core.Store.DB.QueryRow(`SELECT state FROM threads WHERE id = ?`, int64(id)).Scan(&state)
	if state != "closed" || strings.Contains(f.runner.Calls(), "remove state") || count(t, f.core.Store, "events") == 0 {
		t.Fatalf("the pass touched a thread taken in: %s; runner: %s", state, f.runner.Calls())
	}
	var refused int
	f.core.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'removal_refused'`).Scan(&refused)
	if refused != 0 {
		t.Fatalf("%d removal_refused events", refused)
	}
}

// refuseAll is a Remover that the test must never reach.
type refuseAll struct{}

func (refuseAll) Remove(context.Context, string) (string, error) {
	panic("the workspace of a thread taken in reached the remover")
}
