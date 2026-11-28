package cipolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFirstRealPolicyCommitBootstrapAndMainFullFallback(t *testing.T) {
	ctx := context.Background()
	f := fixture(t)
	repo := t.TempDir()
	command(t, repo, "git", "init", "-q")
	command(t, repo, "git", "config", "user.name", "Policy fixture")
	command(t, repo, "git", "config", "user.email", "policy@example.invalid")
	policy := validPolicy()
	policy.Owner = f.root.Controls[f.root.CurrentControl].Owner.Login
	policy.RollbackRevision = ""
	policy.RollbackRoute = "main-full"
	policy.Limits.MaxChanges = 2
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(repo, ".github"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, PolicyPath), raw, 0600); err != nil {
		t.Fatal(err)
	}
	command(t, repo, "git", "add", ".")
	command(t, repo, "git", "commit", "-qm", "First actual policy with existing full-main fallback")
	revision := strings.TrimSpace(command(t, repo, "git", "rev-parse", "HEAD"))
	if count := strings.TrimSpace(command(t, repo, "git", "rev-list", "--count", "HEAD")); count != "1" {
		t.Fatal("predecessor policy required", count)
	}
	actual, err := (GitSource{repo}).PolicyAt(revision)
	if err != nil {
		t.Fatal(err)
	}
	requirement := f.root.Controls[f.root.CurrentControl]
	requirement.PolicyDigest = Digest(actual)
	f.root.Controls = map[string]ControlRequirement{revision: requirement}
	f.root.CurrentControl = revision
	f.policy = map[string][]byte{revision: actual}
	// Actual Git source objects are combined with the captured/mutated API seam.
	// The synthetic signatures/reviews below are tests, NOT live approvals.
	key, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bytes, _ := json.Marshal(f.root)
	a, err := NewAdapter(key, bytes, ed25519.Sign(private, bytes), "")
	if err != nil {
		t.Fatal(err)
	}
	a.base = f.adapter.base
	a.client = f.adapter.client
	a.clock = func() time.Time { return clock.Add(10 * time.Minute) }
	f.adapter = a
	control, err := a.control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.selection.Report(a.clock()).FullOnly || len(a.root.Controls) != 1 {
		t.Fatal("first real approved source still needs genesis", control.selection.Report(a.clock()))
	}
	a.root.Bootstrap = BootstrapRequirement{Target: sha(100), Attempt: 1, Sweep: Sweep{ID: "first real single-control baseline", From: sha(99), Target: sha(100), Control: revision, Digest: control.selection.digest, Authority: control.authority, CompatibilityRequired: true}}
	// Every installed variant is independently verified by NewAdapter under the
	// same test key, including the fixed initial proof and routing-off update.
	resign := func(root AuthorityRoot) *Adapter {
		t.Helper()
		raw, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		next, err := NewAdapter(key, raw, ed25519.Sign(private, raw), "")
		if err != nil {
			t.Fatal(err)
		}
		next.base = a.base
		next.client = a.client
		next.clock = a.clock
		return next
	}
	a = resign(a.root)
	f.adapter = a
	s, err := OpenStore(filepath.Join(t.TempDir(), "first.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.clock = a.clock
	c := NewCoordinator(a, s)
	for _, op := range []string{"bootstrap-prepare", "bootstrap-pump", "bootstrap"} {
		if _, err = c.Execute(ctx, op, 0, 0); err != nil {
			t.Fatal(op, err)
		}
	}
	// Add real observed debt and retain an ambiguous sole proof across fallback.
	a.root.RoutingEnabled = true
	a = resign(a.root)
	f.adapter = a
	c.adapter = a
	snap, o := observed(t, s, f, 1)
	if _, err = s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lost = true
	f.delay = true
	f.mu.Unlock()
	if _, err = c.Execute(ctx, "pump", 0, 0); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	before, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a.root.RoutingEnabled = false
	a = resign(a.root)
	f.adapter = a
	c.adapter = a
	after, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.State.Root != after.State.Root || before.State.Watermark != after.State.Watermark || before.key != after.key || before.State.Control != after.State.Control || before.State.Authority != after.State.Authority || !reflect.DeepEqual(before, after) {
		t.Fatal("fallback reset durable evidence")
	}
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	for _, op := range []string{"admission", "integrate", "done"} {
		out, err := c.Execute(ctx, op, 1107, 0)
		if op != "integrate" && !errors.Is(err, ErrAmbiguous) {
			t.Fatal(op, err)
		}
		if out.Route != nil && (out.Route.Route != "main" || out.Route.Allowed || out.Route.DoneAllowed || out.Route.ReleaseAllowed || out.Route.UpgradeAllowed) {
			t.Fatal("main referral granted permission", op, out)
		}
	}
	final, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	afterPosts := f.posts
	f.mu.Unlock()
	if final.State.Root != before.State.Root || final.State.Watermark != before.State.Watermark || final.State.Control != before.State.Control || final.State.Authority != before.State.Authority || !reflect.DeepEqual(final.State.Active, before.State.Active) || !reflect.DeepEqual(final.State.Pending, before.State.Pending) || !reflect.DeepEqual(final.History, before.History) || final.Intent.Status != "uncertain" || afterPosts != posts {
		t.Fatal("fallback lost debt/proof or dispatched", final, err)
	}
}
