package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// KindReopened is the event of a reopen, after its state event.
const KindReopened control.EventKind = "reopened"

func (a *api) ReopenThread(ctx context.Context, id control.ThreadID) (control.Reopened, error) {
	const name = "ReopenThread"
	scope, err := a.threadScope(ctx, name, id)
	if err != nil {
		return control.Reopened{}, err
	}
	if err := a.check(control.OpReopen, name, scope); err != nil {
		return control.Reopened{}, err
	}
	// The owner only: on the control socket, and for a press of the
	// Reopen button in chat, whoever else may act in the scope.
	if a.c.People == nil || a.caller.Principal == "" || string(a.caller.Principal) != a.c.People.Owner() {
		return control.Reopened{}, a.refused(name, scope, "only the owner may reopen a thread")
	}
	if err := a.left(name, scope); err != nil {
		return control.Reopened{}, err
	}
	if err := a.c.requireCheckedMachine(ctx, id); err != nil {
		return control.Reopened{}, err
	}
	return a.c.reopen(ctx, id, scope, a.caller.Principal)
}

// reopenRow is what a reopen reads of a thread and its current session.
type reopenRow struct {
	session  int64
	state    control.State // the session's
	dir      string
	repo     string
	trash    string
	placed   string // threads.reopen_placed
	agentID  string
	started  bool // the session's agent was launched at least once
	confined bool
	kind     control.ThreadKind
	foreign  bool
}

func (c *Core) reopenRow(ctx context.Context, q querier, id control.ThreadID) (reopenRow, error) {
	var r reopenRow
	sid, state, err := c.current(ctx, q, id)
	if err != nil {
		return r, err
	}
	r.session, r.state = sid, state
	err = q.QueryRowContext(ctx, `SELECT t.workspace, t.repo, t.trash, t.reopen_placed, t.confined, t.kind, t.foreign_workspace, s.agent_session_id, s.spec != ''
		FROM threads t JOIN sessions s ON s.id = ? WHERE t.id = ?`, sid, int64(id)).
		Scan(&r.dir, &r.repo, &r.trash, &r.placed, &r.confined, &r.kind, &r.foreign, &r.agentID, &r.started)
	if err != nil {
		return r, fmt.Errorf("core: thread %d: %w", id, err)
	}
	return r, nil
}

// openSupervisor returns another supervisor's own thread of scope that is
// neither closed nor removed; 0 when there is none.
func openSupervisor(ctx context.Context, q querier, scope string, id control.ThreadID) (control.ThreadID, error) {
	var other int64
	err := q.QueryRowContext(ctx, `SELECT id FROM threads WHERE scope = ? AND kind = ? AND id != ? AND state NOT IN (?, ?)
		ORDER BY id DESC LIMIT 1`, scope, string(control.KindSupervisor), int64(id), string(control.Closed), string(control.Removed)).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("core: the supervisor's own threads of %q: %w", scope, err)
	}
	return control.ThreadID(other), nil
}

// reopen takes the closed or removed thread id back to asleep, first
// putting its directory in place when it is not (design, section 5.1). A
// refusal changes nothing and writes one log line.
func (c *Core) reopen(ctx context.Context, id control.ThreadID, scope string, principal control.Principal) (control.Reopened, error) {
	defer c.locks.lock(threadLock(id))()
	machine, err := c.workspaceMachine(ctx, id)
	if err != nil {
		return control.Reopened{}, err
	}
	if machine != store.LocalMachine {
		return c.reopenOnWorker(ctx, id, principal)
	}
	// Taken last, after the thread's, as the start of a supervisor's own
	// thread takes it after its key's: none of the scope starts while this
	// one is looked at and reopened.
	defer c.locks.lock(supervisorLock(scope))()
	refuse := func(reason string, attrs ...any) (control.Reopened, error) {
		c.Log.Warn("reopen refused", append([]any{"thread", int64(id), "principal", string(principal), "reason", reason}, attrs...)...)
		return control.Reopened{}, fmt.Errorf("core: reopen %v: %w", id, &control.ReopenRefused{Reason: reason})
	}
	r, err := c.reopenRow(ctx, c.Store.DB, id)
	if err != nil {
		return control.Reopened{}, err
	}
	if r.state != control.Closed && r.state != control.Removed {
		return refuse(fmt.Sprintf("the thread is %s, not closed", r.state))
	}
	if r.foreign && r.state == control.Removed {
		// Its directory was never hataori's, and its session's state went
		// with the removal.
		return refuse("it was taken in from a person's window and removed: take the window in again")
	}
	switch r.kind {
	case control.KindLookup:
		return refuse("it answered its one question: a look-up's thread is never reopened")
	case control.KindSupervisor:
		// One of a scope's supervisor's own threads is open at a time
		// (design, section 7.1).
		other, err := openSupervisor(ctx, c.Store.DB, scope, id)
		if err != nil {
			return control.Reopened{}, err
		}
		if other != 0 {
			return refuse(fmt.Sprintf("thread %d, another supervisor thread of this scope, is open: only one is open at a time", other),
				"open", int64(other))
		}
	}
	if r.dir == "" || !r.started {
		// Nothing to go back to: the next message could not wake it.
		return refuse("its agent was never started")
	}
	out := control.Reopened{Workspace: control.ReopenKept, Conversation: control.ReopenResume}
	st, err := os.Lstat(r.dir)
	if err == nil && !st.IsDir() && !r.foreign {
		return refuse("the place of its directory is taken by something that is no directory: nothing was overwritten", "dir", r.dir)
	}
	if ours, how := c.placedBefore(ctx, id, r); err == nil && ours {
		// An earlier reopen put it there and stopped before it recorded
		// the reopen, as when the daemon died: it is the thread's.
		out.Workspace = how
	} else if out.Workspace, err = c.reopenDir(ctx, id, r, err); err != nil {
		var refusal *control.ReopenRefused
		if errors.As(err, &refusal) {
			return refuse(refusal.Reason, "dir", r.dir)
		}
		return control.Reopened{}, err
	}
	// A directory made again holds no work of the conversation, which
	// would resume in it as if it did; and a session that never recorded
	// its conversation has none to resume.
	if out.Workspace == control.ReopenRemade || r.agentID == "" {
		out.Conversation = control.ReopenNew
	}
	// From here on the directory may be in place: a caller that gives up
	// does not keep the reopen from being recorded.
	ctx = context.WithoutCancel(ctx)
	err = c.Store.Update(ctx, func(tx *store.Tx) error {
		now, err := c.reopenRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if now.session != r.session || now.state != r.state {
			return &control.ReopenRefused{Reason: "the thread changed meanwhile"}
		}
		if r.kind == control.KindSupervisor {
			if other, err := openSupervisor(ctx, tx, scope, id); err != nil || other != 0 {
				if err != nil {
					return err
				}
				return &control.ReopenRefused{Reason: fmt.Sprintf("thread %d, another supervisor thread of this scope, is open: only one is open at a time", other)}
			}
		}
		if out.Conversation == control.ReopenNew {
			// The next wake starts a new conversation in the directory, with
			// the thread's earlier exchange as context (the runner's resume).
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id = '' WHERE id = ?`, r.session); err != nil {
				return err
			}
		}
		if err := c.Machine.ApplyTx(ctx, tx, r.session, session.Input{Kind: session.Reopen, Principal: principal, Reason: session.ReasonReopen}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET trash = '', reopen_placed = '' WHERE id = ?`, int64(id)); err != nil {
			return err
		}
		d := map[string]any{"session": r.session, "workspace": out.Workspace, "conversation": out.Conversation}
		if principal != "" {
			d["principal"] = string(principal)
		}
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: id, Time: c.now(), Kind: KindReopened, Data: data})
		return err
	})
	if err != nil && out.Workspace != control.ReopenKept {
		// The directory is back at its place, and the thread was not
		// marked reopened: the next reopen knows the directory for the one
		// it put there (placedBefore).
		c.Log.Error("reopen: the directory was put back, but the thread was not reopened", "thread", int64(id), "dir", r.dir, "err", err)
	}
	var refusal *control.ReopenRefused
	if errors.As(err, &refusal) {
		return refuse(refusal.Reason)
	}
	if err != nil {
		return control.Reopened{}, fmt.Errorf("core: reopen %v: %w", id, err)
	}
	c.Log.Info("thread reopened", "thread", int64(id), "principal", string(principal), "workspace", out.Workspace,
		"conversation", out.Conversation)
	return out, nil
}

// placeAgain puts the directory of thread id in place, where it is not:
// taken back from trash while it is there, else made again (a clone of
// the thread's repository, or an empty directory for a thread started
// confined). Nothing that is at its place is touched. A refusal is a
// *control.ReopenRefused.
func (c *Core) placeAgain(ctx context.Context, id control.ThreadID, r reopenRow) (string, error) {
	// A removal that a crash stopped before it recorded its entry, or one
	// of before entries were recorded, left it in trash all the same.
	if r.trash == "" && c.Restorer != nil && !r.confined && !r.foreign {
		entry, err := c.Restorer.Trashed(id, r.dir)
		if err != nil {
			return "", &control.ReopenRefused{Reason: "which entry in trash is its directory cannot be told: " + err.Error()}
		}
		r.trash = entry
	}
	if r.trash != "" && c.Restorer != nil {
		err := c.Restorer.Restore(ctx, r.trash, r.dir, c.recordPlaced(ctx, id, control.ReopenRestored))
		if err == nil {
			return control.ReopenRestored, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", &control.ReopenRefused{Reason: "its directory could not be taken back from trash: " + err.Error()}
		}
		// The trash pass deleted it: made again below.
	}
	switch {
	case r.confined:
		if _, err := c.Workspaces.Confined(ctx, id, r.dir); err != nil {
			return "", &control.ReopenRefused{Reason: "its directory could not be made again: " + err.Error()}
		}
	case r.foreign:
		// A person's own directory is not hataori's to make.
		return "", &control.ReopenRefused{Reason: "its directory, a person's own, is gone"}
	case r.repo == "":
		return "", &control.ReopenRefused{Reason: "its directory is gone, and it has no repository to clone again"}
	case c.Restorer == nil:
		return "", &control.ReopenRefused{Reason: "its directory is gone, and this instance makes no workspace again"}
	default:
		if err := c.Restorer.Remake(ctx, id, r.repo, r.dir, c.recordPlaced(ctx, id, control.ReopenRemade)); err != nil {
			return "", &control.ReopenRefused{Reason: "its directory could not be made again: " + err.Error()}
		}
	}
	return control.ReopenRemade, nil
}

// reopenDir sees to the directory of the thread of r, which os.Lstat
// answered with lerr: kept when it is there, else put in place again. A
// refusal is a *control.ReopenRefused.
func (c *Core) reopenDir(ctx context.Context, id control.ThreadID, r reopenRow, lerr error) (string, error) {
	switch {
	case lerr == nil && r.state == control.Removed:
		// Removed, its directory went to trash: whatever is at its place
		// now is not the thread's, and is left as it is.
		return "", &control.ReopenRefused{Reason: "the place of its directory is taken by something else: nothing was overwritten"}
	case lerr == nil:
		return control.ReopenKept, nil
	case !errors.Is(lerr, fs.ErrNotExist):
		return "", &control.ReopenRefused{Reason: "its directory cannot be read: " + lerr.Error()}
	}
	return c.placeAgain(ctx, id, r)
}

// placedBefore reports whether the directory at the thread's place is the
// one an earlier reopen of it put there, by the identity that reopen
// recorded (threads.reopen_placed), and what that reopen made of it.
func (c *Core) placedBefore(ctx context.Context, id control.ThreadID, r reopenRow) (bool, string) {
	if r.placed == "" {
		return false, ""
	}
	var p placedRecord
	if json.Unmarshal([]byte(r.placed), &p) != nil || p.Dir != r.dir {
		return false, ""
	}
	_, ok, err := c.Workspaces.Adopt(ctx, id, p.Placement)
	return err == nil && ok, p.Workspace
}

// placedRecord is threads.reopen_placed: the identity of the directory a
// reopen put in place, and whether it was restored or remade.
type placedRecord struct {
	Placement
	Workspace string `json:"workspace"`
}

// recordPlaced records, right before a reopen puts the thread's directory
// in place, what will identify it there (placedBefore).
func (c *Core) recordPlaced(ctx context.Context, id control.ThreadID, how string) func(Placement) error {
	return func(p Placement) error {
		b, _ := json.Marshal(placedRecord{p, how})
		done := context.WithoutCancel(ctx)
		return c.Store.Update(done, func(tx *store.Tx) error {
			_, err := tx.ExecContext(done, `UPDATE threads SET reopen_placed = ? WHERE id = ?`, string(b), int64(id))
			return err
		})
	}
}

// supervisorLock is the key that serializes the start and a reopen of a
// supervisor's own thread in one scope.
func supervisorLock(scope string) string { return "supervisor:" + scope }
