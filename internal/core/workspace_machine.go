package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// WorkerWorkspaces never exposes a worker path to a local filesystem API.
// Every effect uses the recorded machine/session/run and durable receipts.
type WorkerWorkspaces interface {
	RemoveRemoteWorkspace(context.Context, int64, int64) (string, error)
	ReopenRemoteWorkspace(context.Context, int64, int64) (string, string, error)
	AcknowledgeRemoteTrash(context.Context, int64, string) error
}

func (c *Core) workspaceMachine(ctx context.Context, id control.ThreadID) (string, error) {
	var machine string
	err := c.Store.DB.QueryRowContext(ctx, `SELECT coalesce(machine,'') FROM threads WHERE id=?`, int64(id)).Scan(&machine)
	return machine, err
}

func (c *Core) workspaceCycle(ctx context.Context, id control.ThreadID) (int64, error) {
	var cycle int64
	err := c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE thread_id=? AND kind='reopened'`, int64(id)).Scan(&cycle)
	return cycle, err
}

// removeOnWorker is called under the same thread lock as local removal. The
// resident reproves current ownership/work protection immediately before rename.
func (c *Core) removeOnWorker(ctx context.Context, id control.ThreadID, principal control.Principal, trigger string) error {
	sid, state, err := c.current(ctx, c.Store.DB, id)
	if err != nil {
		return err
	}
	refuse := func(reason string, cause error) error {
		c.refuseRemoval(ctx, id, sid, principal, trigger, reason, "", "", "")
		return fmt.Errorf("core: worker removal refused: %s: %w", reason, errors.Join(ErrRemovalRefused, cause))
	}
	if state != control.Closed {
		return refuse(NotClosed, session.ErrRefused)
	}
	worker, ok := c.Runner.(WorkerWorkspaces)
	if !ok {
		return refuse("owning machine has no workspace implementation", nil)
	}
	if err := c.Runner.Shut(ctx, sid, trigger); err != nil {
		return refuse(AgentRunning, err)
	}
	cycle, err := c.workspaceCycle(ctx, id)
	if err != nil {
		return err
	}
	trash, err := worker.RemoveRemoteWorkspace(ctx, sid, cycle)
	if err != nil {
		return refuse("worker-local proof or removal is unavailable", err)
	}
	done := context.WithoutCancel(ctx)
	if err := c.Store.Update(done, func(tx *store.Tx) error {
		current, state, err := c.current(done, tx, id)
		if err != nil {
			return err
		}
		if current != sid || state != control.Closed {
			return session.ErrRefused
		}
		if err := c.Machine.ApplyTx(done, tx, sid, session.Input{Kind: session.Remove, Reason: trigger}); err != nil {
			return err
		}
		_, err = tx.ExecContext(done, `UPDATE threads SET trash=? WHERE id=?`, trash, int64(id))
		return err
	}); err != nil {
		return err
	}
	// Failure keeps retention locked; a later recovery acknowledges the same
	// trash only after the durable main removal record is visible.
	if err := worker.AcknowledgeRemoteTrash(done, sid, trash); err != nil {
		c.Log.Warn("worker trash acknowledgment waits", "thread", id, "err", err)
	}
	return nil
}

func (c *Core) reopenOnWorker(ctx context.Context, id control.ThreadID, principal control.Principal) (control.Reopened, error) {
	r, err := c.reopenRow(ctx, c.Store.DB, id)
	if err != nil {
		return control.Reopened{}, err
	}
	refuse := func(reason string) (control.Reopened, error) {
		return control.Reopened{}, &control.ReopenRefused{Reason: reason}
	}
	if r.state != control.Closed && r.state != control.Removed {
		return refuse("the thread is not closed or removed")
	}
	if !r.started || r.dir == "" {
		return refuse("its agent was never started")
	}
	worker, ok := c.Runner.(WorkerWorkspaces)
	if !ok {
		return refuse("owning machine has no workspace implementation")
	}
	cycle, err := c.workspaceCycle(ctx, id)
	if err != nil {
		return control.Reopened{}, err
	}
	handle, how, err := worker.ReopenRemoteWorkspace(ctx, r.session, cycle)
	if err != nil {
		return refuse("workspace recovery on the recorded machine is unavailable: " + err.Error())
	}
	out := control.Reopened{Workspace: how, Conversation: control.ReopenResume}
	if how == control.ReopenRemade || r.agentID == "" {
		out.Conversation = control.ReopenNew
	}
	done := context.WithoutCancel(ctx)
	err = c.Store.Update(done, func(tx *store.Tx) error {
		current, state, err := c.current(done, tx, id)
		if err != nil {
			return err
		}
		if current != r.session || state != r.state {
			return session.ErrRefused
		}
		if out.Conversation == control.ReopenNew {
			if _, err := tx.ExecContext(done, `UPDATE sessions SET agent_session_id='' WHERE id=?`, r.session); err != nil {
				return err
			}
		}
		if err := c.Machine.ApplyTx(done, tx, r.session, session.Input{Kind: session.Reopen, Principal: principal, Reason: session.ReasonReopen}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(done, `UPDATE threads SET workspace=?,trash='',reopen_placed='' WHERE id=?`, handle, int64(id)); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"session": r.session, "workspace": how, "conversation": out.Conversation, "principal": principal})
		_, err = tx.Append(done, control.Event{Thread: id, Time: c.now(), Kind: KindReopened, Data: data})
		return err
	})
	return out, err
}

func (c *Core) acknowledgeWorkerTrash(ctx context.Context) error {
	worker, ok := c.Runner.(WorkerWorkspaces)
	if !ok {
		return nil
	}
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT s.id,t.trash FROM threads t JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id) WHERE t.machine!='local' AND t.state='removed' AND t.trash!=''`)
	if err != nil {
		return err
	}
	type trash struct {
		sid    int64
		handle string
	}
	var found []trash
	for rows.Next() {
		var x trash
		if err := rows.Scan(&x.sid, &x.handle); err != nil {
			rows.Close()
			return err
		}
		found = append(found, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range found {
		if err := worker.AcknowledgeRemoteTrash(ctx, x.sid, x.handle); err != nil && ctx.Err() == nil {
			c.Log.Warn("worker trash acknowledgment waits", "session", x.sid, "err", err)
		}
	}
	return nil
}
