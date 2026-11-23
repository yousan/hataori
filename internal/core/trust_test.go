package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

const trustRepo = "https://example.com/r.git"

// blocked starts a thread whose first session a start-up dialog blocks,
// naming files.
func (f *fixture) blocked(t *testing.T, key string, files ...string) (control.ThreadID, int64) {
	t.Helper()
	ctx := context.Background()
	id, err := f.owner.Start(ctx, control.StartRequest{Key: key, Scope: "s", Thread: key, Repo: trustRepo, Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Trust: files,
		Text: "the dialog names " + strings.Join(files, ", ")})
	return id, sid
}

// The owner trusts the files that blocked a start: they are recorded for
// the scope and the repository, and the thread starts again in a new
// session with the same first input (#590).
func TestTheOwnerTrustsTheFilesAndTheThreadStartsAgain(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.blocked(t, "t1", ".mcp.json", ".claude/settings.json")
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpTrust})
	if err := owner.Trust(ctx, id, sid, []string{".mcp.json", ".claude/settings.json"}); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.Calls(); got != "start go; start go" {
		t.Fatalf("runner calls %q, want the first input started twice", got)
	}
	nsid, state, err := f.core.current(ctx, f.core.Store.DB, id)
	if err != nil || nsid == sid || state != control.Creating {
		t.Fatalf("current session %d %s %v; want a new one, creating", nsid, state, err)
	}
	files, err := f.core.TrustedFiles(ctx, "s", trustRepo)
	if err != nil || !slices.Equal(files, []string{".claude/settings.json", ".mcp.json"}) {
		t.Fatalf("trusted %q %v", files, err)
	}
	if other, _ := f.core.TrustedFiles(ctx, "s", "https://example.com/other.git"); len(other) != 0 {
		t.Fatalf("another repository trusts %q", other)
	}
	evs, err := f.owner.ThreadEvents(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if e := evs[len(evs)-1]; e.Kind != "session_replaced" || !strings.Contains(string(e.Data), `"reason":"trusted"`) {
		t.Fatalf("last event %s %s", e.Kind, e.Data)
	}
	ts, err := owner.Trusts(ctx)
	if err != nil || len(ts) != 2 || ts[0].By != "owner" || ts[0].Repo != trustRepo {
		t.Fatalf("Trusts: %+v %v", ts, err)
	}

	// Trusting again: the session is no longer blocked.
	if err := owner.Trust(ctx, id, sid, []string{".mcp.json", ".claude/settings.json"}); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("a second Trust: %v, want refused", err)
	}

	// Withdrawn, the file is no longer trusted.
	for _, tf := range ts {
		if err := owner.Untrust(ctx, tf.ID); err != nil {
			t.Fatal(err)
		}
	}
	if files, _ := f.core.TrustedFiles(ctx, "s", trustRepo); len(files) != 0 {
		t.Fatalf("after Untrust: %q", files)
	}
	if err := owner.Untrust(ctx, ts[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Untrust again: %v", err)
	}
}

// Anyone but the owner is refused and nothing is recorded or started; so
// are files other than those the dialog named.
func TestTrustRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.blocked(t, "t1", ".mcp.json")
	calls := f.runner.Calls()
	other := f.core.As(control.Caller{Principal: "bob", Scopes: []string{"s"}, May: all | control.OpTrust})
	guest := f.core.As(control.Caller{From: "chat:nobody", Scopes: []string{"s"}, May: all | control.OpTrust})
	noOp := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	for name, a := range map[string]control.API{"another person": other, "a guest": guest, "no OpTrust": noOp} {
		f.log.Reset()
		if err := a.Trust(ctx, id, sid, []string{".mcp.json"}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v, want forbidden", name, err)
		}
		if !strings.Contains(f.log.String(), "control call refused") {
			t.Errorf("%s: no log line: %s", name, f.log.String())
		}
	}
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpTrust})
	for _, files := range [][]string{{".mcp.json", ".claude/settings.json"}, {".claude/settings.json"}, nil} {
		if err := owner.Trust(ctx, id, sid, files); !errors.Is(err, session.ErrRefused) {
			t.Errorf("files %q: %v, want refused", files, err)
		}
	}
	if n := count(t, f.core.Store, "trusted_files"); n != 0 {
		t.Fatalf("%d files recorded", n)
	}
	if cur, _, _ := f.core.current(ctx, f.core.Store.DB, id); cur != sid || f.runner.Calls() != calls {
		t.Fatal("a refused Trust started a session")
	}

	// A session that failed for another reason is not one to trust.
	id2, err := f.owner.Start(ctx, control.StartRequest{Key: "t2", Scope: "s", Thread: "t2", Repo: trustRepo, Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	sid2, _, _ := f.core.current(ctx, f.core.Store.DB, id2)
	if err := f.core.Machine.Apply(ctx, sid2, session.Input{Kind: session.StartTimeout, Detail: "a screen"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Trust(ctx, id2, sid2, []string{".mcp.json"}); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("a start timeout: %v, want refused", err)
	}
}

// A Trust that names another session than the blocked one, such as an old
// button's, is refused.
func TestTrustNamesTheBlockedSession(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.blocked(t, "t1", ".mcp.json")
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpTrust})
	if err := owner.Trust(ctx, id, sid-1, []string{".mcp.json"}); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("another session: %v, want refused", err)
	}
	if n := count(t, f.core.Store, "trusted_files"); n != 0 {
		t.Fatalf("%d files recorded", n)
	}
}

// earlierRunner is the fake runner that tells a thread's earlier
// conversation and keeps each launch.
type earlierRunner struct {
	*fakeRunner
	launches []Launch
}

func (r *earlierRunner) Start(ctx context.Context, l Launch) error {
	r.launches = append(r.launches, l)
	return r.fakeRunner.Start(ctx, l)
}

func (r *earlierRunner) Earlier(context.Context, interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, control.ThreadID) (string, error) {
	return "user: the earlier question\nagent: the earlier answer\n", nil
}

// A woken session that a dialog blocked starts again with the thread's
// earlier conversation before its input, which its agent would have
// resumed; a first start gets none. Either keeps the way its input came
// (via).
func TestTrustAWokenSessionKeepsTheConversation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	er := &earlierRunner{fakeRunner: f.runner}
	f.core.Runner = er
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpTrust})
	for _, wake := range []bool{false, true} {
		id, sid := f.blocked(t, fmt.Sprint("t-", wake), ".mcp.json")
		if wake {
			if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET first_input = json_set(first_input, '$.wake', json('true')) WHERE id = ?`, sid); err != nil {
				t.Fatal(err)
			}
		}
		// The input came through a way of the daemon's own, as an approved
		// proposal's does: the new session's input names it too.
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET first_input = json_set(first_input, '$.via', 'proposal') WHERE id = ?`, sid); err != nil {
			t.Fatal(err)
		}
		if err := owner.Trust(ctx, id, sid, []string{".mcp.json"}); err != nil {
			t.Fatal(err)
		}
		l := er.launches[len(er.launches)-1]
		if got := l.Context != ""; got != wake || l.Input.Text != "go" || l.Input.Via != "proposal" {
			t.Fatalf("wake %v: launch context %q, input %+v", wake, l.Context, l.Input)
		}
	}
}

// A person allowed in the scope starts a blocked thread without its
// repository's settings: the thread is marked for every later launch, and
// a new session starts with the same first input. A guest is refused and
// nothing changes; so is a session that is not the blocked one (#590).
func TestStartWithoutSettings(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	f.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:o"}}, {Name: "ada", IDs: []string{"chat:ada"}}},
		"owner", map[string][]string{"s": {"ada"}})
	id, sid := f.blocked(t, "t1", ".claude/settings.json")
	marked := func() bool {
		var n bool
		f.core.Store.DB.QueryRow(`SELECT no_project_settings FROM threads WHERE id = ?`, int64(id)).Scan(&n)
		return n
	}
	guest := f.core.As(control.Caller{From: "chat:nobody", Scopes: []string{"s"}, May: all})
	if err := guest.StartWithoutSettings(ctx, id, sid); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a guest: %v", err)
	}
	// A caller whose starts are proposals, such as a supervisor under the
	// autonomy propose: it starts nothing itself.
	proposer := f.core.As(control.Caller{From: "chat:ada", Scopes: []string{"s"}, May: all, Propose: true})
	if err := proposer.StartWithoutSettings(ctx, id, sid); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a caller whose starts are proposals: %v", err)
	}
	ada := f.core.As(control.Caller{From: "chat:ada", Scopes: []string{"s"}, May: all})
	if err := ada.StartWithoutSettings(ctx, id, sid+1); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("another session: %v", err)
	}
	if marked() || f.runner.Calls() != "start go" {
		t.Fatal("a refused call changed something")
	}
	if err := ada.StartWithoutSettings(ctx, id, sid); err != nil {
		t.Fatal(err)
	}
	if !marked() || f.runner.Calls() != "start go; start go" {
		t.Fatalf("marked %v, runner %q", marked(), f.runner.Calls())
	}
	evs, _ := f.owner.ThreadEvents(ctx, id, 0, 100)
	if e := evs[len(evs)-1]; e.Kind != "session_replaced" || !strings.Contains(string(e.Data), `"reason":"without project settings"`) {
		t.Fatalf("last event %s %s", e.Kind, e.Data)
	}
	if n := count(t, f.core.Store, "trusted_files"); n != 0 {
		t.Fatalf("%d files trusted", n)
	}
}
