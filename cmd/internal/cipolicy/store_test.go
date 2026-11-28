package cipolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func storeFixture(t *testing.T) (*Store, *githubFixture, string, Baseline) {
	t.Helper()
	f := fixture(t)
	now := clock.Add(10 * time.Minute)
	f.adapter.clock = func() time.Time { return now }
	c, err := f.adapter.control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sweep := Sweep{ID: "signed baseline fixture", From: sha(99), Target: sha(100), Control: c.revision, Digest: c.selection.digest, Authority: c.authority, CompatibilityRequired: true}
	f.adapter.root.Bootstrap = BootstrapRequirement{Target: sha(100), RunID: f.run.ID, Attempt: 1, Sweep: sweep}
	f.mu.Lock()
	f.run.HeadSHA = sha(100)
	f.run.Title = sweepTitle(sweep)
	f.run.Event = "workflow_dispatch"
	f.run.Status = "completed"
	f.run.Conclusion = "success"
	f.mu.Unlock()
	b, err := f.adapter.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "coverage.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	s.clock = func() time.Time { return now }
	t.Cleanup(func() { s.Close() })
	if err := s.Initialize(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return s, f, file, b
}
func observed(t *testing.T, s *Store, f *githubFixture, count int) (Snapshot, Observation) {
	t.Helper()
	h := history(count, clock)
	f.mu.Lock()
	f.head = h.Root
	for _, c := range h.Commits {
		f.commits[c.SHA] = c
		f.head = c.SHA
	}
	f.mu.Unlock()
	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.adapter.Observe(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	return snap, o
}

func TestDurableCASConcurrentCoordinatorsAndCommitBeforeDispatchCrash(t *testing.T) {
	s, f, file, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	other, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = s.clock
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func(store *Store) { defer wg.Done(); _, err := store.Advance(ctx, snap.Version, o); results <- err }(store)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	committed, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Intent == nil || committed.Intent.Status != "prepared" || len(committed.Intent.Sweep.ID) < 64 {
		t.Fatal(committed)
	}
	// Crash/restart after COMMIT, before any external POST: the same intent
	// survives and exactly one coordinator can cross the POST boundary.
	restarted, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.clock = s.clock
	for _, store := range []*Store{other, restarted} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			err := store.Pump(ctx, f.adapter)
			if err != nil && !errors.Is(err, ErrAmbiguous) && !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}(store)
	}
	wg.Wait()
	final, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	if posts != 1 || final.Intent.Status != "bound" || final.Intent.Sweep != committed.Intent.Sweep {
		t.Fatal(posts, final.Intent)
	}
}

func TestLostResponseRestartReconcilesWithoutAnotherPOST(t *testing.T) {
	s, f, file, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lost = true
	f.delay = true
	f.mu.Unlock()
	if err := s.Pump(ctx, f.adapter); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	restarted, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.clock = s.clock
	if err := restarted.Pump(ctx, f.adapter); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	snap, o = observed(t, restarted, f, 2)
	d, err := restarted.Advance(ctx, snap.Version, o)
	if err != nil || d.AllowCode {
		t.Fatal("ambiguous dispatch allowed admission", d, err)
	}
	f.mu.Lock()
	f.delay = false
	f.lost = false
	f.mu.Unlock()
	if err := restarted.Pump(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	if posts != 1 {
		t.Fatal("lost response retried POST", posts)
	}
	snap, o = observed(t, restarted, f, 2)
	old := *o.event.Completion
	d, err = restarted.Advance(ctx, snap.Version, o)
	if err != nil || d.Uncovered != 1 {
		t.Fatal("older success erased newer debt", d, err)
	}
	latest, err := restarted.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State.Watermark != sha(101) {
		t.Fatal(latest.State)
	}
	// The already verified old completion cannot be replayed into a newer CAS.
	o.event.Completion = &old
	if _, err := restarted.Advance(ctx, latest.Version, o); err == nil {
		t.Fatal("old completion replay accepted")
	}
}

func TestPreflightFailureAndMovedIntegrationDoNotInventAmbiguousPOST(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.preflightFailure = true
	f.mu.Unlock()
	if err := s.Pump(ctx, f.adapter); err == nil || errors.Is(err, ErrAmbiguous) {
		t.Fatal("GET failure marked as ambiguous POST", err)
	}
	current, err := s.Read(ctx)
	if err != nil || current.Intent.Status != "prepared" {
		t.Fatal(current, err)
	}
	f.mu.Lock()
	f.preflightFailure = false
	f.mu.Unlock()
	snap, o = observed(t, s, f, 2)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	if err := s.Pump(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	target, posts := f.run.HeadSHA, f.posts
	f.mu.Unlock()
	if target != sha(101) || posts != 1 {
		t.Fatal("moving integration mutated frozen dispatch", target, posts)
	}
}

func TestCrashAtUncertainBoundaryQuarantinesWithoutBlindRetry(t *testing.T) {
	s, f, file, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after the send-intent commit but before a response
	// (possibly even before POST). External exactly-once is not assumed.
	if err := s.transaction(ctx, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, "UPDATE intent SET status='uncertain' WHERE status='prepared'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.clock = s.clock
	if err := restarted.Pump(ctx, f.adapter); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	if posts != 0 {
		t.Fatal("ambiguous send was blindly repeated")
	}
}

func TestTransactionClockFencesCachedExpiryAndDebt(t *testing.T) {
	for _, kind := range []string{"policy", "root", "stale", "age"} {
		t.Run(kind, func(t *testing.T) {
			s, f, _, b := storeFixture(t)
			ctx := context.Background()
			snap, o := observed(t, s, f, 1)
			now := s.clock()
			switch kind {
			case "policy":
				o.control.selection.policy.ExpiresAt = now.Add(time.Second)
				s.clock = func() time.Time { return now.Add(2 * time.Second) }
			case "root":
				o.expires = now.Add(time.Second)
				s.clock = func() time.Time { return now.Add(2 * time.Second) }
			case "stale":
				s.clock = func() time.Time { return now.Add(time.Hour) }
			case "age":
				o.control.selection.policy.Limits.MaxAgeSeconds = int64((now.Sub(clock) + time.Second) / time.Second)
				o.control.selection.policy.Limits.LeadTimeSeconds = 0
				s.clock = func() time.Time { return now.Add(2 * time.Second) }
			}
			d, err := s.Advance(ctx, snap.Version, o)
			if d.AllowCode {
				t.Fatal("cached observation bypassed transaction fence", kind, d, err)
			}
			if kind == "root" {
				b.expires = now.Add(time.Second)
				fresh, err := OpenStore(filepath.Join(t.TempDir(), "new.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				defer fresh.Close()
				fresh.clock = s.clock
				if err := fresh.Initialize(ctx, b); err == nil {
					t.Fatal("expired baseline initialized")
				}
			}
		})
	}
}

func TestStoreCannotResetDebtOrLoseItOnControlSwitch(t *testing.T) {
	s, f, _, b := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	// Keep this below the dispatch threshold to permit a control transition.
	o.control.selection.policy.Limits.MaxChanges = 10
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx, b); !errors.Is(err, ErrInitialized) {
		t.Fatal("reset accepted", err)
	}
	snap, o = observed(t, s, f, 1)
	old := snap.State
	o.control.revision = sha(2)
	o.control.selection.revision = sha(2)
	o.control.selection.digest = Digest([]byte("new approved scope"))
	o.event.History.Commits[0].Code = false
	if err := s.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	next, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.State.Root != old.Root || next.State.Watermark != old.Root || !next.History.Commits[0].Code || !next.History.Commits[0].IntegratedAt.Equal(clock) {
		t.Fatal("new control erased debt", next)
	}
	before := next.State
	o.event.History = History{Root: sha(999)}
	if err := s.SwitchControl(ctx, next.Version, o); err == nil {
		t.Fatal("arbitrary head reset accepted")
	}
	after, err := s.Read(ctx)
	if err != nil || !reflect.DeepEqual(after.State, before) {
		t.Fatal("failed switch mutated state", after, err)
	}
}

func TestSealedSnapshotCannotSupplyCandidateHistory(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap.History.Commits = history(1, clock).Commits
	if _, err := f.adapter.Observe(context.Background(), snap); err == nil {
		t.Fatal("candidate-provided prefix trusted")
	}
}

func TestObservedSuffixDoesNotRescanCoveredHistoryAndBoundsRequests(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	before := f.historyReads
	f.mu.Unlock()
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.adapter.Observe(ctx, snap); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	after := f.historyReads
	f.mu.Unlock()
	if after != before {
		t.Fatal("same-tip observation rescanned history", before, after)
	}
	_, o = observed(t, s, f, 2)
	if len(o.event.History.Commits) != 2 {
		t.Fatal(o)
	}
	f.mu.Lock()
	after = f.historyReads
	f.mu.Unlock()
	if after-before != 3 {
		t.Fatal("did not fetch only one immutable suffix commit", before, after)
	}
	// A bounded API budget is a fail-closed input limit, not a requirement to
	// accumulate or repeat a sample count before promotion.
	f.adapter.root.MaxRequests = 1
	if _, err = f.adapter.Observe(ctx, snap); err == nil {
		t.Fatal("unbounded API work accepted")
	}
}

func TestSignedRequirementRefreshCannotCheapenFrozenProof(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	if err := s.Pump(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frozen := *snap.State.Active
	// Model another valid authority-signed document with the same key and
	// control/policy bytes. Private fixture mutation is NOT a live approval.
	req := f.adapter.root.Controls[f.adapter.root.CurrentControl]
	req.MacOSJobs = []string{"build-macos"}
	f.adapter.root.Controls[f.adapter.root.CurrentControl] = req

	if _, err = f.adapter.Observe(ctx, snap); err == nil {
		t.Fatal("root refresh changed bound proof scope")
	}
	o.control.authority = f.adapter.authorityDigest()
	if err = s.SwitchControl(ctx, snap.Version, o); err == nil {
		t.Fatal("active proof replaced by scope switch")
	}
	after, err := s.Read(ctx)
	if err != nil || *after.State.Active != frozen {
		t.Fatal(after, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("changed scope dispatched", f.posts)
	}
}

func TestExhaustedPreflightBudgetLeavesDefinitelyUnsentIntentPrepared(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	// Set the signed budget before freezing its authority digest.
	f.adapter.root.MaxRequests = 16
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	snap, o = observed(t, s, f, 1)
	if _, err = s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	if err = s.Pump(ctx, f.adapter); err == nil || errors.Is(err, ErrAmbiguous) {
		t.Fatal("budget exhaustion became ambiguous", err)
	}
	snap, err = s.Read(ctx)
	if err != nil || snap.Intent.Status != "prepared" {
		t.Fatal(snap, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 {
		t.Fatal("POST despite exhausted preflight budget", f.posts)
	}
}

func TestExpiredQueuedProofCompletesBeforeExplicitControlSwitch(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	if err := s.Pump(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	// Policy bytes stay immutable. Refresh only the independent root's expiry,
	// which deliberately does not change frozen scope identity.
	now := validPolicy().ExpiresAt.Add(time.Minute)
	s.clock = func() time.Time { return now }
	f.adapter.clock = s.clock
	f.adapter.root.ExpiresAt = now.Add(time.Hour)
	snap, o = observed(t, s, f, 1)
	d, err := s.Advance(ctx, snap.Version, o)
	if err != nil || d.AllowCode || d.Dispatch != nil {
		t.Fatal("expiry did not fence admission", d, err)
	}
	snap, err = s.Read(ctx)
	if err != nil || snap.State.Active != nil || snap.State.Watermark != sha(101) {
		t.Fatal("expired queued proof stranded", snap, err)
	}
	// A separately approved replacement uses the same immutable root/history,
	// never an arbitrary new baseline. Fixture mutation models that approval.
	p := validPolicy()
	p.Owner = f.root.Controls[sha(2)].Owner.Login
	p.Profile = "stable"
	p.RollbackRevision = sha(1)
	p.ExpiresAt = now.Add(time.Hour)
	raw, _ := json.Marshal(p)
	f.mu.Lock()
	f.policy[sha(2)] = raw
	f.mu.Unlock()
	req := f.adapter.root.Controls[sha(2)]
	req.PolicyDigest = Digest(raw)
	f.adapter.root.Controls[sha(2)] = req
	f.adapter.root.CurrentControl = sha(2)
	o, err = f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	next, err := s.Read(ctx)
	if err != nil || next.State.Root != sha(100) || next.State.Watermark != sha(100) || len(next.History.Commits) != 1 {
		t.Fatal("switch lost fixed-root debt", next, err)
	}
}

func TestDispatchReconciliationIgnoresUnrelatedLifetimeRuns(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.unrelatedRuns = 1200
	f.lost = true
	f.mu.Unlock()
	if err := s.Pump(ctx, f.adapter); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lost = false
	f.mu.Unlock()
	if err := s.Pump(ctx, f.adapter); err != nil {
		t.Fatal("unrelated workflow history blocked fixed-ref reconciliation", err)
	}
	snap, err := s.Read(ctx)
	if err != nil || snap.Intent.Status != "bound" {
		t.Fatal(snap, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("reconciliation duplicated POST", f.posts)
	}
}

func TestExpiredPreparedIntentCanRetireWithoutErasingDebt(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldID := snap.Intent.Sweep.ID
	now := validPolicy().ExpiresAt.Add(time.Minute)
	s.clock = func() time.Time { return now }
	f.adapter.clock = s.clock
	f.adapter.root.ExpiresAt = now.Add(time.Hour)
	if err = s.Pump(ctx, f.adapter); err == nil || errors.Is(err, ErrAmbiguous) {
		t.Fatal("expired unsent intent dispatched or became ambiguous", err)
	}
	p := validPolicy()
	p.Owner = f.root.Controls[sha(2)].Owner.Login
	p.Profile = "stable"
	p.RollbackRevision = sha(1)
	p.ExpiresAt = now.Add(time.Hour)
	raw, _ := json.Marshal(p)
	f.mu.Lock()
	f.policy[sha(2)] = raw
	f.mu.Unlock()
	req := f.adapter.root.Controls[sha(2)]
	req.PolicyDigest = Digest(raw)
	f.adapter.root.Controls[sha(2)] = req
	f.adapter.root.CurrentControl = sha(2)
	o, err = f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal("definitely unsent proof stranded", err)
	}
	next, err := s.Read(ctx)
	if err != nil || next.State.Active != nil || next.State.Root != sha(100) || next.State.Watermark != sha(100) || len(next.History.Commits) != 1 {
		t.Fatal("retirement lost debt", next, err)
	}
	var status string
	if err = s.db.QueryRow("SELECT status FROM intent WHERE id=?", oldID).Scan(&status); err != nil || status != "retired" {
		t.Fatal(status, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 {
		t.Fatal("retirement created an Actions run", f.posts)
	}
}

func TestPreparedSwitchAndCompetingPumpKeepOneSerialOutcome(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replacement := *f.adapter
	replacement.root.CurrentControl = sha(2)
	o, err = replacement.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var pumpErr, switchErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; pumpErr = s.Pump(ctx, f.adapter) }()
	go func() { defer wg.Done(); <-start; switchErr = s.SwitchControl(ctx, snap.Version, o) }()
	close(start)
	wg.Wait()
	next, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	if switchErr == nil {
		if next.State.Control != sha(2) || next.State.Active != nil || posts != 0 {
			t.Fatal("retired intent sent", next, posts, pumpErr)
		}
	} else {
		if pumpErr != nil || posts != 1 || next.State.Control != sha(1) || next.Intent == nil || next.Intent.Status != "bound" {
			t.Fatal("neither serialized outcome retained", next, posts, pumpErr, switchErr)
		}
	}
	if next.State.Root != sha(100) || next.State.Watermark != sha(100) || len(next.History.Commits) != 1 {
		t.Fatal("race erased debt", next)
	}
}

func TestChangedRequirementsRequireExplicitSerializedSwitch(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := f.adapter.root.Controls[sha(1)]
	req.MacOSJobs = []string{"build-macos"}
	f.adapter.root.Controls[sha(1)] = req
	o, err := f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Advance(ctx, snap.Version, o); err == nil {
		t.Fatal("changed gate requirements silently reused lane authority")
	}
	if err = s.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal("explicit safe switch rejected", err)
	}
}

func TestNewScopeRevalidatesHistoricalMandatoryReviews(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	o.control.selection.policy.Limits.MaxChanges = 10
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The existing ordinary prefix needed no review under its former scope.
	// A new signed scope may add provider requirements, but cannot borrow that
	// old prefix as evidence those exact-head requirements were met.
	f.adapter.root.RequiredPullReviews = map[int][]Identity{f.pull.Number: {f.reviews[0].User}}
	if _, err = f.adapter.Observe(ctx, snap); err == nil {
		t.Fatal("new mandatory history review never fetched/verified")
	}
}
