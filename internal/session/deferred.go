package session

import (
	"context"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/store"
)

// This runs only after Step accepted the event, in the same transaction as
// input acknowledgement and pending-slot cleanup. Receiver write receipts do
// not call it. The adapter owns the exact native correlation.
func acknowledgeDeferred(ctx context.Context, tx *store.Tx, sid int64, e agent.Event, now int64) error {
	if e.Input == "" {
		return nil
	}
	state, reason := "", ""
	if e.Kind == agent.InputTaken || e.Kind == agent.TurnStarted {
		state = "delivered"
	}
	if e.Kind == agent.TurnEnded {
		state, reason = "failed", "the bound input ended without an acknowledgement"
	}
	if state == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE deferred_sends SET state=?,reason=?,updated_at=? WHERE session=? AND input_ref=? AND (state='handoff' OR (?='failed' AND state='accepted')) AND (
      (input_ref='first' AND EXISTS(SELECT 1 FROM sessions s WHERE s.id=deferred_sends.session AND s.first_input!=''
       AND json_extract(s.first_input,'$.key')='deferred:' || deferred_sends.id AND json_extract(s.first_input,'$.via')='deferred'))
      OR EXISTS(SELECT 1 FROM pending_inputs p WHERE p.session_id=deferred_sends.session AND cast(p.id AS TEXT)=deferred_sends.input_ref
       AND json_extract(p.data,'$.origin.key')='deferred:' || deferred_sends.id AND json_extract(p.data,'$.origin.via')='deferred'))`, state, reason, now, sid, e.Input, state)
	return err
}
