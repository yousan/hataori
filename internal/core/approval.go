package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

func (a *api) AskInterrupt(ctx context.Context, id control.ThreadID, key, reason string) (control.RequestID, error) {
	scope, err := a.actThread(ctx, control.OpAsk, "AskInterrupt", id)
	if err != nil {
		return "", err
	}
	if a.caller.AsksIn == 0 {
		return "", a.refused("AskInterrupt", scope, "no approval thread")
	}
	var askScope string
	if err := a.c.Store.DB.QueryRowContext(ctx, `SELECT scope FROM threads WHERE id = ?`, a.caller.AsksIn).Scan(&askScope); err != nil || askScope != scope {
		return "", a.refused("AskInterrupt", scope, "approval thread is not in the scope")
	}
	var handle control.RequestID
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		if key != "" {
			err := tx.QueryRowContext(ctx, `SELECT handle FROM approvals WHERE asks_in = ? AND ask_key = ?`, a.caller.AsksIn, key).Scan(&handle)
			if err == nil {
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		sid, state, err := a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		var turn int64
		if state != control.Running {
			return fmt.Errorf("thread is not running; send instead: %w", session.ErrRefused)
		}
		err = tx.QueryRowContext(ctx, `SELECT id FROM turns WHERE session_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, sid).Scan(&turn)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("thread is not running; send instead: %w", session.ErrRefused)
		}
		if err != nil {
			return err
		}
		var askKey any
		if key != "" {
			askKey = key
		}
		marked := a.marked(control.Input{Text: reason}).Text
		err = tx.QueryRowContext(ctx, `INSERT INTO approvals (handle, asks_in, action, target, session_id, turn_id, reason, marked, principal, from_id, ask_key, state, opened_at) VALUES ('approval-' || lower(hex(randomblob(16))), ?, 'interrupt', ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?) RETURNING handle`, a.caller.AsksIn, id, sid, turn, reason, marked, a.caller.Principal, a.caller.From, askKey, time.Now().UnixMilli()).Scan(&handle)
		if err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"request": handle, "action": "interrupt", "target": id, "turn": turn, "reason": reason, "principal": a.caller.Principal, "from": a.caller.From})
		_, err = tx.Append(ctx, control.Event{Thread: a.caller.AsksIn, Kind: "approval_asked", Data: data})
		return err
	})
	return handle, err
}

func (a *api) answerApproval(ctx context.Context, req control.RequestID, ans control.Answer) error {
	unlock := a.c.locks.lock("approval:" + string(req))
	defer unlock()
	var scope, state, principal, from, marked string
	var thread, target control.ThreadID
	var guard sendGuard
	err := a.c.Store.DB.QueryRowContext(ctx, `SELECT t.scope, a.asks_in, a.target, a.session_id, a.turn_id, a.principal, a.from_id, a.marked, a.state FROM approvals a JOIN threads t ON t.id = a.asks_in WHERE a.handle = ?`, req).Scan(&scope, &thread, &target, &guard.session, &guard.turn, &principal, &from, &marked, &state)
	if errors.Is(err, sql.ErrNoRows) {
		a.c.Log.Warn("control call refused", "op", "Answer", "principal", string(a.caller.Principal), "request", string(req), "reason", ErrNotFound)
		return fmt.Errorf("core: answer %q: %w", req, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if err := a.act(control.OpAnswer, "Answer", scope); err != nil {
		return err
	}
	if string(a.caller.Principal) == principal {
		return a.refused("Answer", scope, "the asker may not approve its own request")
	}
	if state != "open" {
		return ErrNotOpen
	}
	allow := ans.Allow || (len(ans.Choices) == 1 && ans.Choices[0] == "approve")
	outcome := "refused"
	if allow {
		sender := a.c.As(control.Caller{From: from, Principal: control.Principal(principal), Scopes: []string{scope}, May: control.OpSend}).(*api)
		err := sender.send(ctx, target, "approval:"+string(req), control.Input{Text: marked}, control.Interrupt, &guard)
		if errors.Is(err, errGuardEnded) {
			outcome = "ended"
		} else if err != nil {
			return err
		} else {
			outcome = "interrupted"
		}
	}
	return a.c.Store.Update(ctx, func(tx *store.Tx) error {
		link, err := a.c.ownLink(ctx, tx, thread, ans.Link, a.caller.From)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE approvals SET state = ?, closed_at = ? WHERE handle = ? AND state = 'open'`, outcome, time.Now().UnixMilli(), req)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		data, _ := json.Marshal(map[string]any{"request": req, "allow": allow, "outcome": outcome, "target": target, "principal": a.caller.Principal, "from": a.caller.From, "link": link})
		_, err = tx.Append(ctx, control.Event{Thread: thread, Kind: "approval_answered", Data: data})
		return err
	})
}
