package cipolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func failedCoordinator(t *testing.T, counts ...int) (*Coordinator, *githubFixture, string) {
	t.Helper()
	s, f, file, _ := storeFixture(t)
	ctx := context.Background()
	snap, observation := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, observation); err != nil {
		t.Fatal(err)
	}
	if err := s.Pump(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.run.Conclusion = "failure"
	f.mu.Unlock()
	count := 2
	if len(counts) > 0 {
		count = counts[0]
	}
	snap, observation = observed(t, s, f, count)
	if _, err := s.Advance(ctx, snap.Version, observation); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Read(ctx)
	if !snap.State.Failed || snap.State.Watermark != sha(100) || snap.State.Active != nil {
		t.Fatal(snap)
	}
	// Explicitly mutated authentication response, not actual owner approval.
	owner := f.root.Controls[f.root.CurrentControl].Owner
	f.tamper = func(path string, value any) {
		if path == "/user" {
			*value.(*Identity) = owner
		}
	}
	return NewCoordinator(f.adapter, s), f, file
}

func TestCoordinatorAuthenticatedRecoveryPreservesDebtAcrossRestart(t *testing.T) {
	c, f, file := failedCoordinator(t)
	ctx := context.Background()
	before, _ := c.store.Read(ctx)
	posts := f.posts
	out, err := c.Recover(ctx, "diagnosed isolated runner failure; new proof required")
	if err != nil || out.State == nil || !out.State.Failed || !out.State.Recovering || out.Decision == nil || out.Decision.Uncovered != 2 || out.Decision.AllowCode || out.Intent == nil || out.Intent.Status != "prepared" {
		t.Fatal(out, err)
	}
	if out.State.Root != before.State.Root || out.State.Watermark != before.State.Watermark || out.State.Active.From != sha(100) || out.State.Active.Target != sha(102) || f.posts != posts {
		t.Fatal("recovery reset debt or sent a workflow", out)
	}
	restarted, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.clock = c.store.clock
	snap, err := restarted.Read(ctx)
	if err != nil || snap.State.Active.ID != out.State.Active.ID || snap.State.Watermark != sha(100) {
		t.Fatal(snap, err)
	}
	var owner, reason, watermark string
	if err := restarted.db.QueryRow("SELECT owner,reason,watermark FROM recovery WHERE revision=?", snap.Version).Scan(&owner, &reason, &watermark); err != nil || owner != f.root.Controls[f.root.CurrentControl].Owner.Login || reason == "" || watermark != before.State.Watermark {
		t.Fatal("diagnosis audit lost", owner, reason, watermark, err)
	}
	var oldProofs, active int
	restarted.db.QueryRow("SELECT count(*) FROM intent WHERE status='done'").Scan(&oldProofs)
	restarted.db.QueryRow("SELECT count(*) FROM intent WHERE status NOT IN ('done','retired')").Scan(&active)
	if oldProofs != 1 || active != 1 {
		t.Fatal("failed proof lost or replacement duplicated", oldProofs, active)
	}
	// Even a forged green replay of the old run cannot cover the new intent.
	observation, err := f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = restarted.db.QueryRow("SELECT sweep FROM intent WHERE status='done'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var old Sweep
	if err = json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	observation.event.Completion = green(&old)
	observation.runID, observation.attempt = f.run.ID, 1
	if _, err = restarted.Advance(ctx, snap.Version, observation); err == nil {
		t.Fatal("old completion repaid new debt")
	}
	// A second diagnosis cannot replace the new sole proof or erase newer debt.
	if _, err := NewCoordinator(f.adapter, restarted).Recover(ctx, "repeat"); err == nil {
		t.Fatal("recovery replaced running proof")
	}
	after, _ := restarted.Read(ctx)
	if !reflect.DeepEqual(snap.State, after.State) || after.Version != snap.Version {
		t.Fatal("rejected recovery changed state")
	}
}

func TestCoordinatorRecoveryRejectsWrongOwnerEmptyAndExpired(t *testing.T) {
	for _, scenario := range []string{"wrong-owner", "empty", "oversized", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, _ := failedCoordinator(t)
			ctx := context.Background()
			before, _ := c.store.Read(ctx)
			reason := "diagnosed failure"
			switch scenario {
			case "wrong-owner":
				owner := f.root.Controls[f.root.CurrentControl].Owner
				owner.ID++
				f.tamper = func(path string, value any) {
					if path == "/user" {
						*value.(*Identity) = owner
					}
				}
			case "empty":
				reason = " \n"
			case "oversized":
				reason = strings.Repeat("x", 4097)
			case "expired":
				c.adapter.root.ExpiresAt = c.store.clock()
			}
			if _, err := c.Recover(ctx, reason); err == nil {
				t.Fatal("unauthenticated/invalid recovery accepted")
			}
			after, _ := c.store.Read(ctx)
			var audits int
			c.store.db.QueryRow("SELECT count(*) FROM recovery").Scan(&audits)
			if before.Version != after.Version || !reflect.DeepEqual(before.State, after.State) || audits != 0 {
				t.Fatal("failed recovery mutated evidence")
			}
		})
	}
}

func TestConcurrentCoordinatorRecoveryCreatesOneIntentAndAudit(t *testing.T) {
	c, f, file := failedCoordinator(t)
	other, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = c.store.clock
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, coordinator := range []*Coordinator{c, NewCoordinator(f.adapter, other)} {
		wg.Add(1)
		go func(c *Coordinator) {
			defer wg.Done()
			_, err := c.Recover(context.Background(), "authenticated diagnosis")
			results <- err
		}(coordinator)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	var audits, active int
	c.store.db.QueryRow("SELECT count(*) FROM recovery").Scan(&audits)
	c.store.db.QueryRow("SELECT count(*) FROM intent WHERE status NOT IN ('done','retired')").Scan(&active)
	if success != 1 || audits != 1 || active != 1 || f.posts != 1 {
		t.Fatal(success, audits, active, f.posts)
	}
}

func TestRecoveryCLIRequiresDiagnosisAndReachesAuthorityLoader(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		status int
	}{
		{[]string{"recover", "missing-authority", "--reason", "nonsecret diagnosis"}, 1},
		{[]string{"recover", "missing-authority"}, 2},
		{[]string{"recover", "missing-authority", "--reason", " "}, 2},
		{[]string{"recover", "missing-authority", "--pull", "1107"}, 2},
		{[]string{"status", "missing-authority", "--reason", "diagnosis"}, 2},
	} {
		var output bytes.Buffer
		status := RunCLI(context.Background(), tc.args, &output, "unused-binary")
		if status != tc.status || !strings.Contains(output.String(), `"Closed":true`) || strings.Contains(output.String(), "nonsecret diagnosis") || strings.Contains(output.String(), "missing-authority") {
			t.Fatal(status, output.String())
		}
	}
}

func TestRecoveryPendingIsClosedInCLIOutput(t *testing.T) {
	var output bytes.Buffer
	writeCLI(&output, Outcome{State: &State{Failed: true, Recovering: true}}, nil)
	if !strings.Contains(output.String(), `"Closed":true`) {
		t.Fatal(output.String())
	}
}

func TestCoordinatorBelowBudgetRecoveryWaitsForFullReplacement(t *testing.T) {
	c, f, _ := failedCoordinator(t, 1)
	ctx := context.Background()
	out, err := c.Recover(ctx, "diagnosed early failure")
	if err != nil || out.Decision == nil || out.Decision.Uncovered != 1 || out.Decision.AllowCode || !out.State.Failed || !out.State.Recovering {
		t.Fatal("diagnosis reopened below-budget lane", out, err)
	}
	out, err = c.Execute(ctx, "observe", 0, 0)
	if err != nil || out.Decision.AllowCode || !out.State.Failed {
		t.Fatal("observation reopened pending recovery", out, err)
	}
	f.tamper = nil // Use the fixture's independently designated dispatcher for pump.
	if err = c.store.Pump(ctx, f.adapter); err != nil {
		t.Fatal(err)
	}
	out, err = c.Execute(ctx, "observe", 0, 0)
	if err != nil || out.State.Failed || out.State.Recovering || out.State.Root != sha(100) || out.State.Watermark != sha(101) || out.Decision.Uncovered != 0 {
		t.Fatal("full replacement not required/completed", out, err)
	}
}

func TestRecoveryControlTransitionPreservesFailureAndDebt(t *testing.T) {
	c, f, _ := failedCoordinator(t, 1)
	ctx := context.Background()
	if _, err := c.Recover(ctx, "diagnosed failure"); err != nil {
		t.Fatal(err)
	}
	f.adapter.root.CurrentControl = sha(2)
	out, err := c.Execute(ctx, "control-transition", 0, 0)
	if err != nil || out.State == nil || !out.State.Failed || out.State.Recovering || out.State.Active != nil || out.State.Root != sha(100) || out.State.Watermark != sha(100) {
		t.Fatal("control transition erased failure or retained retired proof", out, err)
	}
	out, err = c.Execute(ctx, "observe", 0, 0)
	if err != nil || out.Decision == nil || out.Decision.AllowCode || out.Decision.Uncovered != 1 || out.Intent != nil {
		t.Fatal("new control bypassed diagnosis/full-proof fence", out, err)
	}
}
