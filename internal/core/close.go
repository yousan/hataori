package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// What asked for a Close or a Remove, as the state and removal_refused
// events say.
const (
	ByCommand   = "command"    // a call of the control API, such as hataori close or hataori remove
	ByClosedAge = "closed age" // the removal pass, for a thread closed longer than the configured age
)

// KindRemovalRefused is the event of a Remove that was refused.
const KindRemovalRefused control.EventKind = "removal_refused"

// Reasons of a refused removal that the core gives, besides the Remover's.
const (
	NotClosed    = "thread not closed"                  // the thread is not closed
	NoRemover    = "the instance removes no workspaces" // the core has no Remover
	AgentRunning = "agent not stopped"                  // the closed thread's agent could not be stopped
	// MovedNotRemoved starts the reason of a refusal after which the
	// workspace is no longer at its place: the remover moved it into trash
	// and could not finish. The detail says where it is.
	MovedNotRemoved = "workspace moved into trash, removal not finished"
)

// threadLock is the key that serializes Close and Remove of one thread.
func threadLock(id control.ThreadID) string { return fmt.Sprintf("thread:%d", id) }

func (a *api) CloseThread(ctx context.Context, id control.ThreadID) error {
	if _, err := a.actThread(ctx, control.OpClose, "CloseThread", id); err != nil {
		return err
	}
	defer a.c.locks.lock(threadLock(id))()
	sid, state, err := a.c.current(ctx, a.c.Store.DB, id)
	if err != nil {
		return err
	}
	// The runner applies the Close, so that nothing the agent reports after
	// it is read. Which states may be closed is the machine's to decide, in
	// the same transaction as the change: a thread that is running, waiting
	// or starting, or started a turn since it was read here, is refused. A
	// closed thread is not closed again, but its agent is stopped if it
	// still runs, as after a stop that failed.
	if err := a.c.Runner.Shut(ctx, sid, ByCommand); err != nil {
		return fmt.Errorf("core: close %v (%s): %w", id, state, err)
	}
	if state != control.Closed {
		a.c.Log.Info("thread closed", "thread", int64(id), "principal", string(a.caller.Principal))
	}
	return nil
}

func (a *api) RemoveThread(ctx context.Context, id control.ThreadID) error {
	if _, err := a.actThread(ctx, control.OpRemove, "RemoveThread", id); err != nil {
		return err
	}
	return a.c.remove(ctx, id, a.caller.Principal, ByCommand)
}

// remove removes the workspace of the closed thread id through the
// Remover, and marks the thread removed. Every outcome writes one event:
// the state change to removed, or removal_refused with the reason.
func (c *Core) remove(ctx context.Context, id control.ThreadID, principal control.Principal, trigger string) error {
	defer c.locks.lock(threadLock(id))()
	sid, state, err := c.current(ctx, c.Store.DB, id)
	if err != nil {
		return err
	}
	if state != control.Closed {
		c.refuseRemoval(ctx, id, sid, principal, trigger, NotClosed, "", "", string(state))
		return fmt.Errorf("core: remove %v: the thread is %s, not closed: %w", id, state, session.ErrRefused)
	}
	// The directory of a session a person started may hold the only copy of
	// their work, and hataori did not make it: it is never touched. Only
	// hataori's own of the thread goes.
	foreign, err := c.foreignWorkspace(ctx, id)
	if err != nil {
		c.refuseRemoval(ctx, id, sid, principal, trigger, NotHataoris, "", "", err.Error())
		return fmt.Errorf("core: remove %v: %w: %w", id, ErrRemovalRefused, err)
	}
	if foreign {
		return c.removeTakenIn(ctx, id, sid, principal, trigger)
	}
	if c.Remover == nil {
		c.refuseRemoval(ctx, id, sid, principal, trigger, NoRemover, "", "", "")
		return fmt.Errorf("core: remove %v: %w: %s", id, ErrRemovalRefused, NoRemover)
	}
	// The proof looks for a process using the directory; this makes sure
	// first that the closed thread's own agent has ended, as when closing it
	// could not stop it, or a daemon restart is still stopping it.
	if err := c.Runner.Shut(ctx, sid, trigger); err != nil {
		c.refuseRemoval(ctx, id, sid, principal, trigger, AgentRunning, "", "", err.Error())
		return fmt.Errorf("core: remove %v: %w: %w", id, ErrRemovalRefused, err)
	}
	dir, err := c.workspaceDir(ctx, id)
	if err != nil {
		// Nothing is known to be lost or not: refused, as any step that
		// cannot be answered is.
		c.refuseRemoval(ctx, id, sid, principal, trigger, "no workspace recorded", "", "", err.Error())
		return fmt.Errorf("core: remove %v: %w: %w", id, ErrRemovalRefused, err)
	}
	_, before := os.Lstat(dir)
	trash, err := c.Remover.Remove(ctx, dir)
	if err != nil {
		reason, repo, kind, detail := "error", "", "", err.Error()
		var r RemovalRefusal
		if errors.As(err, &r) {
			reason, repo, kind, detail = r.RemovalRefused()
		}
		if _, after := os.Lstat(dir); before == nil && errors.Is(after, fs.ErrNotExist) {
			// The remover moved it and then could not finish, as when the
			// rename out of trash/pending- fails: it is in trash, and its
			// detail says where. The thread stays closed.
			c.Log.Error("removal refused after the workspace was moved", "thread", int64(id), "detail", detail)
			reason = MovedNotRemoved + ": " + reason
		}
		c.refuseRemoval(ctx, id, sid, principal, trigger, reason, repo, kind, detail)
		return fmt.Errorf("core: remove %v: %w: %w", id, ErrRemovalRefused, err)
	}
	// The workspace is in trash. The thread follows; if that fails, the
	// workspace is still in trash for the retention, and the log says where.
	// A caller that gave up meanwhile does not keep it from being recorded.
	if err := c.Machine.Apply(context.WithoutCancel(ctx), sid, session.Input{Kind: session.Remove, Reason: trigger}); err != nil {
		c.Log.Error("workspace moved to trash, but the thread was not marked removed", "thread", int64(id),
			"trash", trash, "err", err)
		return fmt.Errorf("core: remove %v: the workspace is in trash, but: %w", id, err)
	}
	c.Log.Info("workspace removed", "thread", int64(id), "trigger", trigger,
		"principal", string(principal), "trash", trash)
	return nil
}

// refuseRemoval writes the log line and the removal_refused event of a
// refused removal. A failure to write the event is logged.
func (c *Core) refuseRemoval(ctx context.Context, id control.ThreadID, sid int64, principal control.Principal,
	trigger, reason, repo, kind, detail string) {
	c.Log.Warn("removal refused", "thread", int64(id), "trigger", trigger,
		"principal", string(principal), "reason", reason, "repo", repo, "detail", detail)
	d := map[string]any{"session": sid, "trigger": trigger, "reason": reason}
	if principal != "" {
		d["principal"] = string(principal)
	}
	if repo != "" {
		d["repo"] = repo
		d["kind"] = kind
	}
	if detail != "" {
		d["detail"] = detail
	}
	data, _ := json.Marshal(d)
	ctx = context.WithoutCancel(ctx) // the refusal is recorded even when the caller gave up
	err := c.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Append(ctx, control.Event{Thread: id, Time: c.now(), Kind: KindRemovalRefused, Data: data})
		return err
	})
	if err != nil {
		c.Log.Error("recording a refused removal", "thread", int64(id), "err", err)
	}
}

// RemoveClosed is the automatic removal pass: it removes the workspace of
// every thread closed for longer than age, through the same proof as
// Remove. A thread whose removal the pass was refused is tried again only
// once age has passed since, so that a workspace that holds work writes
// one refusal per age, not one per pass. It returns how many it removed.
func (c *Core) RemoveClosed(ctx context.Context, age time.Duration) (int, error) {
	if age <= 0 {
		return 0, errors.New("core: removal pass: no age set")
	}
	since := c.now().Add(-age).UnixMilli()
	// A thread started confined has a directory of its own, which nothing
	// removes, and one taken in a person's directory: the pass does not try.
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT t.id FROM threads t WHERE t.state = ? AND t.updated_at <= ? AND t.confined = 0 AND t.foreign_workspace = 0
		AND NOT EXISTS (SELECT 1 FROM events e WHERE e.thread_id = t.id AND e.kind = ? AND e.time > ?
			AND json_extract(CAST(e.data AS TEXT), '$.trigger') = ?)
		ORDER BY t.id`, string(control.Closed), since, string(KindRemovalRefused), since, ByClosedAge)
	if err != nil {
		return 0, fmt.Errorf("core: removal pass: %w", err)
	}
	var ids []control.ThreadID
	for rows.Next() {
		var id control.ThreadID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("core: removal pass: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("core: removal pass: %w", err)
	}
	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if err := c.remove(ctx, id, "", ByClosedAge); err == nil {
			n++
		}
	}
	return n, nil
}
