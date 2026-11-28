package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yousan/hataori/internal/store"
)

func isMissingDeferred(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Ordinary pending rowids and the "first" slot are reusable. The current
// input's durable origin key, not that slot alone, binds a queue generation.
const deferredBinding = `session=? AND input_ref=? AND (
 (input_ref='first' AND EXISTS(SELECT 1 FROM sessions s WHERE s.id=deferred_sends.session AND s.first_input!=''
  AND json_extract(s.first_input,'$.key')='deferred:' || deferred_sends.id AND json_extract(s.first_input,'$.via')='deferred'))
 OR EXISTS(SELECT 1 FROM pending_inputs p WHERE p.session_id=deferred_sends.session AND cast(p.id AS TEXT)=deferred_sends.input_ref
  AND json_extract(p.data,'$.origin.key')='deferred:' || deferred_sends.id AND json_extract(p.data,'$.origin.via')='deferred'))`

func boundDeferred(ctx context.Context, q deferredReader, sid int64, input string) (deferredRecord, error) {
	return readDeferred(ctx, q, deferredBinding, sid, input)
}

// Input identifies the durable instruction bound to this exact delivery slot.
// Terminal rows remain visible to prevent cancellation from becoming a resend.
func (d *DeferredInputs) Input(ctx context.Context, sid int64, input string) (DeferredBatch, bool, error) {
	if input == "" {
		return DeferredBatch{}, false, nil
	}
	r, err := boundDeferred(ctx, d.Core.Store.DB, sid, input)
	if isMissingDeferred(err) {
		return DeferredBatch{}, false, nil
	}
	r.batch.Number = r.Number
	return r.batch, err == nil, err
}

// Deferred codec IDs carry the immutable queue generation. A late action/ACK
// cannot borrow an ordinary rowid or the next use of the first-input slot.
func (d *DeferredInputs) CodecSlot(ctx context.Context, q deferredReader, sid int64, codecID string) (string, bool, error) {
	if !strings.HasPrefix(codecID, DeferredKeyPrefix) {
		return "", false, nil
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(codecID, DeferredKeyPrefix), 10, 64)
	if err != nil || n <= 0 {
		return "", false, ErrForbidden
	}
	r, err := readDeferred(ctx, q, "id=? AND session=?", n, sid)
	if isMissingDeferred(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	current, err := boundDeferred(ctx, q, sid, r.input)
	if isMissingDeferred(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if current.Number != n {
		return "", false, nil
	}
	return r.input, true, nil
}

// FreezeTx is the cancellation/authorization cutoff. The caller also records
// the ordinary Written marker in this transaction, before any receiver request.
func (d *DeferredInputs) FreezeTx(ctx context.Context, tx *store.Tx, sid int64, input string, b DeferredBatch) (DeferredBatch, error) {
	r, err := boundDeferred(ctx, tx, sid, input)
	if err != nil {
		return b, err
	}
	if r.State == "handoff" {
		if b.Run != r.batch.Run {
			return b, errors.New("the original deferred receiver run is gone")
		}
		return r.batch, nil
	}
	if err = d.RevalidateTx(ctx, tx, sid, input); err != nil {
		return b, err
	}
	if b.Run == "" || b.Digest == "" || len(b.Data) == 0 {
		return b, errors.New("missing frozen deferred action")
	}
	b.Key = fmt.Sprintf("deferred-%d-%d", r.Number, sid)
	b.Number = r.Number
	b.Result = "frozen"
	raw, err := json.Marshal(b)
	if err != nil {
		return b, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deferred_sends SET handoff=? WHERE id=?`, string(raw), r.Number); err != nil {
		return b, err
	}
	err = d.transitionTx(ctx, tx, &r, "handoff", "")
	return b, err
}

// BeforeAbsentTx is needed only when the original surviving receiver proves
// no first attempt exists. A previous freeze is not a timeless new grant.
func (d *DeferredInputs) BeforeAbsentTx(ctx context.Context, tx *store.Tx, sid int64, input string) error {
	r, err := boundDeferred(ctx, tx, sid, input)
	if err != nil {
		return err
	}
	if r.State != "handoff" {
		return errors.New("deferred handoff is no longer active")
	}
	_, err = d.authorityTx(ctx, tx, r, true)
	if err == nil {
		_, err = d.targetTx(ctx, tx, r)
	}
	if err != nil {
		if e := d.FailTx(ctx, tx, sid, input, "authorization, target or question changed before the receiver's first attempt"); e != nil {
			return e
		}
		return ErrForbidden
	}
	return nil
}

func (d *DeferredInputs) ResultTx(ctx context.Context, tx *store.Tx, sid int64, input, result string, completed int64) error {
	r, err := boundDeferred(ctx, tx, sid, input)
	if err != nil {
		return err
	}
	if r.State != "handoff" {
		return nil
	}
	r.batch.Result, r.batch.CompletedAt = result, completed
	raw, err := json.Marshal(r.batch)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deferred_sends SET handoff=?,updated_at=? WHERE id=?`, string(raw), d.Core.now().UnixMilli(), r.Number); err != nil {
		return err
	}
	if result == "failed" {
		return d.FailTx(ctx, tx, sid, input, "receiver write failed or was partial; not retried")
	}
	return nil
}

// Handoffs enumerates only unresolved frozen submissions. A surviving host's
// cached receipt, rather than a new codec rendering, determines recovery.
func (d *DeferredInputs) Handoffs(ctx context.Context, sid int64) (map[string]DeferredBatch, error) {
	rows, err := d.Core.Store.DB.QueryContext(ctx, `SELECT input_ref,handoff FROM deferred_sends WHERE session=? AND state='handoff'`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DeferredBatch{}
	for rows.Next() {
		var input, raw string
		var b DeferredBatch
		if err = rows.Scan(&input, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, err
		}
		out[input] = b
	}
	return out, rows.Err()
}

// ResumeIdentity validates an accepted sleeping input before any launch
// effects. A frozen handoff must keep its surviving original host instead.
func (d *DeferredInputs) ResumeIdentity(ctx context.Context, sid int64) (string, bool, error) {
	var conversation string
	var bound bool
	var denied error
	err := d.Core.Store.Update(ctx, func(tx *store.Tx) error {
		r, e := boundDeferred(ctx, tx, sid, "first")
		if isMissingDeferred(e) {
			return nil
		}
		if e != nil {
			return e
		}
		bound = true
		conversation = r.payload.Conversation
		if r.State == "handoff" {
			denied = ErrForbidden
			return d.FailTx(ctx, tx, sid, "first", "a frozen handoff cannot resume into a different receiver")
		}
		denied = d.RevalidateTx(ctx, tx, sid, "first")
		if errors.Is(denied, ErrForbidden) {
			return nil
		}
		return denied
	})
	if err != nil {
		return "", bound, err
	}
	return conversation, bound, denied
}
