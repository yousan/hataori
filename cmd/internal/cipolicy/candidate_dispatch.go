package cipolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Candidate dispatch has its own pre-lane receipt because releasing coverage
// debt, integration Done and main promotion are separate authorities. It uses
// the same Intent, immutable ref, actual HTTP sender and discovery as bootstrap.
func candidateIntent(ctx context.Context, q queryer, record CandidateRecord) (Intent, error) {
	var i Intent
	var id string
	var raw []byte
	err := q.QueryRowContext(ctx, "SELECT intent,sweep,status,run,attempt FROM candidate_intent WHERE candidate_id=?", record.ID).Scan(&id, &raw, &i.Status, &i.RunID, &i.Attempt)
	if err != nil {
		return i, err
	}
	if err = json.Unmarshal(raw, &i.Sweep); err != nil {
		return i, err
	}
	if id != i.Sweep.ID || i.Sweep != record.Tuple.Request.Sweep || (i.Status == "bound" && (i.RunID <= 0 || i.Attempt != 1)) || (i.Status != "bound" && (i.RunID != 0 || i.Attempt != 0)) {
		return i, fmt.Errorf("candidate outbox seal mismatch")
	}
	return i, nil
}

func (a *Adapter) candidateDispatchReady(ctx context.Context, c verifiedControl, r CandidateRequirement) error {
	pr, err := a.candidatePull(ctx, r, false)
	if err != nil {
		return err
	}
	if err = a.candidateRefs(ctx, r, false, ""); err != nil {
		return err
	}
	if _, err = a.candidateReviews(ctx, c, r, pr, a.clock()); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) candidateFrozenRef(ctx context.Context, r CandidateRequirement) error {
	var ref struct{ Object struct{ SHA string } }
	if err := a.request(ctx, "GET", a.prefix()+"/git/ref/"+dispatchRef(r.Sweep), nil, &ref); err != nil {
		return err
	}
	if ref.Object.SHA != r.Result {
		return fmt.Errorf("frozen candidate dispatch ref moved")
	}
	return nil
}

// Binding a run identifies the original attempt; it grants no full validation.
// The current run endpoint is necessary: attempts/1 alone can hide a later rerun.
func (a *Adapter) candidateDispatchRun(ctx context.Context, c verifiedControl, r CandidateRequirement, id int64) (runRecord, error) {
	var run runRecord
	if id <= 0 {
		return run, fmt.Errorf("candidate run missing")
	}
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d", a.prefix(), id), nil, &run); err != nil {
		return run, err
	}
	if err := a.runIdentity(run, c, r.Sweep, id, 1); err != nil {
		return run, err
	}
	if run.HeadBranch != strings.TrimPrefix(dispatchRef(r.Sweep), "heads/") || run.CreatedAt.IsZero() || run.CreatedAt.Before(c.approvedAt) || run.CreatedAt.Before(c.selection.policy.StartsAt) || !run.CreatedAt.Before(r.ExpiresAt) || run.CreatedAt.After(a.clock()) || run.UpdatedAt.Before(run.CreatedAt) || run.UpdatedAt.After(a.clock()) {
		return run, fmt.Errorf("candidate run original timestamp/ref mismatch")
	}
	switch run.Status {
	case "queued", "in_progress", "waiting", "pending", "requested":
		if run.Conclusion != "" {
			return run, fmt.Errorf("candidate run status ambiguous")
		}
	case "completed":
		switch run.Conclusion {
		case "success", "failure", "timed_out", "cancelled", "startup_failure", "action_required", "neutral", "skipped":
		default:
			return run, fmt.Errorf("candidate run conclusion unknown")
		}
	default:
		return run, fmt.Errorf("candidate run status unknown")
	}
	return run, nil
}

func candidateFresh(s *Store, a *Adapter, c verifiedControl, record CandidateRecord, history bool) error {
	now := s.clock()
	if !now.Before(a.root.ExpiresAt) || c.selection.Report(now).FullOnly || (!history && !now.Before(record.Tuple.Request.ExpiresAt)) {
		return fmt.Errorf("candidate dispatch authority expired")
	}
	return nil
}

func candidateCurrent(ctx context.Context, q queryer, record CandidateRecord) error {
	current, err := candidateAt(ctx, q, record.Tuple.Request.Pull)
	if err != nil {
		return err
	}
	if current.ID != record.ID || current.Revision != record.Revision || current.Status == "invalidated" {
		return ErrConflict
	}
	return nil
}

func (c *Coordinator) candidateDispatchOperation(ctx context.Context, operation string, pull int) (out Outcome, err error) {
	a, s := c.adapter, c.store
	ctx = a.budget(ctx)
	out.Operation = operation
	record, err := candidateAt(ctx, s.db, pull)
	if err != nil {
		return out, err
	}
	// Even an error exposes retained uncertainty without granting permission.
	defer func() {
		out.Candidate = &CandidateReport{Record: record, Boundary: "candidate dispatch receipt only; no full validation, Done, promotion, release or upgrade permission"}
		i, e := candidateIntent(ctx, s.db, record)
		if e == nil {
			out.Intent = &i
			out.Candidate.Dispatch = &i
		} else if e != sql.ErrNoRows && err == nil {
			err = e
		}
	}()
	if record.Tuple.Key != a.keyDigest || record.Tuple.Repository != a.root.RepositoryID || record.Status == "invalidated" {
		return out, fmt.Errorf("candidate dispatch authority changed")
	}
	history := operation == "candidate-dispatch-reconcile"
	var tuple CandidateTuple
	var control verifiedControl
	if history {
		// Observe only the retained signed request, even if the PR has since
		// moved, expired or lost approval. This cannot send or validate it.
		control, err = a.control(ctx)
		if err != nil {
			return out, err
		}
		want, _ := json.Marshal(record.Tuple.Request)
		got, _ := json.Marshal(a.root.Candidate)
		if string(want) != string(got) || control.revision != record.Tuple.Control || control.authority != record.Tuple.Scope || control.selection.digest != record.Tuple.Policy {
			return out, fmt.Errorf("retained candidate authority changed")
		}
		tuple = record.Tuple
	} else {
		var pr pullRecord
		tuple, control, pr, err = a.candidateTuple(ctx, pull, false)
		if err != nil {
			return out, err
		}
		if candidateID(tuple) != record.ID {
			return out, fmt.Errorf("frozen candidate request changed")
		}
		if _, err = a.candidateReviews(ctx, control, tuple.Request, pr, a.clock()); err != nil {
			return out, err
		}
	}
	if operation == "candidate-prepare" {
		err = s.transaction(ctx, func(conn *sql.Conn) error {
			if err := candidateFresh(s, a, control, record, false); err != nil {
				return err
			}
			if err := candidateCurrent(ctx, conn, record); err != nil {
				return err
			}
			existing, e := candidateIntent(ctx, conn, record)
			if e == nil {
				if existing.Status == "retired" {
					return fmt.Errorf("candidate intent retired")
				}
				return nil
			}
			if e != sql.ErrNoRows {
				return e
			}
			if record.Status != "frozen" || record.Evidence != nil {
				return fmt.Errorf("candidate proof already verified")
			}
			if e = candidateIDAvailable(ctx, conn, tuple.Request.Sweep.ID, record.ID); e != nil {
				return e
			}
			if e = nonCandidateIDAvailable(ctx, conn, tuple.Request.Sweep.ID); e != nil {
				return e
			}
			raw, _ := json.Marshal(tuple.Request.Sweep)
			_, e = conn.ExecContext(ctx, "INSERT INTO candidate_intent VALUES(?,?,?,'prepared',0,0)", record.ID, tuple.Request.Sweep.ID, raw)
			return e
		})
		return out, err
	}
	i, err := candidateIntent(ctx, s.db, record)
	if err != nil {
		return out, err
	}
	if i.Status == "retired" {
		return out, fmt.Errorf("candidate intent retired")
	}
	// Discovery precedes any send, including adoption of an already existing run.
	id, err := a.discoverRun(ctx, control, i.Sweep)
	if err != nil {
		return out, err
	}
	closureVerified := false
	if id == 0 && i.Status == "prepared" && operation == "candidate-pump" {
		prepared, e := a.prepareDispatch(ctx, control, i.Sweep)
		if e != nil {
			return out, e
		}
		closureVerified = true
		// Detect changes during remote preflight. This is observational, not an
		// atomic GitHub both-ref/review transaction and never permits promotion.
		if e = a.candidateDispatchReady(ctx, control, tuple.Request); e != nil {
			return out, e
		}
		if e = a.candidateFrozenRef(ctx, tuple.Request); e != nil {
			return out, e
		}
		claim := false
		e = s.transaction(ctx, func(conn *sql.Conn) error {
			if e := candidateFresh(s, a, control, record, false); e != nil {
				return e
			}
			if e := candidateCurrent(ctx, conn, record); e != nil {
				return e
			}
			current, e := candidateIntent(ctx, conn, record)
			if e != nil {
				return e
			}
			if current.Status != "prepared" {
				return nil
			}
			row, e := conn.ExecContext(ctx, "UPDATE candidate_intent SET status='uncertain' WHERE candidate_id=? AND status='prepared'", record.ID)
			if e != nil {
				return e
			}
			n, e := row.RowsAffected()
			claim = e == nil && n == 1
			return e
		})
		if e != nil {
			return out, e
		}
		if claim {
			// The durable claim precedes the POST. Any outcome, including a lost
			// response or crash before the POST, remains uncertain; no blind resend.
			if e = a.dispatch(prepared); e != nil {
				return out, fmt.Errorf("%w: candidate dispatch", ErrAmbiguous)
			}
		}
		id, err = a.discoverRun(ctx, control, i.Sweep)
		if err != nil {
			return out, err
		}
	}
	if id == 0 {
		return out, ErrAmbiguous
	}
	if !closureVerified {
		if err = a.executionClosure(ctx, control, i.Sweep.Target); err != nil {
			return out, err
		}
	}
	if !history {
		if err = a.candidateFrozenRef(ctx, tuple.Request); err != nil {
			return out, err
		}
	}
	if _, err = a.candidateDispatchRun(ctx, control, tuple.Request, id); err != nil {
		return out, err
	}
	err = s.transaction(ctx, func(conn *sql.Conn) error {
		if e := candidateFresh(s, a, control, record, history); e != nil {
			return e
		}
		if e := candidateCurrent(ctx, conn, record); e != nil {
			return e
		}
		current, e := candidateIntent(ctx, conn, record)
		if e != nil {
			return e
		}
		if current.Status == "bound" {
			if current.RunID != id || current.Attempt != 1 {
				return fmt.Errorf("candidate original run changed")
			}
			return nil
		}
		if current.Status != "prepared" && current.Status != "uncertain" {
			return ErrConflict
		}
		if record.Evidence != nil && (record.Evidence.Run != id || record.Evidence.Attempt != 1) {
			return fmt.Errorf("candidate evidence and dispatch differ")
		}
		_, e = conn.ExecContext(ctx, "UPDATE candidate_intent SET status='bound',run=?,attempt=1 WHERE candidate_id=? AND status IN ('prepared','uncertain')", id, record.ID)
		return e
	})
	return out, err
}

// Reserve IDs against retained tuples as well as outboxes. Legacy evidence has
// no outbox, but must not make an invalidated proof reusable by a new request.
func candidateIDAvailable(ctx context.Context, conn *sql.Conn, intent, current string) error {
	var reserved int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM candidate_intent WHERE intent=? AND candidate_id!=?", intent, current).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 0 {
		return fmt.Errorf("candidate proof intent ID already used")
	}
	rows, err := conn.QueryContext(ctx, "SELECT id,tuple FROM candidate WHERE id!=?", current)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		var tuple CandidateTuple
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		if json.Unmarshal(raw, &tuple) != nil || candidateID(tuple) != id {
			return fmt.Errorf("candidate history seal mismatch")
		}
		if tuple.Request.Sweep.ID == intent {
			return fmt.Errorf("candidate proof intent ID already used")
		}
	}
	return rows.Err()
}

func nonCandidateIDAvailable(ctx context.Context, conn *sql.Conn, id string) error {
	var reused int
	if err := conn.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM intent WHERE id=?)+(SELECT count(*) FROM bootstrap_archive WHERE intent=?)", id, id).Scan(&reused); err != nil {
		return err
	}
	_, boot, err := bootstrapIntent(ctx, conn)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if reused != 0 || (err == nil && boot.Sweep.ID == id) {
		return fmt.Errorf("proof intent ID already used")
	}
	return nil
}
