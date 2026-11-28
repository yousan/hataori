package cipolicy

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrConflict    = errors.New("coordinator revision conflict")
	ErrInitialized = errors.New("lane already initialized; reset forbidden")
	ErrAmbiguous   = errors.New("dispatch ambiguous; reconcile without another POST")
)

const applicationID = 0x48434931

type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// OpenStore never replaces an existing file or accepts an unrelated database.
// Callers select a dedicated coordinator-owned path outside candidate checkouts.
func OpenStore(file string) (*Store, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	fresh := err == nil
	if fresh {
		if err = f.Close(); err != nil {
			return nil, err
		}
	} else if !os.IsExist(err) {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("store must be a private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) || stat.Nlink != 1 {
		return nil, fmt.Errorf("untrusted store owner or alias")
	}
	u := url.URL{Scheme: "file", Path: abs}
	if !fresh {
		q := url.Values{"mode": {"ro"}}
		u.RawQuery = q.Encode()
		probe, err := sql.Open("sqlite", u.String())
		if err != nil {
			return nil, err
		}
		var id int
		err = probe.QueryRow("PRAGMA application_id").Scan(&id)
		probe.Close()
		if err != nil || id != applicationID {
			return nil, fmt.Errorf("unrecognized database; no overwrite")
		}
	}
	q := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(FULL)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, clock: time.Now}
	if fresh {
		err = s.transaction(context.Background(), func(c *sql.Conn) error {
			for _, statement := range []string{
				fmt.Sprintf("PRAGMA application_id=%d", applicationID),
				`CREATE TABLE lane (id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, repo INTEGER NOT NULL, ref TEXT NOT NULL, authority TEXT NOT NULL, state BLOB NOT NULL, history BLOB NOT NULL)`,
				`CREATE TABLE intent (id TEXT PRIMARY KEY, sweep BLOB NOT NULL, status TEXT NOT NULL CHECK(status IN ('prepared','uncertain','bound','done','retired')), run INTEGER NOT NULL DEFAULT 0, attempt INTEGER NOT NULL DEFAULT 0)`,
				`CREATE UNIQUE INDEX one_active_intent ON intent((1)) WHERE status NOT IN ('done','retired')`,
			} {
				if _, err := c.ExecContext(context.Background(), statement); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	// Compatible additive schema: never replace an existing lane or debt.
	if err := s.transaction(context.Background(), func(c *sql.Conn) error {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS admission (id INTEGER PRIMARY KEY, pull INTEGER NOT NULL, head TEXT NOT NULL, base TEXT NOT NULL, grant_digest TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('reserved','integrated','withdrawn')), expires TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS admission_grant (pull INTEGER NOT NULL, head TEXT NOT NULL, base TEXT NOT NULL, digest TEXT NOT NULL, expires TEXT NOT NULL, PRIMARY KEY(pull,head,base,digest))`,
			`CREATE UNIQUE INDEX IF NOT EXISTS one_admission ON admission((1)) WHERE status='reserved'`,
			`CREATE TABLE IF NOT EXISTS bootstrap_intent (id INTEGER PRIMARY KEY CHECK(id=1), seal TEXT NOT NULL, sweep BLOB NOT NULL, status TEXT NOT NULL CHECK(status IN ('prepared','uncertain','bound')), run INTEGER NOT NULL, attempt INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS recovery (revision INTEGER PRIMARY KEY, owner TEXT NOT NULL, reason TEXT NOT NULL, control TEXT NOT NULL, authority TEXT NOT NULL, watermark TEXT NOT NULL, at TEXT NOT NULL)`,
		} {
			if _, err := c.ExecContext(context.Background(), q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) transaction(ctx context.Context, fn func(*sql.Conn) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	if err = fn(c); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

func (s *Store) Initialize(ctx context.Context, b Baseline) error {
	return s.transaction(ctx, func(c *sql.Conn) error {
		now := s.clock()
		if b.repositoryID <= 0 || b.ref == "" || !digestOK(b.key) || b.control.revision == "" || b.control.selection.Report(now).FullOnly || !now.Before(b.expires) || now.Before(b.at) || now.Sub(b.at) > 30*time.Second {
			return fmt.Errorf("verified bootstrap required")
		}
		seal, intent, proofErr := bootstrapIntent(ctx, c)
		if proofErr != nil && proofErr != sql.ErrNoRows {
			return proofErr
		}
		if proofErr == nil && (b.proofSeal != seal || intent.Status != "bound" || intent.Sweep.Target != b.target) {
			return fmt.Errorf("bootstrap outbox proof required")
		}
		state, err := NewState(b.control.selection, b.target)
		if err != nil {
			return err
		}
		state.Authority = b.control.authority
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		history, _ := json.Marshal(History{Root: b.target})
		var count int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM lane").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrInitialized
		}
		_, err = c.ExecContext(ctx, "INSERT INTO lane VALUES(1,0,?,?,?,?,?)", b.repositoryID, b.ref, b.key, raw, history)
		return err
	})
}

type Snapshot struct {
	seal     string
	Version  int64
	State    State
	Intent   *Intent
	History  History
	repo     int64
	ref, key string
}
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func load(ctx context.Context, q queryer) (Snapshot, error) {
	var snap Snapshot
	var raw, history []byte
	if err := q.QueryRowContext(ctx, "SELECT revision,repo,ref,authority,state,history FROM lane WHERE id=1").Scan(&snap.Version, &snap.repo, &snap.ref, &snap.key, &raw, &history); err != nil {
		return snap, err
	}
	if err := json.Unmarshal(raw, &snap.State); err != nil {
		return snap, err
	}
	if err := json.Unmarshal(history, &snap.History); err != nil {
		return snap, err
	}
	if snap.State.Active != nil {
		var intent Intent
		var b []byte
		err := q.QueryRowContext(ctx, "SELECT sweep,status,run,attempt FROM intent WHERE id=? AND status IN ('prepared','uncertain','bound')", snap.State.Active.ID).Scan(&b, &intent.Status, &intent.RunID, &intent.Attempt)
		if err != nil {
			return snap, err
		}
		if err = json.Unmarshal(b, &intent.Sweep); err != nil {
			return snap, err
		}
		if intent.Sweep != *snap.State.Active {
			return snap, fmt.Errorf("intent/state identity mismatch")
		}
		snap.Intent = &intent
	}
	snap.seal = snapshotSeal(snap)
	return snap, nil
}
func (s *Store) Read(ctx context.Context) (Snapshot, error) { return load(ctx, s.db) }

// Advance serializes fresh admission, clock/expiry, debt, completion and outbox
// creation in one durable CAS. There is no public arbitrary-state write/reset.
func (s *Store) Advance(ctx context.Context, version int64, o Observation) (Decision, error) {
	var decision Decision
	err := s.transaction(ctx, func(c *sql.Conn) error {
		snap, err := load(ctx, c)
		if err != nil {
			return err
		}
		if snap.Version != version {
			return ErrConflict
		}
		if o.repositoryID != snap.repo || o.ref != snap.ref || o.key != snap.key || o.control.revision == "" || o.control.authority != snap.State.Authority || !s.clock().Before(o.expires) || s.clock().Before(o.event.Now) || s.clock().Sub(o.event.Now) > 30*time.Second {
			return fmt.Errorf("unverified lane authority")
		}
		if o.event.Completion != nil {
			if snap.Intent == nil || snap.Intent.Status != "bound" || snap.Intent.RunID != o.runID || snap.Intent.Attempt != o.attempt {
				return fmt.Errorf("completion is not bound to durable intent")
			}
		}
		event := o.event
		event.Now = s.clock()
		uncovered := snap.State.Watermark == event.History.Root
		for _, commit := range event.History.Commits {
			if uncovered && commit.HighRisk {
				event.Force = true
			}
			if commit.SHA == snap.State.Watermark {
				uncovered = true
			}
		}
		next, d, err := Reconcile(snap.State, o.control.selection, event)
		if err != nil {
			return err
		}
		decision = d
		if event.Recovery != nil {
			if _, err := c.ExecContext(ctx, "INSERT INTO recovery VALUES(?,?,?,?,?,?,?)", version+1, event.Recovery.Owner, event.Recovery.Reason, snap.State.Control, snap.State.Authority, snap.State.Watermark, event.Now.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		if o.event.Completion != nil {
			if _, err := c.ExecContext(ctx, "UPDATE intent SET status='done' WHERE id=? AND status='bound'", o.event.Completion.Sweep.ID); err != nil {
				return err
			}
		}
		if d.Dispatch != nil {
			var nonce [32]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return err
			}
			next.Active.ID += "-" + hex.EncodeToString(nonce[:])
			*d.Dispatch = *next.Active
			decision.Dispatch = d.Dispatch
			raw, err := json.Marshal(next.Active)
			if err != nil {
				return err
			}
			if _, err = c.ExecContext(ctx, "INSERT INTO intent(id,sweep,status) VALUES(?,?,'prepared')", next.Active.ID, raw); err != nil {
				return err
			}
		}
		if snap.Intent != nil && snap.Intent.Status == "uncertain" && o.event.Completion == nil {
			decision.AllowCode = false
			decision.Reason = "dispatch is ambiguous"
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		history, err := json.Marshal(o.event.History)
		if err != nil {
			return err
		}
		if version == 1<<63-1 {
			return fmt.Errorf("store revision exhausted")
		}
		result, err := c.ExecContext(ctx, "UPDATE lane SET revision=revision+1,state=?,history=? WHERE id=1 AND revision=?", raw, history, version)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		decision.AllowCode = false
		decision.Dispatch = nil
		decision.Reason = err.Error()
	}
	return decision, err
}

// Pump persists uncertainty before crossing the external side-effect boundary.
// There is intentionally no lease timeout that blindly repeats a POST.
func (s *Store) Pump(ctx context.Context, a *Adapter) error {
	ctx = a.budget(ctx)
	snap, err := s.Read(ctx)
	if err != nil {
		return err
	}
	if snap.Intent == nil || snap.Intent.Status == "bound" {
		return nil
	}
	c, err := a.control(ctx)
	if err != nil {
		return err
	}
	if a.keyDigest != snap.key || a.root.RepositoryID != snap.repo || a.root.IntegrationRef != snap.ref || c.revision != snap.State.Control || c.selection.digest != snap.State.Digest || c.authority != snap.State.Authority {
		return fmt.Errorf("dispatch authority changed")
	}
	intent := *snap.Intent
	if intent.Status == "prepared" && c.selection.Report(s.clock()).FullOnly {
		return fmt.Errorf("dispatch policy expired or not active")
	}
	var prepared dispatchRequest
	if intent.Status == "prepared" {
		prepared, err = a.prepareDispatch(ctx, c, intent.Sweep)
		if err != nil {
			return err
		}
	}
	claim := false
	err = s.transaction(ctx, func(conn *sql.Conn) error {
		current, err := load(ctx, conn)
		if err != nil {
			return err
		}
		if current.Intent == nil || current.Intent.Sweep != intent.Sweep {
			return ErrConflict
		}
		if current.Intent.Status == "prepared" {
			if !s.clock().Before(a.root.ExpiresAt) || c.selection.Report(s.clock()).FullOnly {
				return fmt.Errorf("dispatch authority expired")
			}
			r, err := conn.ExecContext(ctx, "UPDATE intent SET status='uncertain' WHERE id=? AND status='prepared'", intent.Sweep.ID)
			if err != nil {
				return err
			}
			n, err := r.RowsAffected()
			claim = err == nil && n == 1
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if claim {
		if err := a.dispatch(prepared); err != nil {
			return fmt.Errorf("%w: %v", ErrAmbiguous, err)
		}
	}
	run, err := a.find(ctx, c, intent.Sweep)
	if err != nil {
		return err
	}
	if run == 0 {
		return ErrAmbiguous
	}
	return s.transaction(ctx, func(conn *sql.Conn) error {
		current, err := load(ctx, conn)
		if err != nil {
			return err
		}
		if current.Intent == nil || current.Intent.Sweep != intent.Sweep {
			return ErrConflict
		}
		if current.Intent.Status == "bound" {
			if current.Intent.RunID != run {
				return fmt.Errorf("multiple runs; quarantine")
			}
			return nil
		}
		_, err = conn.ExecContext(ctx, "UPDATE intent SET status='bound',run=?,attempt=1 WHERE id=? AND status='uncertain'", run, intent.Sweep.ID)
		return err
	})
}

// SwitchControl never reinitializes at a new head. It preserves all observed
// debt and failure, and invalidates old-scope coverage back to the fixed root.
// Uncertain/bound proofs must finish under their frozen control. A definitely
// unsent prepared intent may be retired atomically; its debt is preserved.
func (s *Store) SwitchControl(ctx context.Context, version int64, o Observation) error {
	return s.transaction(ctx, func(c *sql.Conn) error {
		snap, err := load(ctx, c)
		if err != nil {
			return err
		}
		if snap.Version != version {
			return ErrConflict
		}
		if (snap.State.Active != nil && (snap.Intent == nil || snap.Intent.Status != "prepared")) || o.key != snap.key || o.repositoryID != snap.repo || o.ref != snap.ref || o.control.selection.Report(s.clock()).FullOnly || !s.clock().Before(o.expires) || s.clock().Before(o.event.Now) || s.clock().Sub(o.event.Now) > 30*time.Second {
			return fmt.Errorf("unsafe control transition")
		}
		positions, err := validateHistory(o.event.History, o.event.Now)
		if err != nil {
			return err
		}
		if o.event.History.Root != snap.State.Root || o.event.Now.Before(snap.State.ObservedAt) {
			return fmt.Errorf("control switch cannot reset history")
		}
		for _, old := range snap.History.Commits {
			idx, ok := positions[old.SHA]
			if !ok || idx < 0 {
				return fmt.Errorf("control switch omitted known debt")
			}
			current := &o.event.History.Commits[idx]
			if current.Parent != old.Parent || !current.IntegratedAt.Equal(old.IntegratedAt) {
				return fmt.Errorf("control switch rewrote merge history")
			}
			current.Code = current.Code || old.Code
			current.Compatibility = current.Compatibility || old.Compatibility
			current.HighRisk = current.HighRisk || old.HighRisk
		}
		if snap.Intent != nil {
			if _, err := c.ExecContext(ctx, "UPDATE intent SET status='retired' WHERE id=? AND status='prepared'", snap.Intent.Sweep.ID); err != nil {
				return err
			}
		}
		next := snap.State
		next.Active = nil
		// Retiring unsent proof cannot turn diagnosis into validated recovery.
		next.Recovering = false
		next.Authority = o.control.authority
		next.Control = o.control.revision
		next.Digest = o.control.selection.digest
		next.Watermark = next.Root
		next.Pending = nil
		next.ObservedTip = o.event.History.Root
		if len(o.event.History.Commits) > 0 {
			next.ObservedTip = o.event.History.Commits[len(o.event.History.Commits)-1].SHA
		}
		next.ObservedDigest, err = historyDigest(o.event.History, len(o.event.History.Commits)-1)
		if err != nil {
			return err
		}
		next.ObservedAt = s.clock()
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		h, err := json.Marshal(o.event.History)
		if err != nil {
			return err
		}
		if version == 1<<63-1 {
			return fmt.Errorf("store revision exhausted")
		}
		_, err = c.ExecContext(ctx, "UPDATE lane SET revision=revision+1,state=?,history=? WHERE id=1 AND revision=?", raw, h, version)
		return err
	})
}

func snapshotSeal(s Snapshot) string {
	b, err := json.Marshal(struct {
		Version  int64
		State    State
		History  History
		Repo     int64
		Ref, Key string
		Intent   *Intent
	}{s.Version, s.State, s.History, s.repo, s.ref, s.key, s.Intent})
	if err != nil {
		return ""
	}
	return Digest(b)
}
