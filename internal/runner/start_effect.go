package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/workerproto"
)

// startEffect retains the complete original request, including admission
// limits. A retry first resolves its receipt; unknown starts never become a
// fresh host. Only confirmed refusal/no retained intent permits a new attempt.
func (r *Runner) startEffect(ctx context.Context, id int64, remote *machineio.Remote, proposed workerproto.Message) error {
	var action int64
	var raw, state string
	err := r.Store.DB.QueryRowContext(ctx, `SELECT id,request,state FROM session_machine_actions WHERE session_id=? AND run=? AND operation='start' ORDER BY id DESC LIMIT 1`, id, remote.Target.Run).Scan(&action, &raw, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var original workerproto.Message
		if err := json.Unmarshal([]byte(raw), &original); err != nil {
			return err
		}
		if original.Binding != remote.Binding || original.Target == nil || *original.Target != remote.Target {
			return errors.New("runner: start binding differs")
		}
		if state == "complete" {
			return nil
		}
		if state != "refused" {
			query, err := remote.Request("receipt", "", workerproto.ReceiptQuery{Operation: "start", Key: original.Key})
			if err != nil {
				return err
			}
			reply, err := remote.Call(ctx, query)
			var refusal *workerproto.Refusal
			if errors.As(err, &refusal) && refusal.Code == "not_found" {
				// The ready bound resident retains every start intent before
				// launch. Absence allows a newly checked attempt, not reuse of
				// stale policy from the old request.
				state = "refused"
			} else if err != nil {
				return err
			} else {
				var receipt workerproto.Receipt
				if err := json.Unmarshal(reply.Body, &receipt); err != nil {
					return err
				}
				if err := workerproto.CheckReceipt(original, receipt); err != nil {
					return err
				}
				state = receipt.State
			}
			if _, err := r.Store.DB.ExecContext(ctx, `UPDATE session_machine_actions SET state=? WHERE id=?`, state, action); err != nil {
				return err
			}
			if state == "complete" {
				return nil
			}
			if state != "refused" {
				return machineio.ErrUnknown
			}
		}
		proposed.Key = "start-" + runID()
	}
	return r.machineEffect(ctx, id, remote, proposed)
}
