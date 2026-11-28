package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

func (d *DeferredInputs) proposeTx(ctx context.Context, tx *store.Tx, r *deferredRecord) error {
	if r.Target == r.issuer {
		return ErrForbidden
	}
	call, err := json.Marshal(proposalCall{Text: r.Text, Deferred: r.Number})
	if err != nil {
		return err
	}
	var handle string
	err = tx.QueryRowContext(ctx, `INSERT INTO proposals(handle,asks_in,action,target,question,call,mark,principal,from_id,ask_key,state,opened_at)
	 VALUES('proposal-' || lower(hex(randomblob(16))),?,'send',?,'',?,?,?,?,?,'open',?) RETURNING id,handle`,
		r.issuer, r.Target, string(call), r.payload.Mark, string(r.payload.Principal), r.payload.From, "deferred:"+r.key, r.Created.UnixMilli()).Scan(&r.Proposal, &handle)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deferred_sends SET proposal=? WHERE id=?`, r.Proposal, r.Number); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"action": "send", "text": r.Text, "principal": r.payload.Principal, "from": r.payload.From, "target": r.Target, "request": handle, "number": r.Proposal, "deferred": r.Number})
	_, err = tx.Append(ctx, control.Event{Thread: r.issuer, Time: d.Core.now(), Kind: "proposal_made", Data: data})
	return err
}

// Queue acceptance and the human proposal outcome commit together. Approval
// authorizes a future checked handoff; it never calls the immediate send API.
func (d *DeferredInputs) answerProposal(ctx context.Context, a *api, req control.RequestID, ans control.Answer, number int64, thread control.ThreadID, id int64) error {
	link, err := d.Core.ownLink(ctx, d.Core.Store.DB, thread, ans.Link, a.caller.From)
	if err != nil {
		return err
	}
	err = d.Core.Store.Update(ctx, func(tx *store.Tx) error {
		r, e := readDeferred(ctx, tx, "id=?", id)
		if e != nil {
			return e
		}
		if r.Proposal != number || r.issuer != thread || r.State != "proposed" {
			return ErrNotOpen
		}
		var state string
		if e = tx.QueryRowContext(ctx, `SELECT state FROM proposals WHERE id=? AND handle=?`, number, req).Scan(&state); e != nil {
			return e
		}
		if state != "open" {
			return ErrNotOpen
		}
		if a.caller.Principal == r.payload.Principal {
			return ErrForbidden
		}
		allow := ans.Allow || (len(ans.Choices) == 1 && ans.Choices[0] == "approve")
		outcome, reason, next := proposalRefused, "", "refused"
		if allow {
			r.payload.ApprovedBy, r.payload.ApprovedFrom = a.caller.Principal, a.caller.From
			_, e = d.authorityTx(ctx, tx, r, true)
			if e == nil {
				_, e = d.targetTx(ctx, tx, r)
			}
			if e == nil {
				outcome, next = proposalRun, "queued"
			} else {
				outcome, reason = proposalFailed, "deferred authorization or target changed"
			}
		}
		if next == "queued" {
			raw, e := json.Marshal(r.payload)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `UPDATE deferred_sends SET data=? WHERE id=?`, string(raw), id); e != nil {
				return e
			}
		}
		if e = d.transitionTx(ctx, tx, &r, next, reason); e != nil {
			return e
		}
		var linkJSON any
		if link != nil {
			raw, _ := json.Marshal(link)
			linkJSON = string(raw)
		}
		if _, e = tx.ExecContext(ctx, `UPDATE proposals SET state=?,closed_at=?,approved_by=?,approved_from=?,approved_link=?,approved_via=? WHERE id=? AND state='open'`, outcome, d.Core.now().UnixMilli(), string(a.caller.Principal), a.caller.From, linkJSON, a.caller.Via, number); e != nil {
			return e
		}
		data := map[string]any{"request": req, "number": number, "action": "send", "allow": allow, "outcome": outcome, "target": r.Target, "deferred": id, "principal": a.caller.Principal}
		if a.caller.From != "" {
			data["from"] = a.caller.From
		}
		if a.caller.Via != "" {
			data["via"] = a.caller.Via
		}
		if link != nil {
			data["link"] = link
		}
		if reason != "" {
			data["reason"] = reason
		}
		raw, _ := json.Marshal(data)
		_, e = tx.Append(ctx, control.Event{Thread: thread, Time: d.Core.now(), Kind: "proposal_answered", Data: raw})
		return e
	})
	if err == nil {
		d.signal()
	}
	return err
}

// RevalidateTx also handles cancellation after ordinary input acceptance. A
// frozen handoff belongs to its original grant and must be reconciled, never
// reinterpreted as a new send after revocation.
func (d *DeferredInputs) RevalidateTx(ctx context.Context, tx *store.Tx, sid int64, input string) error {
	r, err := boundDeferred(ctx, tx, sid, input)
	if isMissingDeferred(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.State == "handoff" {
		return nil
	}
	if r.State != "accepted" {
		return fmt.Errorf("deferred instruction is %s: %w", r.State, ErrForbidden)
	}
	_, err = d.authorityTx(ctx, tx, r, true)
	if err == nil {
		_, err = d.targetTx(ctx, tx, r)
	}
	if err != nil {
		if e := d.transitionTx(ctx, tx, &r, "refused", "deferred authorization, target or question changed before handoff"); e != nil {
			return e
		}
		return ErrForbidden
	}
	return nil
}

// FailTx is used only by the bound input's recovery, not by a retry that
// happened to find the same text. Terminal receipts cannot be resurrected.
func (d *DeferredInputs) FailTx(ctx context.Context, tx *store.Tx, sid int64, input, reason string) error {
	r, err := boundDeferred(ctx, tx, sid, input)
	if isMissingDeferred(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE deferred_sends SET state='failed',reason=?,updated_at=? WHERE id=? AND state IN ('accepted','handoff')`, reason, d.Core.now().UnixMilli(), r.Number)
	return err
}

// PreventReplacementTx withdraws a bound instruction when its conversation
// cannot resume. It must never create a replacement worker for that input.
func (d *DeferredInputs) PreventReplacementTx(ctx context.Context, tx *store.Tx, sid int64) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deferred_sends WHERE session=? AND (state IN ('accepted','handoff') OR reason='the original conversation cannot resume; no replacement was created')`, sid).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE deferred_sends SET state='failed',reason='the original conversation cannot resume; no replacement was created',updated_at=? WHERE session=? AND state IN ('accepted','handoff')`, d.Core.now().UnixMilli(), sid)
	return true, err
}
