package cipolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type integrationIntent struct {
	ID        string
	Write     integrationWrite
	Status    string
	ExpiresAt time.Time
}

func integrationIntentAt(ctx context.Context, q queryer, pull int) (integrationIntent, error) {
	var i integrationIntent
	var raw []byte
	var expiry string
	err := q.QueryRowContext(ctx, "SELECT id,record,status,expires FROM integration_intent WHERE pull=? ORDER BY rowid DESC LIMIT 1", pull).Scan(&i.ID, &raw, &i.Status, &expiry)
	if err != nil {
		return i, err
	}
	if err = json.Unmarshal(raw, &i.Write); err != nil {
		return i, err
	}
	i.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiry)
	return i, err
}

func (s *Store) prepareIntegration(ctx context.Context, snap Snapshot, w integrationWrite, g RouteGrant, p Selection, expires time.Time) (integrationIntent, error) {
	raw, _ := json.Marshal(w)
	i := integrationIntent{ID: Digest(raw), Write: w, Status: "prepared", ExpiresAt: g.ExpiresAt}
	if expires.Before(i.ExpiresAt) {
		i.ExpiresAt = expires
	}
	err := s.transaction(ctx, func(c *sql.Conn) error {
		current, err := load(ctx, c)
		if err != nil {
			return err
		}
		if current.Version != snap.Version {
			return ErrConflict
		}
		_, d, err := Reconcile(current.State, p, Event{Now: s.clock(), History: current.History})
		if err != nil || !d.AllowCode || current.State.ObservedTip != w.Base || !s.clock().Before(i.ExpiresAt) {
			return fmt.Errorf("integration admission expired or closed")
		}
		var grant string
		if err = c.QueryRowContext(ctx, "SELECT grant_digest FROM admission WHERE status='reserved' AND pull=? AND base=? AND head=?", w.Pull, w.Base, w.Head).Scan(&grant); err != nil || grant != w.Grant {
			return fmt.Errorf("exact reservation missing")
		}
		existing, err := integrationIntentAt(ctx, c, w.Pull)
		if err == nil {
			if existing.ID == i.ID {
				i = existing
				return nil
			}
			if existing.Status != "prepared" && existing.Status != "retired" {
				return fmt.Errorf("integration identity changed; retain intent")
			}
			if _, err = c.ExecContext(ctx, "UPDATE integration_intent SET status='retired' WHERE id=? AND status='prepared'", existing.ID); err != nil {
				return err
			}
			err = sql.ErrNoRows
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = c.ExecContext(ctx, "INSERT INTO integration_intent(id,pull,record,status,expires) VALUES(?,?,?,'prepared',?)", i.ID, w.Pull, raw, i.ExpiresAt.UTC().Format(time.RFC3339Nano))
		return err
	})
	return i, err
}

func (s *Store) claimIntegration(ctx context.Context, version int64, i integrationIntent, p Selection, expires time.Time) (bool, error) {
	claim := false
	err := s.transaction(ctx, func(c *sql.Conn) error {
		snap, err := load(ctx, c)
		if err != nil {
			return err
		}
		if snap.Version != version {
			return ErrConflict
		}
		current, err := integrationIntentAt(ctx, c, i.Write.Pull)
		if err != nil {
			return err
		}
		if current != i {
			return ErrConflict
		}
		var grant, expiry string
		if err = c.QueryRowContext(ctx, "SELECT grant_digest,expires FROM admission WHERE status='reserved' AND pull=? AND base=? AND head=?", i.Write.Pull, i.Write.Base, i.Write.Head).Scan(&grant, &expiry); err != nil || grant != i.Write.Grant {
			return fmt.Errorf("exact live reservation changed")
		}
		deadline, err := time.Parse(time.RFC3339Nano, expiry)
		if err != nil || !s.clock().Before(deadline) {
			return fmt.Errorf("reservation expired")
		}
		_, d, err := Reconcile(snap.State, p, Event{Now: s.clock(), History: snap.History})
		if err != nil || !d.AllowCode || !s.clock().Before(i.ExpiresAt) || !s.clock().Before(expires) || snap.State.ObservedTip != i.Write.Base || snap.State.Control != i.Write.Control || snap.State.Authority != i.Write.Authority || snap.key != i.Write.Key {
			return fmt.Errorf("transaction-time integration closed")
		}
		if i.Status != "prepared" {
			return nil
		}
		r, err := c.ExecContext(ctx, "UPDATE integration_intent SET status='uncertain' WHERE id=? AND status='prepared'", i.ID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		claim = err == nil && n == 1
		return err
	})
	return claim, err
}

// Reconciliation requires provider PR merge recognition and the exact admitted
// merge object in immutable observed history. A raw ref success alone is not Done.
func (c *Coordinator) reconcileIntegration(ctx context.Context, i integrationIntent) error {
	a := c.adapter
	if i.Write.Key != a.keyDigest || i.Write.Control != a.root.CurrentControl || i.Write.Authority != a.authorityDigest() {
		return fmt.Errorf("integration authority changed; retain receipt")
	}
	var p pullRecord
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", a.prefix(), i.Write.Pull), nil, &p); err != nil {
		return err
	}
	w := i.Write
	if p.Number != w.Pull || !p.Merged || p.State != "closed" || p.Head.Repo.ID != a.root.RepositoryID || p.Base.Repo.ID != a.root.RepositoryID || p.Head.Ref != w.Source || p.Head.SHA != w.Head || p.Base.Ref != w.Ref || p.MergeSHA != w.Merge || p.MergedAt.IsZero() || !p.MergedAt.Before(i.ExpiresAt) {
		return ErrAmbiguous
	}
	snap, err := c.store.Read(ctx)
	if err != nil {
		return err
	}
	observation, err := a.Observe(ctx, snap)
	if err != nil {
		return err
	}
	h := observation.event.History
	found := false
	for _, commit := range h.Commits {
		if commit.SHA == w.Merge && commit.Parent == w.Base && commit.IntegratedAt.Equal(p.MergedAt) {
			found = true
		}
	}
	if !found {
		return ErrAmbiguous
	}
	return c.store.transaction(ctx, func(conn *sql.Conn) error {
		current, err := integrationIntentAt(ctx, conn, w.Pull)
		if err != nil {
			return err
		}
		if current.ID != i.ID || (current.Status != "uncertain" && current.Status != "confirmed") {
			return ErrConflict
		}
		_, err = conn.ExecContext(ctx, "UPDATE integration_intent SET status='confirmed' WHERE id=? AND status='uncertain'", i.ID)
		return err
	})
}

func (s *Store) integrationReceipt(ctx context.Context, pull int, merge string) error {
	i, err := integrationIntentAt(ctx, s.db, pull)
	if err != nil || i.Status != "confirmed" || i.Write.Merge != merge {
		return fmt.Errorf("matching admitted integration write receipt missing")
	}
	return nil
}

// integrate is a callable owner path, not a generic merge command. The signed
// opt-in and fresh reviews/consent are required in addition to both-ref CAS.
// CAS does not atomically lock GitHub review revocation/PR metadata or time.
func (c *Coordinator) integrate(ctx context.Context, pull int) (Outcome, error) {
	out := Outcome{Operation: "integrate", RoutingEnabled: c.adapter.root.RoutingEnabled}
	ctx = c.adapter.budget(ctx)
	existing, err := integrationIntentAt(ctx, c.store.db, pull)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	if err == nil && existing.Status != "prepared" && existing.Status != "retired" {
		if err = c.reconcileIntegration(ctx, existing); err != nil {
			return out, err
		}
		out, err = c.execute(ctx, "observe", 0, 0, "")
		out.Operation = "integrate"
		if err == nil {
			out.Route = &RouteResult{Pull: pull, Route: "integration", Allowed: true, Integrated: true, Reason: "admitted write recognized; complete coverage still required for Done"}
		}
		return out, err
	}
	admission, err := c.execute(ctx, "admission", pull, 0, "")
	out = admission
	out.Operation = "integrate"
	if err != nil {
		return out, err
	}
	if out.Route == nil || !out.Route.Allowed || out.Route.Route != "integration" {
		return out, fmt.Errorf("owner not admitted to integration")
	}
	// A permit is never returned across the write boundary; failures stay closed.
	out.Route.Allowed = false
	a := c.adapter
	g := a.root.Routes[pull]
	snap, err := c.store.Read(ctx)
	if err != nil {
		return out, err
	}
	control, err := a.control(ctx)
	if err != nil {
		return out, err
	}
	w, err := a.prepareIntegration(ctx, pull, g)
	if err != nil {
		return out, err
	}
	i, err := c.store.prepareIntegration(ctx, snap, w, g, control.selection, a.root.ExpiresAt)
	if err != nil {
		return out, err
	}
	// Refresh mutable authorization immediately before claiming the one write.
	r, err := a.route(ctx, snap, pull, false, admission.Decision)
	if err != nil || !r.Allowed {
		return out, fmt.Errorf("fresh owner authorization missing")
	}
	fresh, err := a.prepareIntegration(ctx, pull, g)
	if err != nil || fresh != w {
		return out, fmt.Errorf("integration identity changed before write")
	}
	claim, err := c.store.claimIntegration(ctx, snap.Version, i, control.selection, a.root.ExpiresAt)
	if err != nil {
		return out, err
	}
	if !claim {
		return out, ErrAmbiguous
	}
	if err = a.writeIntegration(ctx, w); err != nil {
		return out, fmt.Errorf("%w: integration outcome unknown or rejected", ErrAmbiguous)
	}
	i.Status = "uncertain"
	if err = c.reconcileIntegration(ctx, i); err != nil {
		return out, err
	}
	out, err = c.execute(ctx, "observe", 0, 0, "")
	out.Operation = "integrate"
	if err == nil {
		out.Route = &RouteResult{Pull: pull, Route: "integration", Allowed: true, Integrated: true, Reason: "admitted write recognized; complete coverage still required for Done"}
	}
	return out, err
}
