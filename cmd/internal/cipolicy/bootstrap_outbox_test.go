package cipolicy

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func coldFixture(t *testing.T) (*Store, *githubFixture, string) {
	t.Helper()
	f := fixture(t)
	f.adapter.clock = func() time.Time { return clock.Add(10 * time.Minute) }
	c, err := f.adapter.control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.adapter.root.Bootstrap = BootstrapRequirement{Target: sha(100), Attempt: 1, Sweep: Sweep{ID: "first immutable bootstrap", From: sha(99), Target: sha(100), Control: c.revision, Digest: c.selection.digest, Authority: c.authority, CompatibilityRequired: true}}
	file := filepath.Join(t.TempDir(), "cold.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	s.clock = f.adapter.clock
	t.Cleanup(func() { s.Close() })
	return s, f, file
}

func TestColdBootstrapEntrypointDispatchBeforeLane(t *testing.T) {
	s, f, _ := coldFixture(t)
	ctx := context.Background()
	c := NewCoordinator(f.adapter, s)
	if _, err := c.Execute(ctx, "bootstrap", 0, 0); err == nil {
		t.Fatal("unproved baseline accepted")
	}
	if _, err := c.Execute(ctx, "bootstrap-prepare", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx); err == nil {
		t.Fatal("prepared outbox granted lane")
	}
	if _, err := c.Execute(ctx, "bootstrap-pump", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx); err == nil {
		t.Fatal("dispatch granted lane")
	}
	if _, err := c.Execute(ctx, "bootstrap", 0, 0); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Read(ctx)
	if err != nil || snap.State.Watermark != sha(100) {
		t.Fatal(snap, err)
	}
	if _, err = c.Execute(ctx, "bootstrap", 0, 0); !errors.Is(err, ErrInitialized) {
		t.Fatal("reset allowed", err)
	}
	if f.posts != 1 {
		t.Fatal("duplicate dispatch", f.posts)
	}
}

func TestColdBootstrapLostResponseRestartAndFrozenRoot(t *testing.T) {
	s, f, file := coldFixture(t)
	ctx := context.Background()
	if _, err := s.PrepareBootstrap(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lost = true
	f.delay = true
	f.mu.Unlock()
	if _, err := s.PumpBootstrap(ctx, f.adapter); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	other, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = s.clock
	if _, err = other.PumpBootstrap(ctx, f.adapter); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	original := f.adapter.root.Bootstrap
	f.adapter.root.Bootstrap.Sweep.ID = "replacement"
	if _, err = other.PrepareBootstrap(ctx, f.adapter); err == nil {
		t.Fatal("root refresh replaced uncertain proof")
	}
	if _, err = other.PumpBootstrap(ctx, f.adapter); err == nil {
		t.Fatal("changed request dispatched")
	}
	f.adapter.root.Bootstrap = original
	f.mu.Lock()
	f.lost = false
	f.delay = false
	f.mu.Unlock()
	if _, err = other.PumpBootstrap(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.run.Conclusion = "failure"
	f.mu.Unlock()
	if err = other.bootstrap(ctx, f.adapter); err == nil {
		t.Fatal("failed proof grants baseline")
	}
	if _, err = other.Read(ctx); err == nil {
		t.Fatal("failed proof initialized")
	}
	if f.posts != 1 {
		t.Fatal("lost response repeated", f.posts)
	}
	// A terminal failed baseline is retained, never erased/retried implicitly.
}

func TestColdBootstrapConcurrentPumpExpiryAndSkip(t *testing.T) {
	s, f, file := coldFixture(t)
	ctx := context.Background()
	if _, err := s.PrepareBootstrap(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = s.clock
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, v := range []*Store{s, other} {
		wg.Add(1)
		go func(v *Store) { defer wg.Done(); _, err := v.PumpBootstrap(ctx, f.adapter); errs <- err }(v)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrAmbiguous) {
			t.Fatal(err)
		}
	}
	if f.posts != 1 {
		t.Fatal("concurrent duplicate", f.posts)
	}
	f.tamper = func(_ string, v any) {
		if m, ok := v.(map[string]any); ok {
			if jobs, ok := m["jobs"].([]jobRecord); ok {
				jobs[0].Conclusion = "skipped"
			}
		}
	}
	if err = s.bootstrap(ctx, f.adapter); err == nil {
		t.Fatal("skip granted baseline")
	}
	f.tamper = nil
	f.adapter.root.ExpiresAt = s.clock()
	if err = s.bootstrap(ctx, f.adapter); err == nil {
		t.Fatal("expired root initialized")
	}
	if _, err = s.Read(ctx); err == nil {
		t.Fatal("invalid baseline exists")
	}
}

func TestColdBootstrapFailedRecoveryRetainsAuditAndClosesUnknown(t *testing.T) {
	for _, scenario := range []string{"failed", "running", "success", "ambiguous", "wrong-owner", "new-key", "expiry"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, _ := coldFixture(t)
			ctx := context.Background()
			if _, err := s.PrepareBootstrap(ctx, f.adapter); err != nil {
				t.Fatal(err)
			}
			if scenario == "ambiguous" {
				f.mu.Lock()
				f.lost = true
				f.delay = true
				f.mu.Unlock()
			}
			_, err := s.PumpBootstrap(ctx, f.adapter)
			if err != nil && scenario != "ambiguous" {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.run.Status = "completed"
			f.run.Conclusion = "failure"
			if scenario == "running" {
				f.run.Status = "in_progress"
				f.run.Conclusion = ""
			}
			if scenario == "success" {
				f.run.Conclusion = "success"
			}
			f.mu.Unlock()
			old := f.adapter.root.Bootstrap
			f.adapter.root.Bootstrap.Sweep.ID = "diagnosed replacement proof"
			if scenario == "wrong-owner" {
				f.tamper = func(p string, v any) {
					if p == "/user" {
						v.(*Identity).ID++
					}
				}
			}
			if scenario == "new-key" {
				f.adapter.keyDigest = Digest([]byte("other-authority-key"))
			}
			if scenario == "expiry" {
				f.adapter.root.ExpiresAt = s.clock()
			}
			next, err := s.RecoverBootstrap(ctx, f.adapter, "Repair separately reviewed; terminal first proof diagnosed")
			if scenario != "failed" {
				if err == nil {
					t.Fatal("unresolved/unauthorized proof replaced", scenario, next)
				}
				if f.posts != 1 {
					t.Fatal("recovery dispatched", f.posts)
				}
				return
			}
			if err != nil || next.Status != "prepared" || next.Sweep.ID == old.Sweep.ID {
				t.Fatal(next, err)
			}
			var archived int
			if err = s.db.QueryRow("SELECT count(*) FROM bootstrap_archive").Scan(&archived); err != nil || archived != 1 {
				t.Fatal("old proof audit missing", archived, err)
			}
			if f.posts != 1 {
				t.Fatal("recovery dispatched before pump", f.posts)
			}
			if _, err = s.PumpBootstrap(ctx, f.adapter); err != nil {
				t.Fatal(err)
			}
			if err = s.bootstrap(ctx, f.adapter); err != nil {
				t.Fatal(err)
			}
			if f.posts != 2 {
				t.Fatal("replacement dispatch count", f.posts)
			}
		})
	}
}
