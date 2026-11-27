package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// The places of a scope's conversations with its supervisor
// (Scope.ConversationsAtOnce; design, section 7.1, "Several at once"). A
// conversation takes one while its turn runs, from the moment its input was
// given to it. Input that finds no place is kept in waiting_inputs, accepted
// as any input is, and GiveWaiting gives it once a place is free. Threads of
// other kinds take no place and wait for none.

// KindInputWaits is the event that says that input of a conversation waits
// for a place.
const KindInputWaits control.EventKind = "input_waits"

// placesRetry is how long after a failed pass GiveWaiting makes the next one.
var placesRetry = 30 * time.Second

// places tells GiveWaiting that input was stored that waits. Its zero value
// is ready.
type places struct {
	mu sync.Mutex
	c  chan struct{}
}

func (p *places) stored() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c == nil {
		p.c = make(chan struct{}, 1)
	}
	return p.c
}

func (p *places) tell() {
	select {
	case p.stored() <- struct{}{}:
	default: // it looks anyway
	}
}

// mustWait reports whether an input given to thread id of scope, whose
// current session is in state, waits for a place.
func (c *Core) mustWait(ctx context.Context, tx *store.Tx, scope string, id control.ThreadID, state control.State) (bool, error) {
	var (
		kind control.ThreadKind
		own  int
	)
	err := tx.QueryRowContext(ctx, `SELECT kind, (SELECT count(*) FROM waiting_inputs w WHERE w.thread_id = t.id) FROM threads t WHERE id = ?`,
		int64(id)).Scan(&kind, &own)
	if err != nil {
		return false, fmt.Errorf("core: thread %d: %w", id, err)
	}
	if kind != control.KindConversation {
		return false, nil
	}
	switch state {
	case control.Running:
		// The turn's end gives the place up; an input its agent was given
		// meanwhile would start the next turn without one.
		return own > 0 || c.Scopes[scope].ConversationsAtOnce > 0, nil
	case control.Idle, control.Asleep:
		if own > 0 {
			return true, nil // in order within the conversation
		}
		return c.noPlace(ctx, tx, scope)
	}
	return false, nil // an answer, or refused by the machine
}

// noPlace reports whether a conversation of scope that would start a turn now
// has to wait: no place is free, or another that may have it waits longer.
func (c *Core) noPlace(ctx context.Context, tx *store.Tx, scope string) (bool, error) {
	limit := c.Scopes[scope].ConversationsAtOnce
	if limit <= 0 {
		return false, nil
	}
	taken, err := placesTaken(ctx, tx, scope)
	if err != nil || len(taken) >= limit {
		return true, err
	}
	next, err := waitingNext(ctx, tx, scope)
	return len(next) > 0, err
}

// keep stores in, an input of thread id, until a place is free for it. wake
// is that nobody wrote it.
func keep(ctx context.Context, tx *store.Tx, id control.ThreadID, in FirstInput, wake bool) error {
	b, _ := json.Marshal(in)
	if _, err := tx.ExecContext(ctx, `INSERT INTO waiting_inputs (thread_id, input, wake, pos) VALUES (?, ?, ?, (SELECT coalesce(max(pos), 0) FROM events))`,
		int64(id), string(b), wake); err != nil {
		return fmt.Errorf("core: waiting input: %w", err)
	}
	return nil
}

// keepRetry puts a failed launch's input before its own pending follow-ups.
// All wait from this choice, behind other conversations already waiting.
// The move and the replacement session are committed together.
func keepRetry(ctx context.Context, tx *store.Tx, id control.ThreadID, in FirstInput) error {
	rows, err := tx.QueryContext(ctx, `SELECT input, wake, told FROM waiting_inputs WHERE thread_id = ? ORDER BY id`, int64(id))
	if err != nil {
		return err
	}
	type waiting struct {
		input      string
		wake, told bool
	}
	var following []waiting
	for rows.Next() {
		var w waiting
		if err := rows.Scan(&w.input, &w.wake, &w.told); err != nil {
			rows.Close()
			return err
		}
		following = append(following, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := dropWaiting(ctx, tx, id); err != nil {
		return err
	}
	if err := keep(ctx, tx, id, in, false); err != nil {
		return err
	}
	for _, w := range following {
		if _, err := tx.ExecContext(ctx, `INSERT INTO waiting_inputs (thread_id, input, wake, told, pos)
			VALUES (?, ?, ?, ?, (SELECT coalesce(max(pos), 0) FROM events))`, int64(id), w.input, w.wake, w.told); err != nil {
			return err
		}
	}
	return nil
}

// dropWaiting drops what waits for a place for thread id, which is being
// closed, and returns how many inputs that were.
func dropWaiting(ctx context.Context, tx *store.Tx, id control.ThreadID) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM waiting_inputs WHERE thread_id = ?`, int64(id))
	if err != nil {
		return 0, fmt.Errorf("core: waiting input: %w", err)
	}
	return res.RowsAffected()
}

// placesTaken returns the conversations of scope that run a turn, or start
// for one.
func placesTaken(ctx context.Context, tx *store.Tx, scope string) ([]control.ThreadID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id, s.state, s.first_input FROM threads t
		JOIN sessions s ON s.id = (SELECT max(n.id) FROM sessions n WHERE n.thread_id = t.id)
		WHERE t.scope = ? AND t.kind = ? AND s.state IN (?, ?) ORDER BY t.id`,
		scope, string(control.KindConversation), string(control.Running), string(control.Creating))
	if err != nil {
		return nil, fmt.Errorf("core: conversations that run: %w", err)
	}
	defer rows.Close()
	var taken []control.ThreadID
	for rows.Next() {
		var (
			id           int64
			state, first string
			f            FirstInput
		)
		if err := rows.Scan(&id, &state, &first); err != nil {
			return nil, fmt.Errorf("core: conversations that run: %w", err)
		}
		if control.State(state) == control.Creating && json.Unmarshal([]byte(first), &f) == nil && f.NoInput {
			continue // it is only woken: nothing runs in it
		}
		taken = append(taken, control.ThreadID(id))
	}
	return taken, rows.Err()
}

// waitingInput is the input of a conversation that gets the next place the
// conversation gets.
type waitingInput struct {
	row     int64
	thread  control.ThreadID
	session int64
	state   control.State
	begun   bool // the session's agent was started before
	wake    bool // nobody wrote it
	told    bool // the conversation was told that it waits
}

// waitingNext returns the conversations of scope that a place could be given
// to now, in the order they get one: what a person wrote before a wake, then
// what waits longest. A message waits for a place from the end of the turn
// that the one before it in its conversation had, so one with many messages
// does not hold another's first back; a turn that the agent began by itself,
// that a person typed at its terminal, or that went on after a question was
// withdrawn was none of them.
func waitingNext(ctx context.Context, tx *store.Tx, scope string) ([]waitingInput, error) {
	rows, err := tx.QueryContext(ctx, `SELECT w.id, w.thread_id, s.id, s.state, s.spec != '', w.wake, w.told,
			max(w.pos, coalesce((SELECT max(e.pos) FROM events e JOIN turns u ON u.id = json_extract(e.data, '$.turn')
				WHERE e.thread_id = w.thread_id AND e.kind = ? AND u.started_by = ?), 0)) AS since
		FROM waiting_inputs w JOIN threads t ON t.id = w.thread_id
		JOIN sessions s ON s.id = (SELECT max(n.id) FROM sessions n WHERE n.thread_id = t.id)
		WHERE t.scope = ? AND s.state IN (?, ?)
		ORDER BY w.wake, since, w.id`,
		string(session.KindTurnEnded), session.ByCaller, scope, string(control.Asleep), string(control.Idle))
	if err != nil {
		return nil, fmt.Errorf("core: waiting inputs: %w", err)
	}
	defer rows.Close()
	var next []waitingInput
	for rows.Next() {
		var (
			w     waitingInput
			state string
			since uint64
		)
		if err := rows.Scan(&w.row, &w.thread, &w.session, &state, &w.begun, &w.wake, &w.told, &since); err != nil {
			return nil, fmt.Errorf("core: waiting inputs: %w", err)
		}
		if slices.ContainsFunc(next, func(o waitingInput) bool { return o.thread == w.thread }) {
			continue // a later one of its conversation
		}
		w.state = control.State(state)
		next = append(next, w)
	}
	return next, rows.Err()
}

// GiveWaiting gives what waits for a place, in every scope, as places become
// free, until ctx ends; it returns once what it gave was handed over. Each
// place is for one input. Call it after Recover, which hands over what was
// given before. Without a limit in any scope it returns at once.
func (c *Core) GiveWaiting(ctx context.Context) {
	limited := false
	for _, s := range c.Scopes {
		limited = limited || s.ConversationsAtOnce > 0
	}
	if !limited {
		return
	}
	var hands sync.WaitGroup
	defer hands.Wait()
	for {
		err := c.giveWaiting(ctx, &hands)
		if ctx.Err() != nil {
			return
		}
		c.Log.Error("conversations are not given their places; trying again", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(placesRetry):
		}
	}
}

// giveWaiting is GiveWaiting until an error ends it.
func (c *Core) giveWaiting(ctx context.Context, hands *sync.WaitGroup) error {
	// Read before the first pass, so that no change after it is missed.
	var head uint64
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM events`).Scan(&head); err != nil {
		return err
	}
	follow, end := context.WithCancel(ctx)
	events := make(chan control.Event)
	followed := make(chan error, 1)
	go func() {
		followed <- c.Store.Follow(follow, control.Filter{}, head, events)
		close(followed)
	}()
	defer func() {
		end()
		for range followed { // it is over
		}
	}()
	for {
		var retry <-chan time.Time
		if failed, err := c.givePass(ctx, hands); err != nil {
			return err
		} else if failed {
			retry = time.After(placesRetry)
		}
		for next := false; !next; {
			select {
			case e := <-events:
				next = e.Kind == session.KindState
			case <-c.places.stored():
				next = true
			case <-retry:
				next = true
			case err := <-followed:
				return err
			}
		}
	}
}

// waitingScopes are the scopes in which input waits, but those that have no
// supervisor in the config any more, or left it: no conversation of theirs
// could be launched, and what waits in them is kept.
func (c *Core) waitingScopes(ctx context.Context) ([]string, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT DISTINCT t.scope FROM waiting_inputs w JOIN threads t ON t.id = w.thread_id ORDER BY t.scope`)
	if err != nil {
		return nil, fmt.Errorf("core: waiting inputs: %w", err)
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("core: waiting inputs: %w", err)
		}
		if c.Scopes[s].ConversationsAtOnce > 0 {
			scopes = append(scopes, s)
		}
	}
	return scopes, rows.Err()
}

// givePass gives every place that is free, and tells each conversation whose
// message gets none. failed is that one was not given what it waits for, which the
// others do not wait for.
func (c *Core) givePass(ctx context.Context, hands *sync.WaitGroup) (failed bool, err error) {
	scopes, err := c.waitingScopes(ctx)
	if err != nil {
		return false, err
	}
	for _, scope := range scopes {
		var not []control.ThreadID
		for {
			id, hand, err := c.giveNext(ctx, scope, not)
			if err != nil {
				if id == 0 {
					return false, err
				}
				c.Log.Error("input that waits is not given", "thread", int64(id), "err", err)
				failed, not = true, append(not, id)
				continue
			}
			if hand == nil {
				break
			}
			hands.Add(1)
			go func() {
				defer hands.Done()
				if err := hand(); err != nil {
					c.Log.Error("input that waited for a place", "thread", int64(id), "err", err)
				}
			}()
		}
	}
	return failed, nil
}

// giveNext gives the next free place of scope to the conversation that gets
// it, but one of not, in one transaction, and returns the conversation and
// what hands the input to its agent. With no place or nothing to give, hand
// is nil, and every conversation that a person's message waits in was told
// so. An error is that of the conversation returned, or, with 0, of them all.
func (c *Core) giveNext(ctx context.Context, scope string, not []control.ThreadID) (id control.ThreadID, hand func() error, err error) {
	err = c.Store.Update(ctx, func(tx *store.Tx) error {
		taken, err := placesTaken(ctx, tx, scope)
		if err != nil {
			return err
		}
		next, err := waitingNext(ctx, tx, scope)
		if err != nil {
			return err
		}
		next = slices.DeleteFunc(next, func(w waitingInput) bool { return slices.Contains(not, w.thread) })
		if len(next) == 0 {
			return nil
		}
		if len(taken) < c.Scopes[scope].ConversationsAtOnce {
			id = next[0].thread
			hand, err = c.give(ctx, tx, next[0])
			return err
		}
		for _, w := range next {
			if w.told || w.wake { // a wake is nobody's message to wait for
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE waiting_inputs SET told = 1 WHERE id = ?`, w.row); err != nil {
				return fmt.Errorf("core: waiting input: %w", err)
			}
			data, _ := json.Marshal(map[string]any{"session": w.session, "running": taken})
			if _, err := tx.Append(ctx, control.Event{Thread: w.thread, Scope: scope, Time: c.now(), Kind: KindInputWaits, Data: data}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		hand = nil
	}
	return id, hand, err
}

// give gives a conversation the input w, in tx, and returns what hands it to
// its agent: started with it as any thread is, woken with it, or running.
func (c *Core) give(ctx context.Context, tx *store.Tx, w waitingInput) (func() error, error) {
	var (
		text, started, agent, model, effort string
		in                                  FirstInput
	)
	if err := tx.QueryRowContext(ctx, `SELECT w.input, s.first_input, s.agent, s.model, s.effort FROM waiting_inputs w, sessions s WHERE w.id = ? AND s.id = ?`,
		w.row, w.session).Scan(&text, &started, &agent, &model, &effort); err != nil {
		return nil, fmt.Errorf("core: waiting input: %w", err)
	}
	if err := json.Unmarshal([]byte(text), &in); err != nil {
		return nil, fmt.Errorf("core: waiting input: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM waiting_inputs WHERE id = ?`, w.row); err != nil {
		return nil, fmt.Errorf("core: waiting input: %w", err)
	}
	if w.state == control.Idle {
		pending, hand, err := c.sendTx(ctx, tx, w.session, in)
		return func() error { return c.Runner.Deliver(ctx, w.session, pending, hand) }, err
	}
	if w.begun {
		first, err := c.wakeTx(ctx, tx, w.thread, w.session, in, false)
		return func() error { return c.resume(ctx, w.session, first) }, err
	}
	// Its first input: it starts now, in the directory its start was given.
	var f FirstInput
	if err := json.Unmarshal([]byte(started), &f); err != nil {
		return nil, fmt.Errorf("core: first input of session %d: %w", w.session, err)
	}
	in.Dir = f.Dir
	if err := c.Machine.ApplyTx(ctx, tx, w.session, session.Input{Kind: session.Wake}); err != nil {
		return nil, err
	}
	fi, _ := json.Marshal(in)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ?, launch = 1 WHERE id = ?`, string(fi), w.session); err != nil {
		return nil, fmt.Errorf("core: start: %w", err)
	}
	l := Launch{AcceptedOperation: in.AcceptedOperation, Session: w.session, Thread: w.thread, Agent: agent, Model: model, Effort: effort,
		Principal: in.Principal, From: in.From, Key: in.Key, Input: in.Input(), Context: in.Context, Confined: true, Dir: in.Dir}
	return func() error { return c.launch(ctx, l, "") }, nil
}
