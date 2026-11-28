package cipolicy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExternalAuthorityLoaderAndClosedCLI(t *testing.T) {
	f := fixture(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "trusted-coordinator")
	os.WriteFile(binary, []byte("reviewed binary fixture"), 0600)
	f.root.CoordinatorDigest = Digest([]byte("reviewed binary fixture"))
	key, private, _ := ed25519.GenerateKey(rand.Reader)
	raw, _ := json.Marshal(f.root)
	files := map[string][]byte{"authority.pub": []byte(base64.StdEncoding.EncodeToString(key)), "root.json": raw, "root.sig": []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, raw))), "dispatcher.token": []byte("private_fixture_token")}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := LoadCoordinator(dir, binary)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, scenario := range []string{"changed-binary", "public-token", "writable-key", "hardlinked-key", "symlink", "state-symlink", "state-hardlink", "candidate-checkout", "signature"} {
		t.Run(scenario, func(t *testing.T) {
			switch scenario {
			case "changed-binary":
				os.WriteFile(binary, []byte("candidate"), 0600)
				defer os.WriteFile(binary, []byte("reviewed binary fixture"), 0600)
			case "writable-key":
				os.Chmod(filepath.Join(dir, "authority.pub"), 0664)
				defer os.Chmod(filepath.Join(dir, "authority.pub"), 0600)
			case "hardlinked-key":
				alias := filepath.Join(t.TempDir(), "key-alias")
				if err := os.Link(filepath.Join(dir, "authority.pub"), alias); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(alias)
			case "state-symlink":
				state := filepath.Join(dir, "coverage.sqlite")
				os.Rename(state, state+".original")
				os.Symlink(state+".original", state)
				defer func() { os.Remove(state); os.Rename(state+".original", state) }()
			case "state-hardlink":
				alias := filepath.Join(t.TempDir(), "db-alias")
				if err := os.Link(filepath.Join(dir, "coverage.sqlite"), alias); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(alias)
			case "public-token":
				os.Chmod(filepath.Join(dir, "dispatcher.token"), 0644)
				defer os.Chmod(filepath.Join(dir, "dispatcher.token"), 0600)
			case "symlink":
				os.Rename(filepath.Join(dir, "dispatcher.token"), filepath.Join(dir, "original"))
				os.Symlink("original", filepath.Join(dir, "dispatcher.token"))
				defer func() {
					os.Remove(filepath.Join(dir, "dispatcher.token"))
					os.Rename(filepath.Join(dir, "original"), filepath.Join(dir, "dispatcher.token"))
				}()
			case "candidate-checkout":
				os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: candidate"), 0600)
				defer os.Remove(filepath.Join(dir, ".git"))
			case "signature":
				os.WriteFile(filepath.Join(dir, "root.sig"), []byte("candidate"), 0600)
				defer os.WriteFile(filepath.Join(dir, "root.sig"), files["root.sig"], 0600)
			}
			rejected, loadErr := LoadCoordinator(dir, binary)
			if rejected != nil {
				rejected.Close()
			}
			if loadErr == nil {
				t.Fatal("unsafe authority passed the loader itself", scenario)
			}
			var out bytes.Buffer
			if status := RunCLI(context.Background(), []string{"status", dir}, &out, binary); status == 0 {
				t.Fatal("invalid authority opened")
			}
			if strings.Contains(out.String(), "private_fixture_token") || strings.Contains(out.String(), dir) || !strings.Contains(out.String(), `"Closed":true`) {
				t.Fatal("unsafe CLI error", out.String())
			}
		})
	}
	var out bytes.Buffer
	if RunCLI(context.Background(), []string{"rerun", dir}, &out, binary) == 0 {
		t.Fatal("unknown command accepted")
	}
}

func TestCoordinatorRestartConcurrentObserveAndAmbiguousPump(t *testing.T) {
	s, f, file, _ := storeFixture(t)
	ctx := context.Background()
	observed(t, s, f, 1)
	c := NewCoordinator(f.adapter, s)
	if _, err := c.Execute(ctx, "observe", 0, 0); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = s.clock
	restarted := NewCoordinator(f.adapter, other)
	f.mu.Lock()
	f.lost = true
	f.delay = true
	f.mu.Unlock()
	if _, err = c.Execute(ctx, "pump", 0, 0); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	if _, err = restarted.Execute(ctx, "status", 0, 0); !errors.Is(err, ErrAmbiguous) {
		t.Fatal("ambiguous status green", err)
	}
	if _, err = restarted.Execute(ctx, "pump", 0, 0); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lost = false
	f.delay = false
	f.mu.Unlock()
	if _, err = restarted.Execute(ctx, "pump", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Execute(ctx, "observe", 0, 0); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Read(ctx)
	if snap.State.Watermark != sha(101) {
		t.Fatal(snap)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("entrypoint restart resent proof", f.posts)
	}
}

func routeFixture(t *testing.T) (*Coordinator, *githubFixture, RouteGrant) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	req := f.adapter.root.Controls[sha(1)]
	req.DevelopmentJobs = []string{"test"}
	f.adapter.root.Controls[sha(1)] = req
	snap, _ := s.Read(ctx)
	o, err := f.adapter.Observe(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SwitchControl(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	control, err := f.adapter.control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g := RouteGrant{Owner: f.reviews[0].User, Head: sha(101), Base: sha(100), Ref: "develop", ExpiresAt: s.clock().Add(time.Hour), ConsentID: 7232319457, FeedbackRun: f.run.ID, Feedback: Sweep{ID: "development fixture", From: sha(100), Target: sha(101), Control: control.revision, Digest: control.selection.digest, Authority: control.authority}}
	f.adapter.root.RoutingEnabled = true
	f.adapter.root.Routes = map[int]RouteGrant{1107: g}
	p := f.pull
	p.Number = 1107
	p.State = "open"
	p.Draft = false
	p.Merged = false
	p.MergedAt = time.Time{}
	p.Head.SHA = g.Head
	p.Head.Repo = f.repo
	p.Base.SHA = g.Base
	p.Base.Ref = g.Ref
	p.Base.Repo = f.repo
	p.User = Identity{ID: g.Owner.ID + 1, Login: "candidate"}
	var comment map[string]any
	capture(t, "consent-comment", &comment)
	comment["body"] = consentText(f.root.RepositoryID, 1107, g)
	comment["created_at"] = clock.Add(-time.Minute).Format(time.RFC3339)
	comment["updated_at"] = clock.Add(-time.Minute).Format(time.RFC3339)
	comment["issue_url"] = "https://api.github.com/repos/" + f.root.Repository + "/issues/1107"
	f.mu.Lock()
	f.candidate = &p
	f.consent = comment
	f.candidateReviews = append([]reviewRecord{}, f.reviews...)
	f.commits[sha(101)] = history(1, clock).Commits[0]
	f.run.HeadSHA = g.Head
	f.run.Event = "workflow_dispatch"
	f.run.Title = sweepTitle(g.Feedback)
	f.run.Status = "completed"
	f.run.Conclusion = "success"
	f.mu.Unlock()
	return NewCoordinator(f.adapter, s), f, g
}

func TestOwnerOptInActualFeedbackAndMainRoutePreservation(t *testing.T) {
	for _, scenario := range []string{"accepted", "unadopted", "draft", "moved-head", "wrong-owner", "skipped-feedback", "mandatory-provider", "high-risk"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, _ := routeFixture(t)
			ctx := context.Background()
			switch scenario {
			case "unadopted":
				f.adapter.root.Routes = nil
			case "draft":
				f.candidate.Draft = true
			case "moved-head":
				f.candidate.Head.SHA = sha(999)
			case "wrong-owner":
				f.consent.(map[string]any)["body"] = "candidate opt-in"
			case "skipped-feedback":
				f.tamper = func(_ string, v any) {
					if m, ok := v.(map[string]any); ok {
						if jobs, ok := m["jobs"].([]jobRecord); ok {
							for i := range jobs {
								if jobs[i].Name == "test" {
									jobs[i].Conclusion = "skipped"
								}
							}
						}
					}
				}
			case "mandatory-provider":
				f.adapter.root.RequiredPullReviews = map[int][]Identity{1107: {f.reviews[0].User}}
				snap, _ := c.store.Read(ctx)
				o, err := f.adapter.Observe(ctx, snap)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.store.SwitchControl(ctx, snap.Version, o); err != nil {
					t.Fatal(err)
				}
				g := f.adapter.root.Routes[1107]
				g.Feedback.Authority = f.adapter.authorityDigest()
				f.adapter.root.Routes[1107] = g
				f.mu.Lock()
				f.run.Title = sweepTitle(g.Feedback)
				f.mu.Unlock()
			case "high-risk":
				v := f.commits[sha(101)]
				v.Compatibility = true
				f.commits[sha(101)] = v
			}
			out, err := c.Execute(ctx, "admission", 1107, 0)
			if scenario == "accepted" {
				if err != nil || out.Route == nil || !out.Route.Allowed || out.Route.DoneAllowed || out.Route.ReleaseAllowed || out.Route.UpgradeAllowed {
					t.Fatal(out, err)
				}
				r, err := c.store.reservation(ctx)
				if err != nil || r == nil || r.Pull != 1107 {
					t.Fatal(r, err)
				}
			} else {
				if err == nil && out.Route != nil && out.Route.Allowed {
					t.Fatal("unsafe route accepted", scenario, out)
				}
				if scenario == "unadopted" && (out.Route == nil || out.Route.Route != "main") {
					t.Fatal("existing main owner retargeted", out, err)
				}
			}
		})
	}
}

func TestAdmissionReservationsSerializeAndSurviveRestartExpiry(t *testing.T) {
	c, f, g := routeFixture(t)
	ctx := context.Background()
	out, err := c.Execute(ctx, "admission", 1107, 0)
	if err != nil || !out.Route.Allowed {
		t.Fatal(out, err)
	}
	snap, _ := c.store.Read(ctx)
	control, _ := f.adapter.control(ctx)
	var wg sync.WaitGroup
	var another error
	wg.Add(1)
	go func() {
		defer wg.Done()
		another = c.store.reserve(ctx, snap.Version, 1108, g, control.selection, f.adapter.root.ExpiresAt)
	}()
	wg.Wait()
	if another == nil {
		t.Fatal("two active integration reservations")
	}
	c.store.clock = func() time.Time { return g.ExpiresAt.Add(time.Minute) }
	if err = c.store.reserve(ctx, snap.Version, 1107, g, control.selection, f.adapter.root.ExpiresAt); err == nil {
		t.Fatal("expired permit renewed")
	}
	r, _ := c.store.reservation(ctx)
	if r == nil {
		t.Fatal("expiry silently dropped ambiguous possible merge")
	}
}

func TestDoneNeedsActualIntegrationAndFullWatermark(t *testing.T) {
	c, f, _ := routeFixture(t)
	ctx := context.Background()
	out, err := c.Execute(ctx, "done", 1107, 0)
	if err != nil || out.Route.DoneAllowed {
		t.Fatal("open PR done", out, err)
	}
	f.mu.Lock()
	f.candidate.Merged = true
	f.candidate.State = "closed"
	f.candidate.MergeSHA = sha(101)
	f.candidate.MergedAt = clock
	f.head = sha(101)
	f.mu.Unlock()
	out, err = c.Execute(ctx, "done", 1107, 0)
	if err != nil || out.Route.DoneAllowed {
		t.Fatal("uncovered integration done", out, err)
	}
	if _, err = c.Execute(ctx, "pump", 0, 0); err != nil {
		t.Fatal(err)
	}
	out, err = c.Execute(ctx, "done", 1107, 0)
	if err != nil || !out.Route.DoneAllowed || !out.Route.Integrated || out.Route.ReleaseAllowed || out.Route.UpgradeAllowed {
		t.Fatal(out, err)
	}
	now := c.store.clock().Add(2 * time.Hour)
	c.store.clock = func() time.Time { return now }
	f.adapter.clock = c.store.clock
	out, err = c.Execute(ctx, "done", 1107, 0)
	if err != nil || !out.Route.DoneAllowed {
		t.Fatal("valid pre-merge adoption expired during full-proof queue", out, err)
	}
}

func TestSummaryKeepsSkippedAndUnknownCostsHonest(t *testing.T) {
	f := fixture(t)
	capture(t, "summary-run", &f.run)
	var captured struct{ Jobs []jobRecord }
	capture(t, "summary-jobs", &captured)
	f.jobs = captured.Jobs
	// Fixture serves projected original partial records without its positive
	// job mutation, so test the real partial conclusions/timestamps directly.
	f.tamper = func(_ string, v any) {
		if m, ok := v.(map[string]any); ok {
			if _, ok := m["jobs"]; ok {
				m["jobs"] = captured.Jobs
				m["total_count"] = len(captured.Jobs)
			}
		}
	}
	s, err := f.adapter.Summarize(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, j := range s.Jobs {
		if j.Conclusion == "skipped" {
			skipped++
			if j.ElapsedSeconds != nil && *j.ElapsedSeconds != 0 {
				t.Fatal("skipped runtime invented", j)
			}
		}
	}
	if skipped != 2 || s.Tokens != "unknown" || s.Money != "unknown" || s.Evidence != "metadata only; no validation or release claim" {
		t.Fatal(fmt.Sprintf("%+v", s))
	}
}

func TestDoneRejectsLateConsentAndMandatoryReviews(t *testing.T) {
	for _, late := range []string{"consent", "edited-consent", "review"} {
		t.Run(late, func(t *testing.T) {
			c, f, g := routeFixture(t)
			ctx := context.Background()
			f.mu.Lock()
			f.candidate.Merged = true
			f.candidate.State = "closed"
			f.candidate.MergeSHA = sha(101)
			f.candidate.MergedAt = clock
			f.head = sha(101)
			if late == "consent" {
				f.consent.(map[string]any)["created_at"] = clock.Add(time.Minute).Format(time.RFC3339)
			} else if late == "edited-consent" {
				f.consent.(map[string]any)["updated_at"] = clock.Add(time.Minute).Format(time.RFC3339)
			} else {
				f.candidateReviews[0].State = "APPROVED"
				f.candidateReviews[0].CommitID = g.Head
				f.candidateReviews[0].SubmittedAt = clock.Add(time.Minute)
			}
			f.mu.Unlock()
			if late == "review" {
				g.Reviews = []Identity{g.Owner}
				f.adapter.root.Routes[1107] = g
			}
			out, err := c.Execute(ctx, "done", 1107, 0)
			if err == nil || (out.Route != nil && out.Route.DoneAllowed) {
				t.Fatal("late authority accepted", late, out, err)
			}
		})
	}
}

func TestClosedChangedPRReleasesOnlyReservationNotDebt(t *testing.T) {
	c, f, _ := routeFixture(t)
	ctx := context.Background()
	if _, err := c.Execute(ctx, "admission", 1107, 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.candidate.Head.SHA = sha(999)
	f.candidate.Base.SHA = sha(998)
	f.candidate.State = "closed"
	f.mu.Unlock()
	before, _ := c.store.Read(ctx)
	if _, err := c.Execute(ctx, "observe", 0, 0); err != nil {
		t.Fatal("closed changed PR stranded reservation", err)
	}
	r, err := c.store.reservation(ctx)
	if err != nil || r != nil {
		t.Fatal(r, err)
	}
	after, _ := c.store.Read(ctx)
	if after.State.Root != before.State.Root || after.State.Watermark != before.State.Watermark || after.State.Failed != before.State.Failed {
		t.Fatal("withdrawal changed coverage", before, after)
	}
}

func TestSameRevisionSignedGrantRenewsWithoutDroppingReservation(t *testing.T) {
	c, f, g := routeFixture(t)
	ctx := context.Background()
	if _, err := c.Execute(ctx, "admission", 1107, 0); err != nil {
		t.Fatal(err)
	}
	now := g.ExpiresAt.Add(time.Minute)
	c.store.clock = func() time.Time { return now }
	f.adapter.clock = c.store.clock
	g.ExpiresAt = now.Add(time.Hour)
	f.adapter.root.Routes[1107] = g
	f.mu.Lock()
	f.consent.(map[string]any)["body"] = consentText(f.root.RepositoryID, 1107, g)
	f.mu.Unlock()
	if _, err := c.Execute(ctx, "admission", 1107, 0); err != nil {
		t.Fatal("same exact revisions could not renew", err)
	}
	var active, audits int
	c.store.db.QueryRow("SELECT count(*) FROM admission WHERE status='reserved'").Scan(&active)
	c.store.db.QueryRow("SELECT count(*) FROM admission_grant").Scan(&audits)
	if active != 1 || audits != 2 {
		t.Fatal("renewal replaced/lost provenance", active, audits)
	}
}

func TestWorkflowRegistrationMismatchRemainsDefinitelyUnsent(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	observed(t, s, f, 1)
	c := NewCoordinator(f.adapter, s)
	if _, err := c.Execute(ctx, "observe", 0, 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.tamper = func(p string, v any) {
		if strings.HasSuffix(p, "/actions/workflows/415474141") {
			if m, ok := v.(map[string]any); ok {
				m["path"] = "candidate.yml"
			}
		}
	}
	f.mu.Unlock()
	if _, err := c.Execute(ctx, "pump", 0, 0); err == nil || errors.Is(err, ErrAmbiguous) {
		t.Fatal("unregistered contract crossed uncertainty", err)
	}
	snap, _ := s.Read(ctx)
	if snap.Intent.Status != "prepared" {
		t.Fatal(snap)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.creates != 0 {
		t.Fatal("mismatch produced side effects", f.posts, f.creates)
	}
}

func TestNonStickyWritableAncestorCannotReplaceAuthorityAnchor(t *testing.T) {
	ancestor := t.TempDir()
	os.Chmod(ancestor, 0777)
	defer os.Chmod(ancestor, 0700)
	dir := filepath.Join(ancestor, "authority")
	os.Mkdir(dir, 0700)
	if _, err := LoadCoordinator(dir, "not-a-binary"); err == nil || !strings.Contains(err.Error(), "ancestor") {
		t.Fatal("replaceable independent key accepted", err)
	}
}

func TestCoordinatorConcurrentObservationsKeepOneDurableIntent(t *testing.T) {
	s, f, file, _ := storeFixture(t)
	ctx := context.Background()
	observed(t, s, f, 1)
	other, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = s.clock
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			<-start
			_, err := NewCoordinator(f.adapter, store).Execute(ctx, "observe", 0, 0)
			results <- err
		}(store)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	snap, err := s.Read(ctx)
	if err != nil || success == 0 || snap.Intent == nil || snap.Intent.Status != "prepared" {
		t.Fatal(success, snap, err)
	}
	var intents int
	if err = s.db.QueryRow("SELECT count(*) FROM intent WHERE status='prepared'").Scan(&intents); err != nil || intents != 1 {
		t.Fatal("coordinator race duplicated intent", intents, err)
	}
}

func TestReservationRechecksPolicyAtCommitTime(t *testing.T) {
	c, f, g := routeFixture(t)
	ctx := context.Background()
	snap, _ := c.store.Read(ctx)
	control, err := f.adapter.control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := c.store.clock()
	control.selection.policy.ExpiresAt = now.Add(time.Second)
	c.store.clock = func() time.Time { return now.Add(2 * time.Second) }
	if err = c.store.reserve(ctx, snap.Version, 1107, g, control.selection, f.adapter.root.ExpiresAt); err == nil {
		t.Fatal("cached eligibility survived policy expiry before reservation")
	}
	r, _ := c.store.reservation(ctx)
	if r != nil {
		t.Fatal("expired admission committed", r)
	}
}
