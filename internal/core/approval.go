package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	if id == a.caller.AsksIn {
		return "", a.refused("AskInterrupt", scope, "cannot interrupt the approval thread")
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
		err = tx.QueryRowContext(ctx, `SELECT handle FROM approvals WHERE target = ? AND session_id = ? AND turn_id = ? AND state = 'open' ORDER BY id LIMIT 1`, id, sid, turn).Scan(&handle)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
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

// recheck decides an approval answered again after an answer that took
// its reason and recorded no outcome. The reason is neither taken nor
// handed over again: the first answer handed it over, or left it pending
// for the runner, which hands pending input over when it reaches the
// session again. The asked turn is interrupted only while it is the
// thread's running turn, named to the runner, which checks it in the step
// that interrupts; otherwise the outcome is ended. So the approval is never
// recorded as interrupted without an interrupt.
func (a *api) recheck(ctx context.Context, target control.ThreadID, sid, turn int64) (string, error) {
	cur, state, err := a.c.current(ctx, a.c.Store.DB, target)
	if err != nil {
		return "", err
	}
	var open int64
	if err := a.c.Store.DB.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM turns WHERE session_id = ? AND ended_at IS NULL`, sid).Scan(&open); err != nil {
		return "", err
	}
	if cur != sid || state != control.Running || open != turn {
		return "ended", nil
	}
	err = a.c.Runner.Interrupt(ctx, sid, turn, "")
	if errors.Is(err, session.ErrRefused) {
		return "ended", nil
	}
	if err != nil {
		return "", fmt.Errorf("core: approved interrupt of thread %d: %w", target, err)
	}
	return "interrupted", nil
}

// noRequestErr is Answer's error for request req when there is no such
// question or approval, and for one of a scope the caller may not see.
func noRequestErr(req control.RequestID) error {
	return fmt.Errorf("core: answer %q: %w", req, ErrNotFound)
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
		return noRequestErr(req)
	}
	if err != nil {
		return err
	}
	if !slices.Contains(a.caller.Scopes, scope) {
		a.hidden("Answer", "request", string(req), scope)
		return noRequestErr(req)
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
	link, err := a.c.ownLink(ctx, a.c.Store.DB, thread, ans.Link, a.caller.From)
	if err != nil {
		return err
	}
	allow := ans.Allow || (len(ans.Choices) == 1 && ans.Choices[0] == "approve")
	outcome := "refused"
	if allow {
		unlockTurn := a.c.locks.lock(fmt.Sprintf("approval-turn:%d:%d:%d", target, guard.session, guard.turn))
		defer unlockTurn()
		var interrupted bool
		err := a.c.Store.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM approvals WHERE target = ? AND session_id = ? AND turn_id = ? AND state = 'interrupted')`, target, guard.session, guard.turn).Scan(&interrupted)
		if err != nil {
			return err
		}
		if interrupted {
			outcome = "ended"
		} else {
			sender := a.c.As(control.Caller{From: from, Principal: control.Principal(principal), Scopes: []string{scope}, May: control.OpSend}).(*api)
			err := sender.send(ctx, target, "approval:"+string(req), control.Input{Text: marked}, control.Interrupt, &guard)
			switch {
			case err == nil && guard.repeated:
				// An earlier answer took the reason and recorded no outcome:
				// its interrupt failed, or writing the outcome did. The key
				// does not decide it: the asked turn is interrupted now if it
				// still runs, else the outcome is ended (#523).
				if outcome, err = a.recheck(ctx, target, guard.session, guard.turn); err != nil {
					return err
				}
			case errors.Is(err, errGuardEnded):
				outcome = "ended"
			case guard.ended || guard.interrupted:
				// What the interrupt did stands, also when the reason could
				// not be handed over: the approval is not left open for an
				// answer that would record something else.
				outcome = "interrupted"
				if guard.ended {
					outcome = "ended"
				}
				if err != nil {
					a.c.Log.Warn("approved interrupt: the reason was not handed over", "request", string(req), "thread", int64(target), "err", err)
				}
			case err != nil:
				return err
			default:
				outcome = "interrupted"
			}
		}
	}
	// The outcome is written whatever the caller's context says by now: an
	// interrupt that was sent or refused is recorded as such, and not left
	// to an answer given again, which can only look at the turn (#523).
	ctx = context.WithoutCancel(ctx)
	return a.c.Store.Update(ctx, func(tx *store.Tx) error {
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
