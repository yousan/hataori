package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// RetryConversation retries a failed launch, keeping its thread number,
// directory and history. It is only for daemon clients: no socket operation
// can select a thread kind or bypass a failed ordinary thread's choices.
// The new call becomes the replacement session's first input.
func (c *Core) RetryConversation(ctx context.Context, caller control.Caller, id control.ThreadID, key string, in control.Input) error {
	a := c.As(caller).(*api)
	scope, err := a.actThread(ctx, control.OpStart, "Converse", id)
	if err != nil {
		return err
	}
	if err := a.left("Converse", scope); err != nil {
		return err
	}
	kind, err := c.Kind(ctx, id)
	if err != nil {
		return err
	}
	if kind != control.KindConversation || key == "" {
		return session.ErrRefused
	}
	defer c.locks.lock(fmt.Sprintf("conversation-retry:%d", id))()
	accepted := "retry:" + scope + "\x00" + key
	var n int
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM inbox WHERE key = ?`, accepted).Scan(&n); err != nil || n > 0 {
		return err
	}
	t, err := a.Thread(ctx, id, 0, 0)
	if err != nil {
		return err
	}
	if t.State != control.Failed || t.FailedStart == nil {
		return session.ErrRefused
	}
	if in.Link != nil {
		if err := c.checkConfinedLink(ctx, *in.Link, kind); err != nil {
			return err
		}
	}
	in = a.marked(in)
	if in, err = a.inputHooks(ctx, "Send", id, scope, in); err != nil {
		return err
	}
	var launch Launch
	var waits bool
	err = c.Store.Update(ctx, func(tx *store.Tx) error {
		sid, state, err := c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		if sid != t.FailedStart.Session || state != control.Failed {
			return session.ErrRefused
		}
		ok, err := tx.Accept(ctx, accepted, json.RawMessage(`{}`))
		if err != nil || !ok {
			return err
		}
		if in.Link, err = c.ownLink(ctx, tx, id, in.Link, caller.From); err != nil {
			return err
		}
		launch, err = c.again(ctx, tx, id, scope, sid, c.now(), "conversation called again")
		if err != nil {
			return err
		}
		first := FirstInput{Principal: a.caller.Principal, From: caller.From, Key: key, Text: in.Text, Link: in.Link, Before: in.Before, After: in.After, Via: a.caller.Via, Dir: launch.Dir, Context: launch.Context}
		if waits, err = c.noPlace(ctx, tx, scope); err != nil {
			return err
		}
		if waits {
			if err := keep(ctx, tx, id, first, false); err != nil {
				return err
			}
			first = FirstInput{Dir: launch.Dir, Context: launch.Context}
		}
		b, _ := json.Marshal(first)
		state = control.Creating
		run := 1
		if waits {
			state, run = control.Asleep, 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ?, state = ?, launch = ? WHERE id = ?`, string(b), string(state), run, launch.Session); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ? WHERE id = ?`, string(state), int64(id)); err != nil {
			return err
		}
		launch.Principal, launch.From, launch.Key, launch.Input = a.caller.Principal, caller.From, key, in
		return nil
	})
	if err != nil {
		return err
	}
	if waits {
		c.places.tell()
		return nil
	}
	return c.startAgain(ctx, launch)
}
