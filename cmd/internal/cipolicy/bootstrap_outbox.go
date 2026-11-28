package cipolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// bootstrapSeal freezes the signed request independently of authorityDigest,
// which deliberately allows authority expiry renewal without resetting debt.
func (a *Adapter) bootstrapSeal() string {
	b, _ := json.Marshal(struct {
		Repository          int64
		Ref, Key, Authority string
		Proof               BootstrapRequirement
	}{a.root.RepositoryID, a.root.IntegrationRef, a.keyDigest, a.authorityDigest(), a.root.Bootstrap})
	return Digest(b)
}

func (a *Adapter) bootstrapControl(ctx context.Context) (verifiedControl, error) {
	c, err := a.control(ctx)
	if err != nil {
		return c, err
	}
	b := a.root.Bootstrap
	if !revisionOK(b.Target) || b.Sweep.Target != b.Target || !b.Sweep.CompatibilityRequired || b.Sweep.ID == "" || !revisionOK(b.Sweep.From) || b.Attempt != 1 || b.RunID < 0 || b.Sweep.Control != c.revision || b.Sweep.Digest != c.selection.digest || b.Sweep.Authority != c.authority || c.selection.Report(a.clock()).FullOnly {
		return c, fmt.Errorf("invalid or expired fixed bootstrap request")
	}
	return c, nil
}

func bootstrapIntent(ctx context.Context, q queryer) (string, Intent, error) {
	var seal string
	var i Intent
	var raw []byte
	err := q.QueryRowContext(ctx, "SELECT seal,sweep,status,run,attempt FROM bootstrap_intent WHERE id=1").Scan(&seal, &raw, &i.Status, &i.RunID, &i.Attempt)
	if err != nil {
		return seal, i, err
	}
	err = json.Unmarshal(raw, &i.Sweep)
	return seal, i, err
}

// PrepareBootstrap creates a durable outbox BEFORE any baseline/lane exists.
// It grants no watermark, admission, Done or release permission.
func (s *Store) PrepareBootstrap(ctx context.Context, a *Adapter) (Intent, error) {
	ctx = a.budget(ctx)
	c, err := a.bootstrapControl(ctx)
	if err != nil {
		return Intent{}, err
	}
	var i Intent
	err = s.transaction(ctx, func(conn *sql.Conn) error {
		var lanes int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM lane").Scan(&lanes); err != nil {
			return err
		}
		if lanes != 0 {
			return ErrInitialized
		}
		if err := candidateIDAvailable(ctx, conn, a.root.Bootstrap.Sweep.ID, ""); err != nil {
			return err
		}
		seal, existing, err := bootstrapIntent(ctx, conn)
		if err == nil {
			if seal != a.bootstrapSeal() {
				return fmt.Errorf("bootstrap request changed; retain existing proof")
			}
			i = existing
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if !s.clock().Before(a.root.ExpiresAt) || c.selection.Report(s.clock()).FullOnly {
			return fmt.Errorf("bootstrap authority expired")
		}
		b := a.root.Bootstrap
		i = Intent{Sweep: b.Sweep, Status: "prepared"}
		if b.RunID > 0 {
			i.Status = "bound"
			i.RunID = b.RunID
			i.Attempt = 1
		}
		raw, _ := json.Marshal(i.Sweep)
		_, err = conn.ExecContext(ctx, "INSERT INTO bootstrap_intent VALUES(1,?,?,?,?,?)", a.bootstrapSeal(), raw, i.Status, i.RunID, i.Attempt)
		return err
	})
	return i, err
}

// PumpBootstrap uses the same fixed-ref/provenance contract as ordinary sweeps.
// Once uncertain, a restart ONLY reconciles; it never repeats the dispatch.
func (s *Store) PumpBootstrap(ctx context.Context, a *Adapter) (Intent, error) {
	ctx = a.budget(ctx)
	c, err := a.bootstrapControl(ctx)
	if err != nil {
		return Intent{}, err
	}
	seal, i, err := bootstrapIntent(ctx, s.db)
	if err != nil {
		return i, err
	}
	if seal != a.bootstrapSeal() {
		return i, fmt.Errorf("bootstrap request changed")
	}
	if i.Status == "bound" {
		return i, nil
	}
	var prepared dispatchRequest
	if i.Status == "prepared" {
		prepared, err = a.prepareDispatch(ctx, c, i.Sweep)
		if err != nil {
			return i, err
		}
	}
	claim := false
	err = s.transaction(ctx, func(conn *sql.Conn) error {
		currentSeal, current, err := bootstrapIntent(ctx, conn)
		if err != nil {
			return err
		}
		if currentSeal != seal || current.Sweep != i.Sweep {
			return ErrConflict
		}
		if current.Status != "prepared" {
			return nil
		}
		if !s.clock().Before(a.root.ExpiresAt) || c.selection.Report(s.clock()).FullOnly {
			return fmt.Errorf("bootstrap authority expired")
		}
		r, err := conn.ExecContext(ctx, "UPDATE bootstrap_intent SET status='uncertain' WHERE id=1 AND seal=? AND status='prepared'", seal)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		claim = err == nil && n == 1
		return err
	})
	if err != nil {
		return i, err
	}
	if claim {
		if err = a.dispatch(prepared); err != nil {
			i.Status = "uncertain"
			return i, fmt.Errorf("%w: bootstrap dispatch", ErrAmbiguous)
		}
	}
	run, err := a.find(ctx, c, i.Sweep)
	if err != nil {
		return i, err
	}
	if run == 0 {
		i.Status = "uncertain"
		return i, ErrAmbiguous
	}
	err = s.transaction(ctx, func(conn *sql.Conn) error {
		currentSeal, current, err := bootstrapIntent(ctx, conn)
		if err != nil {
			return err
		}
		if currentSeal != seal || current.Sweep != i.Sweep {
			return ErrConflict
		}
		if current.Status == "bound" {
			if current.RunID != run {
				return fmt.Errorf("multiple bootstrap runs; quarantine")
			}
			i = current
			return nil
		}
		if current.Status != "uncertain" {
			return ErrConflict
		}
		_, err = conn.ExecContext(ctx, "UPDATE bootstrap_intent SET status='bound',run=?,attempt=1 WHERE id=1 AND seal=? AND status='uncertain'", run, seal)
		i.Status = "bound"
		i.RunID = run
		i.Attempt = 1
		return err
	})
	return i, err
}

func (s *Store) bootstrap(ctx context.Context, a *Adapter) error {
	seal, i, err := bootstrapIntent(ctx, s.db)
	if err == sql.ErrNoRows { // Compatible import of a separately pinned exact run.
		b, err := a.Bootstrap(ctx)
		if err != nil {
			return err
		}
		return s.Initialize(ctx, b)
	}
	if err != nil {
		return err
	}
	if seal != a.bootstrapSeal() || i.Status != "bound" || i.Attempt != 1 {
		return fmt.Errorf("bootstrap proof not bound to fixed request")
	}
	b, err := a.bootstrapRun(ctx, i.RunID)
	if err != nil {
		return err
	}
	b.proofSeal = seal
	return s.Initialize(ctx, b)
}

// RecoverBootstrap replaces only a positively identified terminal failed run.
// It requires the authenticated responsible owner, a new signed request, and a
// retained audit of the old request. Running/unknown/successful/ambiguous sends
// cannot be replaced. It neither dispatches nor initializes a lane.
func (s *Store) RecoverBootstrap(ctx context.Context, a *Adapter, reason string) (Intent, error) {
	ctx = a.budget(ctx)
	c, err := a.bootstrapControl(ctx)
	if err != nil {
		return Intent{}, err
	}
	oldSeal, old, err := bootstrapIntent(ctx, s.db)
	if err != nil {
		return old, err
	}
	if old.Status != "bound" || old.Attempt != 1 || old.RunID <= 0 || old.Sweep.Control != c.revision || old.Sweep.Digest != c.selection.digest || old.Sweep.Authority != c.authority || oldSeal == a.bootstrapSeal() || old.Sweep.ID == a.root.Bootstrap.Sweep.ID || a.root.Bootstrap.RunID != 0 {
		return old, fmt.Errorf("new same-scope signed request and bound failed proof required")
	}
	// Prove the retained request was signed under this same key/scope, allowing
	// expiry renewal only. A new authority key cannot take over pre-lane debt.
	original := *a
	original.root.Bootstrap = BootstrapRequirement{Target: old.Sweep.Target, Attempt: 1, Sweep: old.Sweep}
	sameKey := original.bootstrapSeal() == oldSeal
	original.root.Bootstrap.RunID = old.RunID
	sameKey = sameKey || original.bootstrapSeal() == oldSeal
	if !sameKey {
		return old, fmt.Errorf("bootstrap recovery authority changed")
	}
	run, err := a.run(ctx, c, old.Sweep, old.RunID, old.Attempt)
	if err != nil {
		return old, err
	}
	if run.Status != "completed" {
		return old, fmt.Errorf("bootstrap run still unresolved")
	}
	switch run.Conclusion {
	case "failure", "timed_out", "cancelled", "startup_failure", "action_required":
	default:
		return old, fmt.Errorf("bootstrap run not positively failed")
	}
	observation := Observation{control: c, key: a.keyDigest, repositoryID: a.root.RepositoryID}
	if _, err = a.Diagnose(ctx, observation, reason); err != nil {
		return old, err
	}
	next := Intent{Sweep: a.root.Bootstrap.Sweep, Status: "prepared"}
	err = s.transaction(ctx, func(conn *sql.Conn) error {
		var lanes int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM lane").Scan(&lanes); err != nil {
			return err
		}
		if lanes != 0 {
			return ErrInitialized
		}
		seal, current, err := bootstrapIntent(ctx, conn)
		if err != nil {
			return err
		}
		if seal != oldSeal || current != old {
			return ErrConflict
		}
		if !s.clock().Before(a.root.ExpiresAt) || c.selection.Report(s.clock()).FullOnly {
			return fmt.Errorf("bootstrap recovery expired")
		}
		if err = candidateIDAvailable(ctx, conn, next.Sweep.ID, ""); err != nil {
			return err
		}
		var reused int
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM bootstrap_archive WHERE seal=? OR intent=?", a.bootstrapSeal(), next.Sweep.ID).Scan(&reused); err != nil {
			return err
		}
		if reused != 0 {
			return fmt.Errorf("archived bootstrap request cannot be reused")
		}
		audit, _ := json.Marshal(old)
		owner, _ := json.Marshal(c.requirement.Owner)
		if _, err = conn.ExecContext(ctx, "INSERT INTO bootstrap_archive VALUES(?,?,?,?,?,?)", seal, old.Sweep.ID, audit, owner, reason, s.clock().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		raw, _ := json.Marshal(next.Sweep)
		_, err = conn.ExecContext(ctx, "UPDATE bootstrap_intent SET seal=?,sweep=?,status='prepared',run=0,attempt=0 WHERE id=1 AND seal=?", a.bootstrapSeal(), raw, oldSeal)
		return err
	})
	return next, err
}
