package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
)

func (r *Runner) deferredResume(ctx context.Context, sid int64, first firstInput, resumeID *string) error {
	if r.Deferred == nil {
		return nil
	}
	expected, bound, err := r.Deferred.ResumeIdentity(ctx, sid)
	if err != nil {
		return err
	}
	if first.Via == control.ViaDeferred && strings.HasPrefix(first.Key, core.DeferredKeyPrefix) && !bound {
		return core.ErrForbidden
	}
	if bound && resumeID != nil && *resumeID != expected {
		return errors.New("deferred input cannot start a new or different conversation")
	}
	return nil
}

// deferredClient never starts a host or changes its holder to gain support.
// Each receipt operation has a dedicated bounded connection.
func (r *Runner) deferredClient(ctx context.Context, lv *live) (*host.Client, string, error) {
	if lv.remote || lv.taken != nil || !agent.CanDeferInput(lv.adapter) {
		return nil, "", errors.New("this adapter or holder cannot durably defer input")
	}
	if _, ok := heldOf(lv.dir); ok {
		return nil, "", errors.New("a held window cannot durably defer input")
	}
	if _, ok := lv.codec.(agent.DeferredInputCodec); !ok {
		return nil, "", errors.New("the codec has no current native eligibility contract")
	}
	c, err := host.DialContext(ctx, lv.dir)
	if err != nil {
		return nil, "", err
	}
	c.Deadline(time.Now().Add(2 * time.Second))
	cap, err := c.InputCapability()
	if err != nil {
		c.Close()
		return nil, "", err
	}
	return c, cap.Run, nil
}

func (r *Runner) DeferredTransport(ctx context.Context, sid int64) (string, error) {
	lv, err := r.get(sid)
	if err != nil {
		return "", err
	}
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if lv.ended || lv.failed || lv.asleep || lv.warmPending || lv.readErr != nil || lv.catchingUp || !r.sessionChecked(sid) {
		return "", core.ErrMachineUnchecked
	}
	c, run, err := r.deferredClient(ctx, lv)
	if err != nil {
		return "", err
	}
	c.Close()
	return run, nil
}

func (r *Runner) codecInput(lv *live, in agent.Input) (agent.Actions, error) {
	if r.Deferred != nil {
		b, ok, err := r.Deferred.Input(context.Background(), lv.id, in.ID)
		if err != nil {
			return agent.Actions{}, err
		}
		if ok {
			in.IdleOnly = true
			in.ID = fmt.Sprintf("%s%d", core.DeferredKeyPrefix, b.Number)
			if in.Written {
				in.Command = b.Command
				in.WrittenAt = r.writtenClock(lv, b.CompletedAt)
			}
		}
	}
	return lv.codec.Input(in)
}

func (r *Runner) failDeferred(lv *live, input, reason string) error {
	codecID := input
	if strings.HasPrefix(input, core.DeferredKeyPrefix) {
		slot, bound, err := r.Deferred.CodecSlot(context.Background(), r.Store.DB, lv.id, input)
		if err != nil {
			return err
		}
		input = slot
		if !bound {
			input = ""
		}
	} else {
		b, bound, err := r.Deferred.Input(context.Background(), lv.id, input)
		if err != nil {
			return err
		}
		if bound {
			codecID = fmt.Sprintf("%s%d", core.DeferredKeyPrefix, b.Number)
		}
	}
	if input != "" {
		if err := r.Store.Update(context.Background(), func(tx *store.Tx) error { return r.Deferred.FailTx(context.Background(), tx, lv.id, input, reason) }); err != nil {
			return err
		}
	}
	if c, ok := lv.codec.(agent.InputRefuser); ok {
		return r.commitDeferredRestore(lv, c.RefuseInput([]string{codecID}, agent.NotDelivered))
	}
	return r.commitDeferredRestore(lv, agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Input: codecID, Status: agent.Failed, Reason: agent.NotDelivered}}})
}

// actDeferred returns handled even on a refusal: an opt-in instruction can
// never fall through to the ordinary raw input path.
func (r *Runner) actDeferred(lv *live, a agent.Actions) (bool, error) {
	if r.Deferred == nil {
		return false, nil
	}
	var input, codecID string
	for _, id := range a.Inputs {
		if strings.HasPrefix(id, core.DeferredKeyPrefix) {
			codecID = id
			break
		}
	}
	if codecID == "" {
		return false, nil
	}
	if !r.sessionChecked(lv.id) || lv.catchingUp || lv.warmPending || lv.readErr != nil {
		lv.warmPending, lv.pending = true, true
		return true, core.ErrMachineUnchecked
	}
	var bound bool
	input, bound, err := r.Deferred.CodecSlot(context.Background(), r.Store.DB, lv.id, codecID)
	if err != nil {
		return true, err
	}
	refuse := func(reason string) (bool, error) { return true, r.failDeferred(lv, codecID, reason) }
	if !bound {
		return refuse("the original deferred input generation is no longer current")
	}
	ad, ok := lv.adapter.(agent.DeferredInputAdapter)
	if !ok || !ad.CanDeferInput() || ad.ValidateDeferredInput(a) != nil || len(a.Inputs) != 1 || len(a.Keys) != 0 || len(a.Write) == 0 || len(a.Reply) != 0 {
		return refuse("the adapter did not emit one complete supported deferred submission")
	}
	// Only complete pipe submissions are supported. Terminal paste and submit
	// have separate effects, even if transported in one byte batch.
	if state, ok := lv.codec.(agent.DeferredInputCodec); !ok || !state.DeferredInputReady(a.Write) {
		lv.warmPending, lv.pending = true, true
		return true, nil // no freeze/mark/write; reconstruct the held input
	}
	c, run, err := r.deferredClient(context.Background(), lv)
	if err != nil {
		return refuse("the original owned receiver is unavailable or unsupported")
	}
	defer c.Close()
	b := core.DeferredBatch{Run: run, Data: a.Write, Command: a.Write, Digest: host.InputBatchDigest(a.Write)}
	var denied error
	err = r.Store.Update(context.Background(), func(tx *store.Tx) error {
		b, denied = r.Deferred.FreezeTx(context.Background(), tx, lv.id, input, b)
		if errors.Is(denied, core.ErrForbidden) {
			return nil
		}
		if denied != nil {
			return denied
		}
		return r.setWrittenTx(context.Background(), tx, lv, []string{input}, true, a.InputText)
	})
	if err != nil || denied != nil {
		return refuse("deferred authorization, target, question or frozen receiver changed before input")
	}
	r.at("deferred frozen")
	return true, r.submitDeferred(lv, c, input, b)
}

func (r *Runner) submitDeferred(lv *live, c *host.Client, input string, b core.DeferredBatch) error {
	lv.deferredBlocked = true
	got, err := c.InputReceipt(b.Run, b.Key)
	if err != nil {
		var refused *host.InputBatchRefused
		if errors.As(err, &refused) {
			return r.failDeferred(lv, input, "the original receiver explicitly refused reconciliation; no new receiver was used")
		}
		return err
	} // ambiguous connection: reconcile the original key
	if got.State == "absent" {
		var denied error
		err = r.Store.Update(context.Background(), func(tx *store.Tx) error {
			denied = r.Deferred.BeforeAbsentTx(context.Background(), tx, lv.id, input)
			if errors.Is(denied, core.ErrForbidden) {
				return nil
			}
			return denied
		})
		if err != nil {
			return err
		}
		if denied != nil {
			return r.failDeferred(lv, input, "authorization or question changed before the first receiver attempt")
		}
		state, ok := lv.codec.(agent.DeferredInputCodec)
		if !ok {
			return r.failDeferred(lv, input, "the codec cannot recheck current native idle eligibility")
		}
		if lv.deferredProgress || !state.DeferredInputReady(b.Command) {
			// Proven absence is not uncertain I/O. Let ordinary native replies
			// progress; only this input's unstarted timeout remains suspended.
			lv.deferredBlocked = false
			return nil
		}
		got, err = c.InputBatch(host.InputBatch{Run: b.Run, Key: b.Key, Data: b.Data})
		if err != nil {
			var refused *host.InputBatchRefused
			if errors.As(err, &refused) {
				return r.failDeferred(lv, input, "the receiver explicitly refused the frozen input (capacity or validation); not retried")
			}
			return err
		} // receiver reservation survives a lost reply
	}
	if got.State != "absent" && got.Digest != b.Digest {
		return r.failDeferred(lv, input, "the receiver receipt has different frozen bytes")
	}
	if got.State == "pending" {
		lv.deferredBlocked = true
		return nil
	}
	if got.State != "written" || got.CompletedAt == 0 {
		return r.failDeferred(lv, input, "receiver write failed, was partial or returned an invalid receipt; not retried")
	}
	err = r.Store.Update(context.Background(), func(tx *store.Tx) error {
		return r.Deferred.ResultTx(context.Background(), tx, lv.id, input, "written", got.CompletedAt)
	})
	if err != nil {
		return err
	}
	// Restore exact request correlation with the receiver's actual completion
	// clock. It is not a delivery ACK and does not reset on the next daemon.
	lv.deferredBlocked = false
	return r.refreshDeferred(lv, input)
}

func (r *Runner) reconcileDeferred(lv *live) error {
	lv.deferredBlocked = false
	if r.Deferred == nil {
		return nil
	}
	if !r.sessionChecked(lv.id) || lv.catchingUp || lv.warmPending || lv.readErr != nil {
		lv.deferredBlocked = true
		return core.ErrMachineUnchecked
	}
	all, err := r.Deferred.Handoffs(context.Background(), lv.id)
	if err != nil {
		lv.deferredBlocked = true
		return err
	}
	for input, b := range all {
		// A completed receipt already has a durable clock. Continue reading
		// for the exact adapter ACK; do not renew the input timeout.
		if b.Result == "written" {
			continue
		}
		c, run, e := r.deferredClient(context.Background(), lv)
		if e != nil {
			if r.polled(lv.dir).Ended() {
				if e = r.failDeferred(lv, input, "the original receiver ended; delivery is unconfirmed and was not retried"); e != nil {
					return e
				}
				continue
			}
			lv.deferredBlocked = true
			return e
		}
		if run != b.Run {
			c.Close()
			if e = r.failDeferred(lv, input, "the original receiver run was lost; no new receiver was used"); e != nil {
				return e
			}
			continue
		}
		e = r.submitDeferred(lv, c, input, b)
		c.Close()
		if e != nil {
			lv.deferredBlocked = true
			return fmt.Errorf("reconciling deferred input: %w", e)
		}
	}
	return nil
}

// Refresh only the bound slot; ordinary Written inputs must not be handed to
// their codec a second time when a deferred receiver receipt arrives.
func (r *Runner) refreshDeferred(lv *live, input string) error {
	var in agent.Input
	in.ID, in.Written = input, true
	if input == firstInputID {
		var raw string
		var f firstInput
		if err := r.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=?`, lv.id).Scan(&raw); err != nil {
			return err
		}
		if raw == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			return err
		}
		in.Text = f.Context + f.Input().ForAgent()
	} else {
		var raw, earlier string
		var c control.Input
		err := r.Store.DB.QueryRow(`SELECT data,context FROM pending_inputs WHERE session_id=? AND id=?`, lv.id, input).Scan(&raw, &earlier)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &c); err != nil {
			return err
		}
		in.Text = earlier + c.Text
	}
	a, err := r.codecInput(lv, in)
	if err != nil {
		return r.failDeferred(lv, input, "adapter could not restore exact deferred input correlation")
	}
	return r.commitDeferredRestore(lv, a)
}

func (r *Runner) deferredEvents(ctx context.Context, tx *store.Tx, sid int64, events []agent.Event) ([]agent.Event, error) {
	if r.Deferred == nil {
		return events, nil
	}
	out := make([]agent.Event, 0, len(events))
	for _, e := range events {
		if strings.HasPrefix(e.Input, core.DeferredKeyPrefix) {
			slot, bound, err := r.Deferred.CodecSlot(ctx, tx, sid, e.Input)
			if err != nil {
				return nil, err
			}
			if !bound {
				continue
			}
			e.Input = slot
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *Runner) restoreWrittenInput(lv *live, in agent.Input) {
	bound := false
	if r.Deferred != nil {
		_, ok, err := r.Deferred.Input(context.Background(), lv.id, in.ID)
		if err != nil {
			lv.warmPending, lv.deferredBlocked = true, true
			r.Log.Error("deferred Written lookup", "session", lv.id, "err", err)
			return
		}
		bound = ok
	}
	if !bound {
		r.do(lv, r.inputActions(lv, in))
		return
	}
	a, err := r.codecInput(lv, in)
	if err == nil {
		err = r.commitDeferredRestore(lv, a)
	}
	if err != nil {
		lv.warmPending, lv.pending, lv.deferredBlocked = true, true, true
		r.Log.Error("deferred Written restoration waits", "session", lv.id, "err", err)
	}
}

// Written restoration or refusal may emit events without any new I/O. Their commit error must
// propagate: the codec has consumed that ACK and must reconstruct it again.
// The ordinary do path and its existing error behavior are unchanged.
func (r *Runner) commitDeferredRestore(lv *live, a agent.Actions) error {
	if len(a.Keys) > 0 || len(a.Write) > 0 || len(a.Reply) > 0 {
		return errors.New("deferred event restoration attempted new I/O")
	}
	if len(a.Events) == 0 {
		return nil
	}
	b := batch{events: a.Events, offset: lv.offset, screenOffset: lv.seenTo, files: map[string]int64{}}
	for p, o := range lv.files {
		b.files[p] = o
	}
	_, err := r.commit(lv, b)
	if err != nil {
		lv.warmPending, lv.pending, lv.deferredBlocked = true, true, true
	}
	return err
}

// If codec actions were consumed from committed output while a receiver was
// pending, reconstruct them from durable input/reply/read evidence before any
// later write. Do not leave an unsent steer waiting forever for its ACK.
func (r *Runner) holdDeferredActions(lv *live, actions []agent.Actions) {
	// Even no-action inputs may already be held inside this codec from
	// before the freeze. Reconstruct before revisiting their durable rows.
	lv.pending, lv.warmPending = true, true
}

func actionsHaveIO(actions []agent.Actions) bool {
	for _, a := range actions {
		if len(a.Keys) != 0 || len(a.Write) != 0 || len(a.Reply) != 0 {
			return true
		}
	}
	return false
}

func deferredAction(a agent.Actions) bool {
	for _, id := range a.Inputs {
		if strings.HasPrefix(id, core.DeferredKeyPrefix) {
			return true
		}
	}
	return false
}

// Probe before consuming fresh native output. A pending/unknown receiver must
// not make one-shot native responses get committed and then lost on rewarm.
// This is lookup-only: absence always proceeds through fresh read/commit and
// policy/question validation before any first physical attempt.
func (r *Runner) beforeDeferredRead(lv *live) (bool, error) {
	if r.Deferred == nil {
		return true, nil
	}
	if lv.deferredDrain {
		// Retain the fence after an ACK removes the final handoff. A later
		// receiver can never inherit this terminal drain or its queued input.
		lv.deferredBlocked = true
		if !r.polled(lv.dir).Ended() {
			return false, core.ErrMachineUnchecked
		}
		return true, nil
	}
	if lv.warmPending {
		return false, nil // Written restoration failed; reconstruct its exact ACK first
	}
	lv.deferredBlocked = false
	all, err := r.Deferred.Handoffs(context.Background(), lv.id)
	if err != nil {
		lv.deferredBlocked = true
		r.holdDeferredActions(lv, nil)
		return false, err
	}
	if len(all) != 0 && r.polled(lv.dir).Ended() {
		lv.deferredDrain, lv.deferredBlocked = true, true
		return true, nil // includes completed host writes still awaiting exact ACK
	}
	for input, b := range all {
		if b.Result == "written" {
			continue
		}
		c, run, e := r.deferredClient(context.Background(), lv)
		if e != nil {
			lv.deferredBlocked = true
			if r.polled(lv.dir).Ended() {
				lv.deferredDrain = true
				return true, nil // commit exact final ACKs before terminal failure
			}
			r.holdDeferredActions(lv, nil)
			return false, e
		}
		got, e := c.InputReceipt(b.Run, b.Key)
		c.Close()
		var refused *host.InputBatchRefused
		if run != b.Run || errors.As(e, &refused) {
			if e = r.failDeferred(lv, input, "the original receiver run was lost or explicitly refused reconciliation; no new receiver was used"); e != nil {
				lv.deferredBlocked = true
				return false, e
			}
			r.holdDeferredActions(lv, nil)
			return false, nil
		}
		if e != nil || got.State == "pending" {
			lv.deferredBlocked = true
			r.holdDeferredActions(lv, nil)
			return false, e
		}
		if got.State == "absent" {
			lv.deferredBlocked = false
			continue
		}
		if got.State != "written" || got.Digest != b.Digest || got.CompletedAt == 0 {
			if e = r.failDeferred(lv, input, "the original receiver returned a failed or invalid receipt; not retried"); e != nil {
				lv.deferredBlocked = true
				return false, e
			}
			r.holdDeferredActions(lv, nil)
			return false, nil
		}
		if e = r.Store.Update(context.Background(), func(tx *store.Tx) error {
			return r.Deferred.ResultTx(context.Background(), tx, lv.id, input, "written", got.CompletedAt)
		}); e != nil {
			lv.deferredBlocked = true
			r.holdDeferredActions(lv, nil)
			return false, e
		}
		lv.deferredBlocked = false
		if e = r.refreshDeferred(lv, input); e != nil {
			return false, e
		}
	}
	return true, nil
}
