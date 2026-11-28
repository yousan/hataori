package cipolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type reservation struct {
	Pull                      int
	Head, Base, Grant, Status string
	ExpiresAt                 time.Time
}

func grantDigest(g RouteGrant) string { b, _ := json.Marshal(g); return Digest(b) }
func (s *Store) reservation(ctx context.Context) (*reservation, error) {
	var r reservation
	var expiry string
	err := s.db.QueryRowContext(ctx, "SELECT pull,head,base,grant_digest,status,expires FROM admission WHERE status='reserved'").Scan(&r.Pull, &r.Head, &r.Base, &r.Grant, &r.Status, &expiry)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiry)
	return &r, err
}
func (s *Store) reserve(ctx context.Context, version int64, pull int, g RouteGrant, policy Selection, expires time.Time) error {
	return s.transaction(ctx, func(c *sql.Conn) error {
		snap, err := load(ctx, c)
		if err != nil {
			return err
		}
		if snap.Version != version {
			return ErrConflict
		}
		_, decision, checkErr := Reconcile(snap.State, policy, Event{Now: s.clock(), History: snap.History})
		if checkErr != nil || !decision.AllowCode || !s.clock().Before(expires) || (snap.Intent != nil && snap.Intent.Status == "uncertain") {
			return fmt.Errorf("transaction-time admission closed")
		}
		if snap.State.ObservedTip != g.Base || !s.clock().Before(g.ExpiresAt) {
			return fmt.Errorf("admission base/expiry changed")
		}
		var r reservation
		err = c.QueryRowContext(ctx, "SELECT pull,head,base,grant_digest FROM admission WHERE status='reserved'").Scan(&r.Pull, &r.Head, &r.Base, &r.Grant)
		audit := func() error {
			_, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO admission_grant VALUES(?,?,?,?,?)", pull, g.Head, g.Base, grantDigest(g), g.ExpiresAt.UTC().Format(time.RFC3339Nano))
			return err
		}
		if err == nil {
			if r.Pull == pull && r.Head == g.Head && r.Base == g.Base {
				if err := audit(); err != nil {
					return err
				}
				_, err := c.ExecContext(ctx, "UPDATE admission SET grant_digest=?,expires=? WHERE status='reserved' AND pull=? AND head=? AND base=? AND grant_digest=?", grantDigest(g), g.ExpiresAt.UTC().Format(time.RFC3339Nano), pull, g.Head, g.Base, r.Grant)
				return err
			}
			return fmt.Errorf("another integration reservation is unresolved")
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err := audit(); err != nil {
			return err
		}
		_, err = c.ExecContext(ctx, "INSERT INTO admission(pull,head,base,grant_digest,status,expires) VALUES(?,?,?,?,'reserved',?)", pull, g.Head, g.Base, grantDigest(g), g.ExpiresAt.UTC().Format(time.RFC3339Nano))
		return err
	})
}

// Only actual merged/closed PR evidence resolves a reservation. Expiry, root
// refresh or restart never releases an unknown possible merge automatically.
func (c *Coordinator) reconcileReservation(ctx context.Context, snap Snapshot) error {
	r, err := c.store.reservation(ctx)
	if err != nil || r == nil {
		return err
	}
	var p pullRecord
	if err = c.adapter.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", c.adapter.prefix(), r.Pull), nil, &p); err != nil {
		return err
	}
	if p.Number != r.Pull || p.Head.Repo.ID != snap.repo || p.Base.Repo.ID != snap.repo {
		return fmt.Errorf("reserved PR identity changed")
	}
	status := ""
	if !p.Merged && p.State == "closed" {
		status = "withdrawn"
	} else {
		if p.Head.SHA != r.Head || p.Base.SHA != r.Base || p.Base.Ref != snap.ref {
			return fmt.Errorf("reserved PR changed; diagnose without dropping reservation")
		}
		if p.Merged && !p.MergedAt.IsZero() {
			for _, commit := range snap.History.Commits {
				if commit.SHA == p.MergeSHA {
					status = "integrated"
				}
			}
		}
	}
	if status == "" {
		return nil
	}
	return c.store.transaction(ctx, func(conn *sql.Conn) error {
		current, err := load(ctx, conn)
		if err != nil {
			return err
		}
		if current.Version != snap.Version {
			return ErrConflict
		}
		_, err = conn.ExecContext(ctx, "UPDATE admission SET status=? WHERE status='reserved' AND pull=? AND head=? AND base=? AND grant_digest=?", status, r.Pull, r.Head, r.Base, r.Grant)
		return err
	})
}
