package cipolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The payload shapes are actual captured GitHub output. Positive variants below
// deliberately mutate identity/state/scope/time to exercise an UNADOPTED route;
// they are not captured execution/approval of the new candidate workflow.
type candidateSeam struct {
	key                    ed25519.PrivateKey
	mu                     sync.Mutex
	run, ci, tmux          runRecord
	jobs, ciJobs, tmuxJobs []jobRecord
	artifacts              any
	checks                 map[string]any
	mode                   string
	main                   bool
	now                    time.Time
	release                func()
	entered                chan struct{}
	requests               int
	onArtifacts            func()
	onMain                 func()
}

func candidateFixture(t *testing.T) (*Coordinator, *githubFixture, *candidateSeam) {
	t.Helper()
	f := fixture(t)
	c := f.root.Controls[f.root.CurrentControl]
	c.WorkflowPath = ProofWorkflow
	c.WorkflowID = 433472301
	c.ExecutionFiles = map[string]string{ProofWorkflow: Digest(f.workflow)}
	c.LinuxJobs, c.MacOSJobs = []string{"contract", "linux", "web"}, []string{"macos"}
	c.CompatibilityJobs = nil
	c.RequiredSteps, c.RunnerLabels = map[string][]string{}, map[string][]string{}
	min := map[string][]string{"contract": {"Validate frozen dispatch"}, "linux": {"Full Linux suite", "Cross-build macOS"}, "macos": {"Full macOS suite"}, "web": {"Reproducible web build"}}
	for _, version := range compatibilityVersions {
		name := "compatibility (" + version + ")"
		c.CompatibilityJobs = append(c.CompatibilityJobs, name)
		min[name] = []string{"Full applicable tmux compatibility"}
	}
	for name, steps := range min {
		c.RequiredSteps[name] = steps
		c.RunnerLabels[name] = []string{"ubuntu-latest"}
		if name == "macos" {
			c.RunnerLabels[name] = []string{"macos-latest"}
		}
		if strings.HasPrefix(name, "compatibility") {
			c.RunnerLabels[name] = []string{"ubuntu-24.04"}
		}
	}
	p := validPolicy()
	p.Owner = c.Owner.Login
	p.RollbackRoute, p.RollbackRevision = "main-full", ""
	f.policy[f.root.CurrentControl], _ = json.Marshal(p)
	c.PolicyDigest = Digest(f.policy[f.root.CurrentControl])
	f.root.Controls = map[string]ControlRequirement{f.root.CurrentControl: c}
	f.root.MaxRequests = 128
	provider := Identity{ID: c.Owner.ID + 10, Login: "required-provider"}
	f.root.RequiredPullReviews = map[int][]Identity{1199: {provider}}
	r := CandidateRequirement{Pull: 1200, SourceRef: "release-candidate", Base: sha(100), IntegrationFrom: sha(100), Head: sha(101), Result: sha(102), Tree: sha(103), ExpiresAt: clock.Add(time.Hour), Reviews: []Identity{c.Owner, provider}, MainCI: 415474141, MainTmux: 432147149, MainActor: f.root.Dispatcher, Dependencies: map[string]string{}, MainFiles: map[string]string{}}
	for _, name := range candidateDependencies {
		r.Dependencies[name] = Digest(f.workflow)
	}
	for _, name := range candidateMainFiles {
		r.MainFiles[name] = Digest(f.workflow)
	}
	r.Sweep = Sweep{ID: "candidate-fixture", From: r.Base, Target: r.Result, Control: f.root.CurrentControl, Digest: c.PolicyDigest, CompatibilityRequired: true}
	var checkPayload map[string]any
	capture(t, "candidate-checks", &checkPayload)
	var check map[string]any
	for _, row := range checkPayload["check_runs"].([]any) {
		if row.(map[string]any)["name"] == "web" {
			check = row.(map[string]any)
		}
	}
	check["id"] = float64(49000001)
	check["head_sha"], check["status"], check["conclusion"], check["completed_at"] = r.Result, "completed", "success", clock.Add(-time.Minute).Format(time.RFC3339)
	r.Checks = []CandidateCheck{{Name: check["name"].(string), AppID: int64(check["app"].(map[string]any)["id"].(float64))}}
	f.root.Candidate = &r
	f.adapter.root = f.root
	r.Sweep.Authority = f.adapter.authorityDigest()
	f.root.Candidate = &r
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.root)
	a, err := NewAdapter(key.Public().(ed25519.PublicKey), raw, ed25519.Sign(key, raw), "")
	if err != nil {
		t.Fatal(err)
	}
	a.base, a.client = f.adapter.base, f.adapter.client
	seam := &candidateSeam{now: clock.Add(3 * time.Minute), checks: map[string]any{r.Result: checkPayload}}
	seam.key = key
	a.clock = func() time.Time { seam.mu.Lock(); defer seam.mu.Unlock(); return seam.now }
	f.adapter = a
	pr := f.pull
	pr.Number, pr.State, pr.Draft, pr.Merged = r.Pull, "open", false, false
	pr.User = Identity{ID: c.Owner.ID + 7, Login: "candidate-author"}
	pr.Head.Ref, pr.Head.SHA, pr.Head.Repo = r.SourceRef, r.Head, f.repo
	pr.Base.Ref, pr.Base.SHA, pr.Base.Repo = "main", r.Base, f.repo
	pr.MergeSHA = r.Result
	f.candidate = &pr
	f.candidateReviews = append([]reviewRecord{}, f.reviews...)
	f.candidateReviews[0].State = "APPROVED"
	f.candidateReviews[0].CommitID = r.Head
	f.candidateReviews[0].SubmittedAt = clock.Add(-2 * time.Minute)
	secondReview := f.candidateReviews[0]
	secondReview.ID++
	secondReview.User = provider
	f.candidateReviews = append(f.candidateReviews, secondReview)
	f.refs["heads/main"], f.refs["heads/"+r.SourceRef] = r.Base, r.Head
	f.commits[r.Result] = Commit{SHA: r.Result, Parent: r.Base, Code: true, Compatibility: true}
	f.policy[r.Result] = append([]byte{}, f.policy[f.root.CurrentControl]...)
	f.commits[r.Head] = Commit{SHA: r.Head, Parent: r.IntegrationFrom, Code: true, Compatibility: true}
	capture(t, "candidate-main-run", &seam.run)
	seam.run.ID = 47000001
	seam.run.WorkflowID = c.WorkflowID
	seam.run.Path = ProofWorkflow
	seam.run.HeadSHA = r.Result
	seam.run.HeadBranch = strings.TrimPrefix(dispatchRef(r.Sweep), "heads/")
	seam.run.Event = "workflow_dispatch"
	seam.run.Title = sweepTitle(r.Sweep)
	seam.run.CreatedAt = clock.Add(time.Minute)
	seam.run.UpdatedAt = clock.Add(2 * time.Minute)
	seam.ci = seam.run
	seam.ci.ID++
	seam.ci.WorkflowID = r.MainCI
	seam.ci.Path = ".github/workflows/ci.yml"
	seam.ci.HeadBranch = "main"
	seam.ci.HeadSHA = sha(104)
	seam.ci.Event = "push"
	seam.ci.CreatedAt = clock.Add(5 * time.Minute)
	seam.ci.UpdatedAt = clock.Add(20 * time.Minute)
	seam.tmux = seam.ci
	seam.tmux.ID++
	seam.tmux.WorkflowID = r.MainTmux
	seam.tmux.Path = ".github/workflows/tmux.yml"
	var captured struct{ Jobs []jobRecord }
	capture(t, "candidate-main-jobs", &captured)
	seam.ciJobs = captured.Jobs
	captured.Jobs = nil
	capture(t, "candidate-main-tmux-jobs", &captured)
	seam.tmuxJobs = captured.Jobs
	for n := range seam.ciJobs {
		j := &seam.ciJobs[n]
		j.RunID, j.Head, j.StartedAt, j.CompletedAt = seam.ci.ID, sha(104), clock.Add(6*time.Minute), clock.Add(20*time.Minute)
	}
	for n := range seam.tmuxJobs {
		j := &seam.tmuxJobs[n]
		j.RunID, j.Head, j.StartedAt, j.CompletedAt = seam.tmux.ID, sha(104), clock.Add(6*time.Minute), clock.Add(20*time.Minute)
	}
	for name, steps := range min {
		j := seam.ciJobs[0]
		j.ID = int64(48000000 + len(seam.jobs))
		j.CheckRunURL = fmt.Sprintf("https://api.github.com%s/check-runs/%d", a.prefix(), 50000000+len(seam.jobs))
		if name == "web" {
			j.CheckRunURL = "https://api.github.com" + a.prefix() + "/check-runs/49000001"
		}
		j.Name, j.Head, j.RunID, j.Labels = name, r.Result, seam.run.ID, c.RunnerLabels[name]
		j.StartedAt, j.CompletedAt = clock.Add(time.Minute), clock.Add(2*time.Minute)
		j.Steps = nil
		for _, name := range steps {
			j.Steps = append(j.Steps, stepRecord{Name: name, Status: "completed", Conclusion: "success"})
		}
		if name == "linux" {
			j.Steps = append(j.Steps, stepRecord{Name: "Upload frozen candidate builds", Status: "completed", Conclusion: "success"})
		}
		seam.jobs = append(seam.jobs, j)
	}
	var artifacts struct {
		Total     int `json:"total_count"`
		Artifacts []candidateArtifact
	}
	capture(t, "candidate-artifacts", &artifacts)
	if len(artifacts.Artifacts) != 1 {
		t.Fatal("actual artifact capture changed")
	}
	art := &artifacts.Artifacts[0]
	art.Name = "candidate-macos-" + r.Result
	art.WorkflowRun.ID, art.WorkflowRun.Head = seam.run.ID, r.Result
	art.CreatedAt = clock.Add(2 * time.Minute)
	seam.artifacts = artifacts
	f.gitServe = func(w http.ResponseWriter, req *http.Request) bool {
		seam.mu.Lock()
		prefix := a.prefix()
		path := req.URL.Path
		write := func(v any) { seam.requests++; json.NewEncoder(w).Encode(v) }
		if req.Method != "GET" {
			seam.requests++
			http.Error(w, "write forbidden", 405)
			seam.mu.Unlock()
			return true
		}
		if strings.HasPrefix(path, prefix+"/git/commits/") {
			rev := strings.TrimPrefix(path, prefix+"/git/commits/")
			if rev == r.Head {
				write(map[string]any{"sha": rev, "parents": []map[string]string{{"sha": r.IntegrationFrom}, {"sha": sha(109)}}, "tree": map[string]string{"sha": sha(110)}})
				seam.mu.Unlock()
				return true
			}
			if rev == r.Result || rev == sha(104) {
				tree := r.Tree
				if seam.mode == "wrong-tree" {
					tree = sha(999)
				}
				write(map[string]any{"sha": rev, "parents": []map[string]string{{"sha": r.Base}, {"sha": r.Head}}, "tree": map[string]string{"sha": tree}})
				seam.mu.Unlock()
				return true
			}
		}
		if path == prefix+"/commits/"+r.Head+"/pulls" {
			p := pr
			p.Number = 1199
			p.MergedAt = clock.Add(-time.Minute)
			p.MergeSHA = r.Head
			p.Head.SHA = sha(109)
			p.Base.Ref = "develop"
			if seam.mode == "manual-source" {
				write([]pullRecord{})
			} else {
				write([]pullRecord{p})
			}
			seam.mu.Unlock()
			return true
		}
		if path == prefix+"/pulls/1199/reviews" {
			reviews := append([]reviewRecord{}, f.candidateReviews...)
			for i := range reviews {
				reviews[i].CommitID = sha(109)
			}
			write(reviews)
			seam.mu.Unlock()
			return true
		}
		if path == prefix+"/actions/runs/47000001" || path == prefix+"/actions/runs/47000001/attempts/1" {
			v := seam.run
			if seam.mode == "rerun" {
				v.RunAttempt = 2
			}
			if seam.mode == "late" {
				v.UpdatedAt = r.ExpiresAt
			}
			write(v)
			seam.mu.Unlock()
			return true
		}
		if path == fmt.Sprintf("%s/actions/workflows/%d/runs", prefix, c.WorkflowID) {
			v := []runRecord{seam.run}
			if seam.mode == "duplicate-run" {
				v = append(v, seam.run)
				v[1].ID++
			}
			write(map[string]any{"total_count": len(v), "workflow_runs": v})
			seam.mu.Unlock()
			return true
		}
		if strings.HasSuffix(path, "/jobs") {
			var jobs []jobRecord
			switch {
			case strings.Contains(path, "47000001/"):
				jobs = append([]jobRecord{}, seam.jobs...)
			case strings.Contains(path, "47000002/"):
				jobs = append([]jobRecord{}, seam.ciJobs...)
			case strings.Contains(path, "47000003/"):
				jobs = append([]jobRecord{}, seam.tmuxJobs...)
			}
			if jobs != nil {
				if seam.mode == "skip" {
					jobs[0].Conclusion = "skipped"
				}
				if seam.mode == "wrong-job-head" {
					jobs[0].Head = sha(888)
				}
				if seam.mode == "upload-skipped" {
					for i := range jobs {
						if jobs[i].Name == "linux" {
							jobs[i].Steps = append([]stepRecord{}, jobs[i].Steps...)
							jobs[i].Steps[len(jobs[i].Steps)-1].Conclusion = "skipped"
						}
					}
				}
				write(map[string]any{"total_count": len(jobs), "jobs": jobs})
				seam.mu.Unlock()
				return true
			}
		}
		if strings.HasSuffix(path, "/artifacts") {
			if seam.onArtifacts != nil {
				callback := seam.onArtifacts
				seam.onArtifacts = nil
				seam.mu.Unlock()
				callback()
				seam.mu.Lock()
			}
			if seam.mode == "artifact-missing" {
				write(map[string]any{"total_count": 0, "artifacts": []any{}})
			} else {
				write(seam.artifacts)
			}
			seam.mu.Unlock()
			return true
		}
		if strings.HasSuffix(path, "/check-runs") {
			head := strings.TrimSuffix(strings.TrimPrefix(path, prefix+"/commits/"), "/check-runs")
			v := seam.checks[head]
			if seam.mode == "untrusted-check" {
				raw, _ := json.Marshal(v)
				var bad map[string]any
				json.Unmarshal(raw, &bad)
				for _, row := range bad["check_runs"].([]any) {
					item := row.(map[string]any)
					if item["name"] == "web" {
						item["id"] = float64(12345)
					}
				}
				v = bad
			}
			if seam.mode == "check-missing" {
				v = map[string]any{"total_count": 0, "check_runs": []any{}}
			}
			write(v)
			seam.mu.Unlock()
			return true
		}
		for _, run := range []runRecord{seam.ci, seam.tmux} {
			if path == fmt.Sprintf("%s/actions/workflows/%d/runs", prefix, run.WorkflowID) {
				if run.WorkflowID == r.MainCI && seam.onMain != nil {
					callback := seam.onMain
					seam.mu.Unlock()
					callback()
					seam.mu.Lock()
				}
				if seam.entered != nil {
					close(seam.entered)
					seam.entered = nil
					release := seam.release
					seam.mu.Unlock()
					release()
					seam.mu.Lock()
				}
				write(map[string]any{"total_count": 1, "workflow_runs": []runRecord{run}})
				seam.mu.Unlock()
				return true
			}
		}
		seam.mu.Unlock()
		return false
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "candidate.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.clock = a.clock
	return NewCoordinator(a, s), f, seam
}

func TestCandidateFrozenFullProofAndIndependentMain(t *testing.T) {
	c, f, seam := candidateFixture(t)
	ctx := context.Background()
	for _, op := range []string{"candidate-freeze", "candidate-verify"} {
		out, err := c.Execute(ctx, op, 1200, 0)
		if err != nil {
			t.Fatal(op, err)
		}
		if out.Candidate.PromotionAllowed || out.Candidate.UpgradeAllowed || out.Candidate.ReleaseAllowed {
			t.Fatal("evidence granted permission")
		}
	}
	before, err := candidateAt(ctx, c.store.db, 1200)
	if err != nil {
		t.Fatal(err)
	}
	if before.Evidence == nil || len(before.Evidence.Artifacts) != 1 {
		t.Fatal(before)
	}
	if _, err = c.Execute(ctx, "candidate-main", 1200, 0); err == nil {
		t.Fatal("unmerged candidate accepted")
	}
	seam.mu.Lock()
	seam.now = clock.Add(30 * time.Minute)
	seam.mu.Unlock()
	f.mu.Lock()
	f.candidate.State = "closed"
	f.candidate.Merged = true
	f.candidate.MergedAt = clock.Add(4 * time.Minute)
	f.candidate.MergeSHA = sha(104)
	f.refs["heads/main"] = sha(104)
	f.mu.Unlock()
	var checks map[string]any
	raw, _ := json.Marshal(seam.checks[sha(102)])
	json.Unmarshal(raw, &checks)
	var item map[string]any
	for _, row := range checks["check_runs"].([]any) {
		if row.(map[string]any)["name"] == "web" {
			item = row.(map[string]any)
		}
	}
	for _, job := range seam.ciJobs {
		if job.Name == "web" {
			parts := strings.Split(job.CheckRunURL, "/")
			id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			item["id"] = float64(id)
		}
	}
	item["head_sha"], item["completed_at"] = sha(104), clock.Add(20*time.Minute).Format(time.RFC3339)
	seam.checks[sha(104)] = checks
	out, err := c.Execute(ctx, "candidate-main", 1200, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Candidate.Record.Main.Merge != sha(104) || len(out.Candidate.Record.Main.Runs) != 2 || out.Candidate.Record.Main.Runs[0] == before.Evidence.Run || out.Candidate.UpgradeAllowed {
		t.Fatal(out)
	}
	if f.posts != 0 || f.creates != 0 {
		t.Fatal("read-only route wrote remote state")
	}
}

func TestCandidateRejectsChangedOrPartialEvidence(t *testing.T) {
	for _, scenario := range []string{"base", "head", "policy", "candidate-policy", "dependency", "before-control", "review", "provider", "manual-source", "expired", "wrong-tree", "rerun", "late", "duplicate-run", "skip", "wrong-job-head", "upload-skipped", "artifact-missing", "check-missing", "untrusted-check"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, seam := candidateFixture(t)
			ctx := context.Background()
			if _, err := c.Execute(ctx, "candidate-freeze", 1200, 0); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "base":
				f.refs["heads/main"] = sha(333)
			case "head":
				f.candidate.Head.SHA = sha(333)
			case "policy":
				f.policy[f.root.CurrentControl] = []byte("{}")
			case "candidate-policy":
				f.policy[sha(102)] = []byte("candidate cannot select policy")
			case "dependency":
				f.tamper = func(path string, out any) {
					if strings.HasSuffix(path, "/contents/go.mod") {
						record := out.(*contentRecord)
						record.Size = 0
						record.Content = ""
					}
				}
			case "before-control":
				seam.run.CreatedAt = clock.Add(-time.Minute)
			case "review":
				f.candidateReviews[0].State = "COMMENTED"
			case "provider":
				f.candidateReviews = f.candidateReviews[:1]
			case "expired":
				seam.now = c.adapter.root.Candidate.ExpiresAt
			default:
				seam.mode = scenario
			}
			if _, err := c.Execute(ctx, "candidate-verify", 1200, 0); err == nil {
				t.Fatal("invalid candidate accepted", scenario)
			}
			row, err := candidateAt(ctx, c.store.db, 1200)
			if err != nil || row.Status != "frozen" || row.Evidence != nil {
				t.Fatal("bad evidence persisted", row, err)
			}
		})
	}
}

func TestCandidateConcurrentFreezeRestartAndInvalidation(t *testing.T) {
	c, f, _ := candidateFixture(t)
	ctx := context.Background()
	var path string
	if err := c.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = c.store.clock
	second := NewCoordinator(c.adapter, other)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, co := range []*Coordinator{c, second} {
		wg.Add(1)
		go func(co *Coordinator) {
			defer wg.Done()
			_, err := co.Execute(ctx, "candidate-freeze", 1200, 0)
			errs <- err
		}(co)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	row, _ := candidateAt(ctx, other.db, 1200)
	var count int
	other.db.QueryRow("SELECT count(*) FROM candidate").Scan(&count)
	if count != 1 || row.Revision != 1 {
		t.Fatal("duplicate freeze", count, row)
	}
	// Authenticated responsible owner, not a supplied diagnosis alone.
	f.root.Dispatcher = f.root.Controls[f.root.CurrentControl].Owner
	if _, err = c.execute(ctx, "candidate-invalidate", 0, 0, "candidate changed; preserve history"); err != nil {
		t.Fatal(err)
	}
	if _, err = second.Execute(ctx, "candidate-verify", 1200, 0); err == nil {
		t.Fatal("invalidated candidate revived")
	}
	if _, err = second.Execute(ctx, "candidate-freeze", 1200, 0); err == nil {
		t.Fatal("same invalidated tuple replayed")
	}
	row, _ = candidateAt(ctx, other.db, 1200)
	if row.Status != "invalidated" {
		t.Fatal(row)
	}
}

func prepareCandidateMain(t *testing.T, c *Coordinator, f *githubFixture, seam *candidateSeam) {
	t.Helper()
	ctx := context.Background()
	for _, op := range []string{"candidate-freeze", "candidate-verify"} {
		if _, err := c.Execute(ctx, op, 1200, 0); err != nil {
			t.Fatal(op, err)
		}
	}
	seam.mu.Lock()
	seam.now = clock.Add(30 * time.Minute)
	seam.mu.Unlock()
	f.mu.Lock()
	f.candidate.State = "closed"
	f.candidate.Merged = true
	f.candidate.MergedAt = clock.Add(4 * time.Minute)
	f.candidate.MergeSHA = sha(104)
	f.refs["heads/main"] = sha(104)
	f.mu.Unlock()
	var checks map[string]any
	raw, _ := json.Marshal(seam.checks[sha(102)])
	json.Unmarshal(raw, &checks)
	var item map[string]any
	for _, row := range checks["check_runs"].([]any) {
		if row.(map[string]any)["name"] == "web" {
			item = row.(map[string]any)
		}
	}
	for _, job := range seam.ciJobs {
		if job.Name == "web" {
			parts := strings.Split(job.CheckRunURL, "/")
			id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			item["id"] = float64(id)
		}
	}
	item["head_sha"], item["completed_at"] = sha(104), clock.Add(20*time.Minute).Format(time.RFC3339)
	seam.checks[sha(104)] = checks
}

func TestCandidateMainRejectsPartialStaleOrUnrelatedProof(t *testing.T) {
	for _, scenario := range []string{"main-tree", "main-head", "main-event", "main-rerun", "main-skip", "main-step-skip", "main-moved", "late-review", "candidate-artifact-relabel"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, seam := candidateFixture(t)
			prepareCandidateMain(t, c, f, seam)
			switch scenario {
			case "main-tree":
				seam.mode = "wrong-tree"
			case "main-head":
				seam.ci.HeadSHA = sha(102)
			case "main-event":
				seam.ci.Event = "pull_request"
			case "main-rerun":
				seam.ci.RunAttempt = 2
			case "main-skip":
				seam.mode = "skip"
			case "main-step-skip":
				for i := range seam.ciJobs {
					if seam.ciJobs[i].Name == "test-macos" {
						for j := range seam.ciJobs[i].Steps {
							if seam.ciJobs[i].Steps[j].Name == "go test" {
								seam.ciJobs[i].Steps[j].Conclusion = "skipped"
							}
						}
					}
				}
			case "main-moved":
				f.refs["heads/main"] = sha(777)
			case "late-review":
				f.candidateReviews[0].SubmittedAt = f.candidate.MergedAt.Add(time.Second)
			case "candidate-artifact-relabel":
				seam.ci.ID = seam.run.ID
			}
			if _, err := c.Execute(context.Background(), "candidate-main", 1200, 0); err == nil {
				t.Fatal("bad main evidence accepted")
			}
			row, _ := candidateAt(context.Background(), c.store.db, 1200)
			if row.Main != nil || row.Status != "verified" {
				t.Fatal("main evidence copied", row)
			}
		})
	}
}

func TestCandidateInvalidationWinsAgainstInFlightVerification(t *testing.T) {
	c, f, seam := candidateFixture(t)
	ctx := context.Background()
	if _, err := c.Execute(ctx, "candidate-freeze", 1200, 0); err != nil {
		t.Fatal(err)
	}
	f.root.Dispatcher = f.root.Controls[f.root.CurrentControl].Owner
	seam.onArtifacts = func() {
		if _, err := c.execute(ctx, "candidate-invalidate", 0, 0, "rollback during proof collection"); err != nil {
			t.Error(err)
		}
	}
	if _, err := c.Execute(ctx, "candidate-verify", 1200, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("stale completion won", err)
	}
	row, _ := candidateAt(ctx, c.store.db, 1200)
	if row.Status != "invalidated" || row.Evidence != nil {
		t.Fatal(row)
	}
}

func TestCandidateCompetingMainObserversUseDurableCAS(t *testing.T) {
	c, f, seam := candidateFixture(t)
	prepareCandidateMain(t, c, f, seam)
	ctx := context.Background()
	var path string
	c.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path)
	other, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.clock = c.store.clock
	second := NewCoordinator(c.adapter, other)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	seam.onMain = func() { arrived <- struct{}{}; <-release }
	errs := make(chan error, 2)
	for _, co := range []*Coordinator{c, second} {
		go func(co *Coordinator) { _, err := co.Execute(ctx, "candidate-main", 1200, 0); errs <- err }(co)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("observer did not reach boundary")
		}
	}
	close(release)
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("both stale observers committed", success, conflict)
	}
	row, _ := candidateAt(ctx, other.db, 1200)
	if row.Status != "main_verified" || row.Revision != 3 {
		t.Fatal(row)
	}
}

func TestCandidateHistoricalStatusNeverOpensCLIGate(t *testing.T) {
	c, _, _ := candidateFixture(t)
	out, err := c.Execute(context.Background(), "candidate-freeze", 1200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	writeCLI(&b, out, nil)
	var response struct{ Closed bool }
	if err = json.Unmarshal([]byte(b.String()), &response); err != nil || !response.Closed {
		t.Fatal("evidence became a permission", b.String(), err)
	}
}

func TestCandidateVerifiedArtifactIdentityCannotBeReplaced(t *testing.T) {
	c, _, seam := candidateFixture(t)
	ctx := context.Background()
	for _, op := range []string{"candidate-freeze", "candidate-verify"} {
		if _, err := c.Execute(ctx, op, 1200, 0); err != nil {
			t.Fatal(op, err)
		}
	}
	before, _ := candidateAt(ctx, c.store.db, 1200)
	var replacement struct {
		Total     int `json:"total_count"`
		Artifacts []candidateArtifact
	}
	raw, _ := json.Marshal(seam.artifacts)
	json.Unmarshal(raw, &replacement)
	replacement.Artifacts[0].Digest = "sha256:" + Digest([]byte("different archive"))
	seam.artifacts = replacement
	if _, err := c.Execute(ctx, "candidate-verify", 1200, 0); err == nil {
		t.Fatal("frozen artifact digest replaced")
	}
	after, _ := candidateAt(ctx, c.store.db, 1200)
	if after.Revision != before.Revision || after.Evidence.Artifacts[0] != before.Evidence.Artifacts[0] {
		t.Fatal("frozen evidence changed")
	}
}

func TestCandidateNewSignedRequestRetainsInvalidatedHistoryAndKey(t *testing.T) {
	c, f, seam := candidateFixture(t)
	ctx := context.Background()
	if _, err := c.Execute(ctx, "candidate-freeze", 1200, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.execute(ctx, "candidate-invalidate", 0, 0, "prepare a distinct candidate"); err != nil {
		t.Fatal(err)
	}
	root := c.adapter.root
	raw, _ := json.Marshal(root)
	json.Unmarshal(raw, &root)
	root.Candidate.Sweep.ID = "candidate-next"
	resign := func(key ed25519.PrivateKey) *Adapter {
		raw, _ := json.Marshal(root)
		a, err := NewAdapter(key.Public().(ed25519.PublicKey), raw, ed25519.Sign(key, raw), "")
		if err != nil {
			t.Fatal(err)
		}
		a.base, a.client, a.clock = c.adapter.base, c.adapter.client, c.adapter.clock
		return a
	}
	_, different, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c.adapter = resign(different)
	if _, err = c.Execute(ctx, "candidate-freeze", 1200, 0); err == nil {
		t.Fatal("new key replaced retained history")
	}
	c.adapter = resign(seam.key)
	f.adapter = c.adapter
	if _, err = c.Execute(ctx, "candidate-freeze", 1200, 0); err != nil {
		t.Fatal(err)
	}
	var total, invalidated int
	c.store.db.QueryRow("SELECT count(*) FROM candidate").Scan(&total)
	c.store.db.QueryRow("SELECT count(*) FROM candidate WHERE status='invalidated'").Scan(&invalidated)
	if total != 2 || invalidated != 1 {
		t.Fatal("history erased", total, invalidated)
	}
}

func TestCandidateInvalidationNeedsRealOwnerAndUnexpiredRoot(t *testing.T) {
	for _, scenario := range []string{"other-principal", "expired-root"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, _ := candidateFixture(t)
			ctx := context.Background()
			if _, err := c.Execute(ctx, "candidate-freeze", 1200, 0); err != nil {
				t.Fatal(err)
			}
			if scenario == "other-principal" {
				f.root.Dispatcher = Identity{ID: 999, Login: "other"}
			} else {
				c.adapter.root.ExpiresAt = clock
			}
			if _, err := c.execute(ctx, "candidate-invalidate", 0, 0, "text alone is not authority"); err == nil {
				t.Fatal("unauthorized invalidation")
			}
			record, _ := candidateAt(ctx, c.store.db, 1200)
			if record.Status != "frozen" {
				t.Fatal(record)
			}
		})
	}
}
