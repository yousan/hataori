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
	if lv.remote || lv.taken != nil || r.Holder == HolderTmux || !agent.CanDeferInput(lv.adapter) {
		return nil, "", errors.New("this adapter or holder cannot durably defer input")
	}
	if _, ok := heldOf(lv.dir); ok {
		return nil, "", errors.New("a held window cannot durably defer input")
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
		return r.do(lv, c.RefuseInput([]string{codecID}, agent.NotDelivered))
	}
	return r.do(lv, agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Input: codecID, Status: agent.Failed, Reason: agent.NotDelivered}}})
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
		got, err = c.InputBatch(host.InputBatch{Run: b.Run, Key: b.Key, Data: b.Data})
		if err != nil {
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
	return r.do(lv, a)
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
