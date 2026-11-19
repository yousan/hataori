package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

// fakeRemover stands for workspace.Remover: it records the directories it
// is asked to remove, and refuses them while refuse is set, or the n-th
// call (from 0) that only says.
type fakeRemover struct {
	mu     sync.Mutex
	asked  []string
	refuse *fakeRefusal
	only   func(n int) bool
}

type fakeRefusal struct{ reason, repo, kind, detail string }

func (r *fakeRefusal) Error() string { return "refused: " + r.reason }
func (r *fakeRefusal) RemovalRefused() (string, string, string, string) {
	return r.reason, r.repo, r.kind, r.detail
}

func (r *fakeRemover) Remove(_ context.Context, dir string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.asked)
	r.asked = append(r.asked, dir)
	if r.refuse != nil && (r.only == nil || r.only(n)) {
		return "", r.refuse
	}
	return "trash/" + dir, nil
}

func (r *fakeRemover) Asked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

const closer = all | control.OpClose | control.OpRemove

// idle starts a thread and brings it to idle after one turn.
func (f *fixture) idle(t *testing.T, key string) (control.ThreadID, int64) {
	t.Helper()
	id, sid := f.running(t, key)
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded})
	return id, sid
}

func (f *fixture) threadState(t *testing.T, id control.ThreadID) control.State {
	t.Helper()
	_, st, err := f.core.current(context.Background(), f.core.Store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// eventsOf returns the data of the thread's events of kind.
func (f *fixture) eventsOf(t *testing.T, id control.ThreadID, kind control.EventKind) []map[string]any {
	t.Helper()
	rows, err := f.core.Store.DB.Query(`SELECT data FROM events WHERE thread_id = ? AND kind = ? ORDER BY pos`,
		int64(id), string(kind))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		var m map[string]any
		json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}

// Closing an idle thread stops its agent through the runner and leaves the
// thread closed; a message to it then starts nothing, is not accepted, and
// is answered ErrClosed. Closing again changes nothing.
func TestCloseAnIdleThread(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})
	id, _ := f.idle(t, "t1")
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st := f.threadState(t, id); st != control.Closed {
		t.Fatalf("state %s, want closed", st)
	}
	if !strings.HasSuffix(f.runner.Calls(), "shut") {
		t.Fatalf("runner calls %q: the agent was not stopped", f.runner.Calls())
	}
	closed := f.eventsOf(t, id, session.KindState)
	if last := closed[len(closed)-1]; last["to"] != "closed" || last["reason"] != ByCommand {
		t.Fatalf("last state event %v", last)
	}
	events, turns, inbox, calls := count(t, f.core.Store, "events"), count(t, f.core.Store, "turns"), count(t, f.core.Store, "inbox"), f.runner.Calls()
	if err := owner.Send(ctx, id, "m-late", control.Input{Text: "wake up"}, control.Deliver); !errors.Is(err, ErrClosed) {
		t.Fatalf("send to a closed thread: %v, want ErrClosed", err)
	}
	if count(t, f.core.Store, "events") != events || count(t, f.core.Store, "turns") != turns ||
		count(t, f.core.Store, "inbox") != inbox || f.runner.Calls() != calls || f.threadState(t, id) != control.Closed {
		t.Fatal("a message to a closed thread changed something")
	}
	// Closing again closes nothing, and makes sure again that the agent
	// has ended, as after a stop that failed.
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatalf("closing a closed thread: %v", err)
	}
	if count(t, f.core.Store, "events") != events || f.threadState(t, id) != control.Closed || f.runner.Calls() != calls+"; shut" {
		t.Fatalf("a second close: runner calls %q", f.runner.Calls())
	}
}

// A thread with a turn running or a question open is not closed.
func TestCloseRefusesARunningOrWaitingThread(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})
	running, _ := f.running(t, "t1")
	waiting, sid := f.running(t, "t2")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1"}})
	for _, id := range []control.ThreadID{running, waiting} {
		was := f.threadState(t, id)
		if err := owner.CloseThread(ctx, id); !errors.Is(err, session.ErrRefused) {
			t.Fatalf("close of a %s thread: %v, want refused", was, err)
		}
		if f.threadState(t, id) != was {
			t.Fatalf("a refused close changed the state of a %s thread", was)
		}
	}
	if strings.Contains(f.runner.Calls(), "shut") {
		t.Fatalf("runner calls %q", f.runner.Calls())
	}
}

// Remove asks the remover only for a closed thread. A thread that is not
// closed is refused with one removal_refused event, and its workspace is
// not asked about.
func TestRemoveOnlyAClosedThread(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	rm := &fakeRemover{}
	f.core.Remover = rm
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})
	idle, _ := f.idle(t, "t1")
	running, _ := f.running(t, "t2")
	for _, id := range []control.ThreadID{idle, running} {
		was := f.threadState(t, id)
		if err := owner.RemoveThread(ctx, id); !errors.Is(err, session.ErrRefused) {
			t.Fatalf("remove of a %s thread: %v, want refused", was, err)
		}
		if f.threadState(t, id) != was {
			t.Fatalf("a refused removal changed a %s thread", was)
		}
		ev := f.eventsOf(t, id, KindRemovalRefused)
		if len(ev) != 1 || ev[0]["reason"] != NotClosed || ev[0]["detail"] != string(was) || ev[0]["trigger"] != ByCommand ||
			ev[0]["principal"] != "owner" {
			t.Fatalf("removal_refused events of a %s thread: %v", was, ev)
		}
	}
	if rm.Asked() != 0 {
		t.Fatalf("the remover was asked %d times", rm.Asked())
	}
}

// A closed thread whose workspace the remover removes is removed, with a
// state event that says what asked for it. One whose removal is refused
// stays closed, and gets one removal_refused event with the reason.
func TestRemoveAClosedThread(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	rm := &fakeRemover{}
	f.core.Remover = rm
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})

	kept, _ := f.idle(t, "t1")
	if err := owner.CloseThread(ctx, kept); err != nil {
		t.Fatal(err)
	}
	rm.refuse = &fakeRefusal{reason: "unpushed commit on a branch", repo: "lib", kind: "submodule", detail: "commit abc"}
	err := owner.RemoveThread(ctx, kept)
	if !errors.Is(err, ErrRemovalRefused) || !strings.Contains(err.Error(), "unpushed commit on a branch") {
		t.Fatalf("refused removal: %v", err)
	}
	if st := f.threadState(t, kept); st != control.Closed {
		t.Fatalf("after a refused removal the thread is %s", st)
	}
	ev := f.eventsOf(t, kept, KindRemovalRefused)
	if len(ev) != 1 || ev[0]["reason"] != "unpushed commit on a branch" || ev[0]["repo"] != "lib" || ev[0]["kind"] != "submodule" ||
		ev[0]["detail"] != "commit abc" {
		t.Fatalf("removal_refused events: %v", ev)
	}
	if !strings.Contains(f.log.String(), "removal refused") {
		t.Fatalf("the refusal was not logged: %s", f.log.String())
	}

	rm.refuse = nil
	if err := owner.RemoveThread(ctx, kept); err != nil {
		t.Fatal(err)
	}
	if st := f.threadState(t, kept); st != control.Removed {
		t.Fatalf("after the removal the thread is %s", st)
	}
	states := f.eventsOf(t, kept, session.KindState)
	if last := states[len(states)-1]; last["from"] != "closed" || last["to"] != "removed" || last["reason"] != ByCommand {
		t.Fatalf("last state event %v", last)
	}
	if rm.asked[len(rm.asked)-1] != "dir" {
		t.Fatalf("the remover was asked about %v, want the thread's workspace", rm.asked)
	}
	if n := len(f.eventsOf(t, kept, KindRemovalRefused)); n != 1 {
		t.Fatalf("%d removal_refused events after the removal", n)
	}
}

// Without a remover nothing is removed, and the refusal is an event and a
// log line like any other.
func TestRemoveWithoutARemover(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})
	id, _ := f.idle(t, "t1")
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	f.log.Reset()
	if err := owner.RemoveThread(ctx, id); !errors.Is(err, ErrRemovalRefused) || f.threadState(t, id) != control.Closed {
		t.Fatalf("remove without a remover: %v, state %s", err, f.threadState(t, id))
	}
	if ev := f.eventsOf(t, id, KindRemovalRefused); len(ev) != 1 || ev[0]["reason"] != NoRemover {
		t.Fatalf("removal_refused events: %v", ev)
	}
	if !strings.Contains(f.log.String(), "removal refused") {
		t.Fatalf("log %q", f.log.String())
	}
}

// A closed thread whose agent cannot be made to end is not removed, and
// the remover is not asked.
func TestRemoveRefusesWhileTheAgentRuns(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	rm := &fakeRemover{}
	f.core.Remover = rm
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})
	id, _ := f.idle(t, "t1")
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	f.runner.shutErr = errors.New("its host has not ended")
	if err := owner.RemoveThread(ctx, id); !errors.Is(err, ErrRemovalRefused) || f.threadState(t, id) != control.Closed {
		t.Fatalf("remove while the agent runs: %v, state %s", err, f.threadState(t, id))
	}
	if ev := f.eventsOf(t, id, KindRemovalRefused); len(ev) != 1 || ev[0]["reason"] != AgentRunning {
		t.Fatalf("removal_refused events: %v", ev)
	}
	if rm.Asked() != 0 {
		t.Fatal("the remover was asked while the agent runs")
	}
}

// The removal pass removes the threads closed longer than its age, and no
// other; a thread whose removal it was refused is tried again only once the
// age has passed since.
func TestRemovalPass(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f.core.Machine.Now = func() time.Time { return now }
	rm := &fakeRemover{}
	f.core.Remover = rm
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})

	old, _ := f.idle(t, "old")
	refused, _ := f.idle(t, "refused")
	for _, id := range []control.ThreadID{old, refused} {
		if err := owner.CloseThread(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Hour)
	young, _ := f.idle(t, "young")
	if err := owner.CloseThread(ctx, young); err != nil {
		t.Fatal(err)
	}
	open, _ := f.idle(t, "open") // idle, not closed
	now = now.Add(30 * time.Minute)

	pass := func() int {
		t.Helper()
		n, err := f.core.RemoveClosed(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The pass asks by thread number: old, then refused.
	rm.refuse = &fakeRefusal{reason: "untracked file", detail: "notes.txt"}
	rm.only = func(n int) bool { return n == 1 }
	if n := pass(); n != 1 {
		t.Fatalf("first pass removed %d, want 1", n)
	}
	if f.threadState(t, old) != control.Removed || f.threadState(t, refused) != control.Closed || f.threadState(t, young) != control.Closed ||
		f.threadState(t, open) != control.Idle {
		t.Fatalf("after the first pass: old %s, refused %s, young %s, open %s",
			f.threadState(t, old), f.threadState(t, refused), f.threadState(t, young), f.threadState(t, open))
	}
	ev := f.eventsOf(t, refused, KindRemovalRefused)
	if len(ev) != 1 || ev[0]["trigger"] != ByClosedAge || ev[0]["principal"] != nil || ev[0]["reason"] != "untracked file" {
		t.Fatalf("removal_refused of the pass: %v", ev)
	}
	states := f.eventsOf(t, old, session.KindState)
	if last := states[len(states)-1]; last["to"] != "removed" || last["reason"] != ByClosedAge {
		t.Fatalf("the pass's removal: %v", last)
	}

	// Within the age, the refused thread is not tried again, and the young
	// one is not old enough.
	rm.refuse = nil
	asked := rm.Asked()
	now = now.Add(15 * time.Minute) // the young one is closed 45 minutes, the refusal 15 minutes ago
	if n := pass(); n != 0 || rm.Asked() != asked {
		t.Fatalf("second pass removed %d and asked %d times, want nothing", n, rm.Asked()-asked)
	}
	// Once the age has passed since the refusal, it is, and so is the young one.
	now = now.Add(time.Hour)
	if n := pass(); n != 2 || f.threadState(t, refused) != control.Removed || f.threadState(t, young) != control.Removed {
		t.Fatalf("third pass removed %d: refused %s, young %s", n, f.threadState(t, refused), f.threadState(t, young))
	}
	if f.threadState(t, open) != control.Idle {
		t.Fatal("the pass touched a thread that is not closed")
	}
	if _, err := f.core.RemoveClosed(ctx, 0); err == nil {
		t.Fatal("a pass without an age ran")
	}
}

// A remover that moved the workspace and then refused, as when the rename
// out of trash/pending- fails, is not reported as a refusal that changed
// nothing: the reason says so, and an error is logged.
func TestRefusalAfterTheWorkspaceMoved(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	f.core.Workspaces = dirWorkspaces{ws}
	f.core.Remover = movingRemover{}
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer})
	id, _ := f.idle(t, "t1")
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := owner.RemoveThread(ctx, id); !errors.Is(err, ErrRemovalRefused) {
		t.Fatalf("remove: %v", err)
	}
	ev := f.eventsOf(t, id, KindRemovalRefused)
	if len(ev) != 1 || ev[0]["reason"] != MovedNotRemoved+": not a workspace" || f.threadState(t, id) != control.Closed {
		t.Fatalf("removal_refused events %v, state %s", ev, f.threadState(t, id))
	}
	if !strings.Contains(f.log.String(), "removal refused after the workspace was moved") {
		t.Fatalf("log %q", f.log.String())
	}
}

// dirWorkspaces makes every workspace at dir.
type dirWorkspaces struct{ dir string }

func (w dirWorkspaces) Make(context.Context, control.ThreadID, string, func(Placement) error) (string, error) {
	return w.dir, nil
}
func (dirWorkspaces) Adopt(context.Context, control.ThreadID, Placement) (string, bool, error) {
	return "", false, nil
}

func (dirWorkspaces) Empty(context.Context, control.ThreadID) (string, error) {
	return "", errors.New("no confined threads here")
}
func (dirWorkspaces) State(context.Context, string) (control.WorkspaceState, error) {
	return control.WorkspaceState{}, nil
}

// movingRemover moves the directory aside and then refuses.
type movingRemover struct{}

func (movingRemover) Remove(_ context.Context, dir string) (string, error) {
	if err := os.Rename(dir, dir+".pending"); err != nil {
		return "", err
	}
	return "", &fakeRefusal{reason: "not a workspace", detail: "the workspace is at " + dir + ".pending"}
}
