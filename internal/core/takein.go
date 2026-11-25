package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/window"
)

// Windows has the last report of each window a person started.
type Windows interface {
	// Report returns the report kept for the window that name names, as
	// control.ReportedSession.Window does, while its agent's process runs.
	Report(name string) (window.Report, bool)
}

// TakeInRunner is the part of a Runner that takes in windows people
// started (design, section 4.8). A Runner without it has TakeIn and Release
// refused.
type TakeInRunner interface {
	// CheckTakeIn asks the window's holder whether rep's window may be
	// taken in.
	CheckTakeIn(ctx context.Context, rep window.Report) error
	// TakenIn reports whether rep's window is taken in now.
	TakenIn(rep window.Report) (bool, error)
	// TakeIn follows the new session l of rep's window: no agent is
	// launched, and the session becomes idle once its codec reports it
	// ready.
	TakeIn(ctx context.Context, l Launch, rep window.Report) error
	// Release puts a session taken in asleep and lets its window go.
	Release(ctx context.Context, sessionID int64) error
	// RemoveState removes a session's state directory, which no host runs
	// in: what hataori keeps of it, never its workspace.
	RemoveState(ctx context.Context, sessionID int64) error
}

// NotHataoris is the reason a removal of a thread taken in is refused when
// it cannot be told whether the thread was taken in; one that was is
// removed without its workspace (removeTakenIn).
const NotHataoris = "the workspace is not hataori's"

// removeTakenIn removes a closed thread taken in from a person's window:
// its sessions' state directories, and the thread is marked removed. Its
// workspace, the directory of the session the person started, is left as
// it is: hataori did not make it (design, section 4.8).
func (c *Core) removeTakenIn(ctx context.Context, id control.ThreadID, sid int64, principal control.Principal, trigger string) error {
	tr, ok := c.Runner.(TakeInRunner)
	if !ok {
		c.refuseRemoval(ctx, id, sid, principal, trigger, NotHataoris, "", "", "nothing removes a session's state")
		return fmt.Errorf("core: remove %v: %w: %s", id, ErrRemovalRefused, NotHataoris)
	}
	// Its relay host, if one still runs, goes first.
	if err := c.Runner.Shut(ctx, sid, trigger); err != nil {
		c.refuseRemoval(ctx, id, sid, principal, trigger, AgentRunning, "", "", err.Error())
		return fmt.Errorf("core: remove %v: %w: %w", id, ErrRemovalRefused, err)
	}
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT id FROM sessions WHERE thread_id = ? ORDER BY id`, int64(id))
	if err != nil {
		return fmt.Errorf("core: remove %v: %w", id, err)
	}
	var sessions []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return fmt.Errorf("core: remove %v: %w", id, err)
		}
		sessions = append(sessions, s)
	}
	rows.Close()
	for _, s := range sessions {
		if err := tr.RemoveState(ctx, s); err != nil {
			c.refuseRemoval(ctx, id, sid, principal, trigger, "error", "", "", err.Error())
			return fmt.Errorf("core: remove %v: %w: %w", id, ErrRemovalRefused, err)
		}
	}
	if err := c.Machine.Apply(context.WithoutCancel(ctx), sid, session.Input{Kind: session.Remove, Reason: trigger}); err != nil {
		return fmt.Errorf("core: remove %v: %w", id, err)
	}
	c.Log.Info("thread taken in removed; its directory is the person's and stays", "thread", int64(id),
		"trigger", trigger, "principal", string(principal))
	return nil
}

// takeInLog logs a refused take-in and returns err.
func (a *api) takeInRefused(name, scope, window, reason string, err error) error {
	a.c.Log.Warn("take-in refused", "op", name, "scope", scope, "window", window, "reason", reason,
		"principal", string(a.caller.Principal))
	return err
}

func (a *api) TakeIn(ctx context.Context, req control.TakeInRequest) (control.ThreadID, error) {
	const name = "TakeIn"
	if err := a.check(control.OpTakeIn, name, req.Scope); err != nil {
		return 0, err
	}
	sc, ok := a.c.Scopes[req.Scope]
	if !ok || !sc.Enabled {
		return 0, a.takeInRefused(name, req.Scope, req.Window, "scope is not enabled",
			fmt.Errorf("core: take in to %q: %w", req.Scope, ErrDisabled))
	}
	tr, ok := a.c.Runner.(TakeInRunner)
	if !ok || a.c.Windows == nil {
		return 0, a.takeInRefused(name, req.Scope, req.Window, "nothing takes windows' reports",
			fmt.Errorf("core: take in %q: no window reports are taken (host_terminals): %w", req.Window, ErrNotFound))
	}
	// The key is per scope, as Start's is; takes with one key run one at a
	// time, so the window is taken in once.
	inboxKey := "takein:" + req.Scope + "\x00" + req.Key
	defer a.c.locks.lock(inboxKey)()
	if id, ok, err := a.c.started(ctx, inboxKey); err != nil || ok {
		return id, err
	}
	rep, ok := a.c.Windows.Report(req.Window)
	if !ok {
		return 0, a.takeInRefused(name, req.Scope, req.Window, "no report",
			fmt.Errorf("core: take in %q: no session reported from it: %w", req.Window, ErrNotFound))
	}
	if !rep.Interactive {
		return 0, a.takeInRefused(name, req.Scope, req.Window, "not interactive",
			fmt.Errorf("core: take in %q: the session is not interactive: %w", req.Window, session.ErrRefused))
	}
	if _, known := a.c.Agents[rep.Agent]; a.c.Agents != nil && !known || a.c.Agents == nil && rep.Agent != a.c.Agent {
		return 0, a.takeInRefused(name, req.Scope, req.Window, "unknown agent",
			fmt.Errorf("core: take in %q: agent %q: %w", req.Window, rep.Agent, control.ErrUnknownAgent))
	}
	// One window at a time: windows are taken in under one lock, so two
	// keys cannot take the same window.
	defer a.c.locks.lock("takein-window")()
	if taken, err := tr.TakenIn(rep); err != nil || taken {
		if err == nil {
			err = fmt.Errorf("core: take in %q: it is taken in already: %w", req.Window, session.ErrRefused)
		}
		return 0, a.takeInRefused(name, req.Scope, req.Window, "taken in already", err)
	}
	if err := tr.CheckTakeIn(ctx, rep); err != nil {
		return 0, a.takeInRefused(name, req.Scope, req.Window, "the window's check failed",
			fmt.Errorf("core: take in %q: %w: %w", req.Window, session.ErrRefused, err))
	}
	title := filepath.Base(rep.Dir)
	owner, ok := a.c.Owners[sc.Owner]
	if !ok {
		return 0, fmt.Errorf("core: take in to %q: no owner %q", req.Scope, sc.Owner)
	}
	link, err := owner.NewThread(ctx, req.Scope, req.Key, title)
	if err != nil {
		return 0, fmt.Errorf("core: take in to %q: owner: %w", req.Scope, err)
	}
	if link.Client != sc.Owner || link.Channel != req.Scope || link.Thread == "" {
		return 0, fmt.Errorf("core: take in to %q: owner %q returned a link elsewhere: %s %s %s", req.Scope, sc.Owner, link.Client, link.Channel, link.Thread)
	}
	var (
		id        control.ThreadID
		sessionID int64
	)
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `INSERT INTO threads (scope, key, title, repo, workspace, started_by, state, created_at, updated_at, foreign_workspace)
			VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, 1)`,
			req.Scope, link.Thread, title, rep.Dir, string(a.caller.Principal), string(control.Creating), now, now)
		if err != nil {
			return fmt.Errorf("core: create thread: %w", err)
		}
		thread, _ := res.LastInsertId()
		id = control.ThreadID(thread)
		ref, _ := json.Marshal(id)
		if ok, err := tx.Accept(ctx, inboxKey, ref); err != nil || !ok {
			if err == nil {
				err = errors.New("core: take in: the key was accepted before")
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO links (thread_id, client, channel, thread, made_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			thread, link.Client, link.Channel, link.Thread, string(a.caller.Principal), now); err != nil {
			return fmt.Errorf("core: create thread: link %s %s %s: %w", link.Client, link.Channel, link.Thread, err)
		}
		// launch 0: nothing is launched for it, now or after a restart.
		res, err = tx.ExecContext(ctx,
			`INSERT INTO sessions (thread_id, agent, agent_session_id, state, model, launch, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
			thread, rep.Agent, rep.Session, string(control.Creating), rep.Model, now, now)
		if err != nil {
			return fmt.Errorf("core: create session: %w", err)
		}
		sessionID, _ = res.LastInsertId()
		data, _ := json.Marshal(map[string]any{"thread": thread, "session": sessionID, "repo": "",
			"principal": string(a.caller.Principal), "model": rep.Model, "effort": "", "kind": string(control.KindOrdinary)})
		_, err = tx.Append(ctx, control.Event{Thread: id, Scope: req.Scope, Time: a.c.now(), Kind: "thread_created", Data: data})
		return err
	})
	if err != nil {
		return 0, err
	}
	a.c.Log.Info("window taken in", "thread", int64(id), "scope", req.Scope, "window", req.Window,
		"principal", string(a.caller.Principal))
	if err := tr.TakeIn(ctx, Launch{Session: sessionID, Thread: id, Agent: rep.Agent, Dir: rep.Dir, Model: rep.Model,
		Principal: a.caller.Principal, From: a.caller.From, Key: req.Key}, rep); err != nil {
		if ferr := a.c.Machine.Apply(ctx, sessionID, session.Input{Kind: session.Exit, Reason: "take in: " + err.Error()}); ferr != nil {
			a.c.Log.Error("recording a failed take-in", "session", sessionID, "err", ferr)
		}
		return id, fmt.Errorf("core: take in: %w", err)
	}
	return id, nil
}

func (a *api) Release(ctx context.Context, id control.ThreadID) error {
	const name = "Release"
	scope, err := a.threadScope(ctx, name, id)
	if err != nil {
		return err
	}
	if err := a.check(control.OpTakeIn, name, scope); err != nil {
		return err
	}
	tr, ok := a.c.Runner.(TakeInRunner)
	if !ok {
		return fmt.Errorf("core: release %v: nothing takes windows in: %w", id, session.ErrRefused)
	}
	defer a.c.locks.lock(threadLock(id))()
	sid, _, err := a.c.current(ctx, a.c.Store.DB, id)
	if err != nil {
		return err
	}
	if err := tr.Release(ctx, sid); err != nil {
		return a.takeInRefused(name, scope, "", err.Error(), fmt.Errorf("core: release %v: %w: %w", id, session.ErrRefused, err))
	}
	a.c.Log.Info("window let go", "thread", int64(id), "principal", string(a.caller.Principal))
	return nil
}

// foreignWorkspace reports whether thread id was taken in from a window: its
// workspace is not hataori's.
func (c *Core) foreignWorkspace(ctx context.Context, id control.ThreadID) (bool, error) {
	var foreign bool
	err := c.Store.DB.QueryRowContext(ctx, `SELECT foreign_workspace FROM threads WHERE id = ?`, int64(id)).Scan(&foreign)
	if err != nil {
		return false, fmt.Errorf("core: thread %d: %w", id, err)
	}
	return foreign, nil
}
