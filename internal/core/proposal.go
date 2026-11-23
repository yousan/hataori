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
	"github.com/yousan/hataori/internal/store"
)

// proposalCall is what an approved proposal runs, as stored with it: a
// send's text, or a start's repository, agent, model, effort and first
// text. The model and the effort are those the proposal showed.
type proposalCall struct {
	Text   string `json:"text"`
	Repo   string `json:"repo,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// The outcomes of a proposal, in its proposal_answered event and its row.
const (
	proposalRun     = "run"     // approved, and the call was made
	proposalRefused = "refused" // a person refused it
	proposalBusy    = "busy"    // approved; the thread ran a turn, or waited on a question it did not answer
	proposalEnded   = "ended"   // approved; the question it answered was no longer open
	proposalClosed  = "closed"  // approved; the thread was closed or removed
	proposalFailed  = "failed"  // approved; the call was refused for another reason, said in reason
)

// asksHere checks that the caller has a thread to ask in, of scope.
func (a *api) asksHere(ctx context.Context, name, scope string) error {
	if a.caller.AsksIn == 0 {
		return a.refused(name, scope, "no thread to propose in")
	}
	var askScope string
	if err := a.c.Store.DB.QueryRowContext(ctx, `SELECT scope FROM threads WHERE id = ?`, a.caller.AsksIn).Scan(&askScope); err != nil || askScope != scope {
		return a.refused(name, scope, "the thread to propose in is not in the scope")
	}
	return nil
}

// proposeStart stores the Start r of a caller with Propose, which the
// start checked up to the point where it would run, with the agent, the
// model and the effort it would run with.
func (a *api) proposeStart(ctx context.Context, r control.StartRequest, agent, model, effort string) error {
	if err := a.asksHere(ctx, "Start", r.Scope); err != nil {
		return err
	}
	call := proposalCall{Text: r.Input.Text, Repo: r.Repo, Agent: agent, Model: model, Effort: effort}
	return a.propose(ctx, "start", r.Key, call, func(tx *store.Tx, d map[string]any) (control.ThreadID, string, error) {
		d["repo"], d["agent"], d["model"], d["effort"] = r.Repo, agent, model, effort
		return 0, "", nil
	})
}

// proposeSend stores the Send of in to thread id of scope by a caller with
// Propose, which the send checked up to the point where it would run. A
// thread waiting on a question when it is proposed gets it as the answer
// to that question, if any.
func (a *api) proposeSend(ctx context.Context, scope string, id control.ThreadID, key string, in control.Input) error {
	if err := a.asksHere(ctx, "Send", scope); err != nil {
		return err
	}
	if id == a.caller.AsksIn {
		return a.refused("Send", scope, "cannot propose a send to the thread it proposes in")
	}
	call := proposalCall{Text: in.Text}
	return a.propose(ctx, "send", key, call, func(tx *store.Tx, d map[string]any) (control.ThreadID, string, error) {
		sid, state, err := a.c.current(ctx, tx, id)
		if err != nil {
			return 0, "", err
		}
		if state == control.Closed || state == control.Removed {
			return 0, "", fmt.Errorf("core: send to %v: %w", id, ErrClosed)
		}
		var question string
		if state == control.Waiting {
			err := tx.QueryRowContext(ctx, `SELECT handle FROM requests WHERE session_id = ? AND state = 'open' ORDER BY id DESC LIMIT 1`, sid).Scan(&question)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, "", fmt.Errorf("core: request: %w", err)
			}
		}
		var model, effort string
		if err := tx.QueryRowContext(ctx, `SELECT model, effort FROM sessions WHERE id = ?`, sid).Scan(&model, &effort); err != nil {
			return 0, "", fmt.Errorf("core: session: %w", err)
		}
		d["target"] = id
		if question != "" {
			d["question"] = question
		}
		if model != "" {
			d["model"] = model
		}
		if effort != "" {
			d["effort"] = effort
		}
		return id, question, nil
	})
}

// propose stores one proposal of action and writes its proposal_made in
// the caller's AsksIn thread, and returns it as a *control.Proposed. A key
// the caller proposed with before returns that proposal. what reads, in
// the same transaction, the target, the question and the event's fields
// of action.
func (a *api) propose(ctx context.Context, action, key string, call proposalCall,
	what func(tx *store.Tx, d map[string]any) (control.ThreadID, string, error)) error {
	var askKey any
	if key != "" {
		askKey = action + ":" + key
	}
	var number int64
	err := a.c.Store.Update(ctx, func(tx *store.Tx) error {
		if askKey != nil {
			err := tx.QueryRowContext(ctx, `SELECT id FROM proposals WHERE asks_in = ? AND ask_key = ?`, a.caller.AsksIn, askKey).Scan(&number)
			if err == nil {
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		d := map[string]any{"action": action, "text": call.Text, "principal": string(a.caller.Principal), "from": a.caller.From}
		target, question, err := what(tx, d)
		if err != nil {
			return err
		}
		for k, v := range d {
			if s, ok := v.(string); ok && s == "" && k != "text" {
				delete(d, k) // optional fields are left out, not empty
			}
		}
		var targetCol any
		if target != 0 {
			targetCol = int64(target)
		}
		b, _ := json.Marshal(call)
		var handle string
		err = tx.QueryRowContext(ctx, `INSERT INTO proposals (handle, asks_in, action, target, question, call, mark, principal, from_id, ask_key, state, opened_at)
			VALUES ('proposal-' || lower(hex(randomblob(16))), ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?) RETURNING id, handle`,
			a.caller.AsksIn, action, targetCol, question, string(b), a.caller.Mark, string(a.caller.Principal), a.caller.From, askKey,
			time.Now().UnixMilli()).Scan(&number, &handle)
		if err != nil {
			return fmt.Errorf("core: proposal: %w", err)
		}
		d["request"], d["number"] = handle, number
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: a.caller.AsksIn, Time: a.c.now(), Kind: "proposal_made", Data: data})
		return err
	})
	if err != nil {
		return err
	}
	return &control.Proposed{Number: number}
}

// answerProposal decides proposal req: refused, or approved and run once,
// as the caller that proposed it, under a key made from it. Who may decide
// it is as for an approval: a person of its scope other than the one who
// proposed it. Its outcome is written once, in one proposal_answered; a
// proposal decided before is ErrNotOpen. An approval whose call failed in
// a way that may pass later (not one of the outcomes) leaves it open, and
// approving it again makes the same call under the same key, which runs
// it once.
func (a *api) answerProposal(ctx context.Context, req control.RequestID, ans control.Answer) error {
	unlock := a.c.locks.lock("proposal:" + string(req))
	defer unlock()
	var (
		number                                                          int64
		scope, action, question, callJSON, mark, principal, from, state string
		thread                                                          control.ThreadID
		target                                                          sql.NullInt64
	)
	err := a.c.Store.DB.QueryRowContext(ctx, `SELECT p.id, t.scope, p.asks_in, p.action, p.target, p.question, p.call, p.mark, p.principal, p.from_id, p.state
		FROM proposals p JOIN threads t ON t.id = p.asks_in WHERE p.handle = ?`, req).
		Scan(&number, &scope, &thread, &action, &target, &question, &callJSON, &mark, &principal, &from, &state)
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
		return a.refused("Answer", scope, "the one who proposed it may not decide it")
	}
	if state != "open" {
		return ErrNotOpen
	}
	link, err := a.c.ownLink(ctx, a.c.Store.DB, thread, ans.Link, a.caller.From)
	if err != nil {
		return err
	}
	var call proposalCall
	if err := json.Unmarshal([]byte(callJSON), &call); err != nil {
		return fmt.Errorf("core: proposal %d: %w", number, err)
	}
	allow := ans.Allow || (len(ans.Choices) == 1 && ans.Choices[0] == "approve")
	outcome, reason := proposalRefused, ""
	if allow {
		// As the one who proposed it, whose person must still be allowed
		// in the scope; marked as its input; never into a running turn.
		sender := a.c.As(control.Caller{From: from, Principal: control.Principal(principal), Scopes: []string{scope},
			May: control.OpStart | control.OpSend, RefuseBusy: true, NoAnswer: question == "", Mark: mark,
			Via: control.ViaProposal}).(*api)
		key := "proposal:" + string(req)
		var err error
		switch action {
		case "send":
			err = sender.send(ctx, control.ThreadID(target.Int64), key, control.Input{Text: call.Text}, control.Refuse,
				&sendGuard{proposal: true, question: question})
		case "start":
			var id control.ThreadID
			id, err = sender.start(ctx, control.StartRequest{Key: key, Scope: scope, Repo: call.Repo, Agent: call.Agent,
				Model: call.Model, Effort: call.Effort, Input: control.Input{Text: call.Text}}, "")
			if id != 0 {
				// The thread is made; a launch that failed is the thread's own
				// failure, which its events show.
				target, err = sql.NullInt64{Int64: int64(id), Valid: true}, nil
			}
		default:
			return fmt.Errorf("core: proposal %d: unknown action %q", number, action)
		}
		if outcome, reason = proposalOutcome(err); outcome == "" {
			return err
		}
		if outcome == proposalFailed {
			a.c.Log.Warn("approved proposal not run", "request", string(req), "number", number, "action", action, "err", err)
		}
	}
	// The outcome is written whatever the caller's context says by now.
	ctx = context.WithoutCancel(ctx)
	return a.c.Store.Update(ctx, func(tx *store.Tx) error {
		var targetCol any
		if target.Valid {
			targetCol = target.Int64
		}
		res, err := tx.ExecContext(ctx, `UPDATE proposals SET state = ?, target = ?, closed_at = ? WHERE handle = ? AND state = 'open'`,
			outcome, targetCol, time.Now().UnixMilli(), req)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		d := map[string]any{"request": req, "number": number, "action": action, "allow": allow, "outcome": outcome,
			"principal": a.caller.Principal, "from": a.caller.From, "link": link}
		if target.Valid {
			d["target"] = target.Int64
		}
		if reason != "" {
			d["reason"] = reason
		}
		if a.caller.Via != "" {
			d["via"] = a.caller.Via
		}
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: thread, Time: a.c.now(), Kind: "proposal_answered", Data: data})
		return err
	})
}

// proposalOutcome is the outcome of an approved proposal whose call
// returned err, and for failed the reason, in hataori's words; "" when err
// may pass if the call is made again, so the proposal stays open.
func proposalOutcome(err error) (outcome, reason string) {
	var stopped *control.Stopped
	switch {
	case err == nil:
		return proposalRun, ""
	case errors.Is(err, ErrBusy):
		return proposalBusy, ""
	case errors.Is(err, errGuardEnded):
		return proposalEnded, ""
	case errors.Is(err, ErrClosed):
		return proposalClosed, ""
	case errors.As(err, &stopped) && !stopped.Failed:
		// The same input would be stopped again.
		return proposalFailed, fmt.Sprintf("the plugin %s stopped it at %s", stopped.Plugin, stopped.Point)
	case errors.Is(err, ErrForbidden):
		return proposalFailed, "not allowed"
	case errors.Is(err, ErrDisabled):
		return proposalFailed, "the channel is not enabled"
	case errors.Is(err, ErrNotFound):
		return proposalFailed, "no such thread"
	case errors.Is(err, control.ErrUnknownAgent):
		return proposalFailed, "unknown agent"
	case errors.Is(err, control.ErrUnknownEffort):
		return proposalFailed, "unknown effort"
	}
	return "", ""
}

// openProposals appends to reqs the open proposals shown in thread id, as
// approvals with the choices Approve and Refuse, oldest first; their text
// is left out for a caller that may not read text.
func (c *Core) openProposals(ctx context.Context, id control.ThreadID, reqs []control.Request, noText bool) ([]control.Request, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT id, handle, action, target, call FROM proposals WHERE asks_in = ? AND state = 'open' ORDER BY id`, id)
	if err != nil {
		return reqs, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			number       int64
			r            control.Request
			action, call string
			target       sql.NullInt64
		)
		if err := rows.Scan(&number, &r.ID, &action, &target, &call); err != nil {
			return reqs, err
		}
		r.Kind = control.Approval
		if !noText {
			var p proposalCall
			json.Unmarshal([]byte(call), &p) // written by propose
			if action == "start" {
				r.Text = fmt.Sprintf("Proposal %d: start a thread in %s? %s", number, p.Repo, p.Text)
			} else {
				r.Text = fmt.Sprintf("Proposal %d: send to thread %d? %s", number, target.Int64, p.Text)
			}
		}
		r.Choices = []control.Choice{{Label: "Approve", Token: "approve"}, {Label: "Refuse", Token: "refuse"}}
		reqs = append(reqs, r)
	}
	return reqs, rows.Err()
}
