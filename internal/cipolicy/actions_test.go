package cipolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func capture(t *testing.T, name string, out any) {
	t.Helper()
	b, err := os.ReadFile("testdata/github/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

// This HTTP backend derives records from captured GitHub payloads. Explicitly
// mutated positive variants test the adapter; they are NOT live approvals.
type githubFixture struct {
	mu                                                       sync.Mutex
	adapter                                                  *Adapter
	root                                                     AuthorityRoot
	policy                                                   map[string][]byte
	workflow                                                 []byte
	repo                                                     repoRecord
	pull                                                     pullRecord
	reviews                                                  []reviewRecord
	run                                                      runRecord
	jobs                                                     []jobRecord
	head                                                     string
	commits                                                  map[string]Commit
	refs                                                     map[string]string
	posts, creates, historyReads                             int
	lost, delay, preflightFailure, duplicate, incompleteJobs bool
	unrelatedRuns                                            int
	tamper                                                   func(string, any)
}

func fixture(t *testing.T) *githubFixture {
	t.Helper()
	f := &githubFixture{policy: map[string][]byte{}, head: sha(100), commits: map[string]Commit{}, refs: map[string]string{}}
	capture(t, "repository", &f.repo)
	capture(t, "pull", &f.pull)
	capture(t, "reviews", &f.reviews)
	capture(t, "run", &f.run)
	var jobs struct{ Jobs []jobRecord }
	capture(t, "full-jobs", &jobs)
	f.jobs = jobs.Jobs
	var content contentRecord
	capture(t, "content", &content)
	f.workflow, _ = base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	owner := f.reviews[0].User
	requirement := ControlRequirement{Pull: 1068, Head: sha(11), Owner: owner, Reviewers: []Identity{owner}, WorkflowID: f.run.WorkflowID, WorkflowPath: f.run.Path, ExecutionFiles: map[string]string{f.run.Path: Digest(f.workflow)}, LinuxJobs: []string{"test"}, MacOSJobs: []string{"test-macos"}, CompatibilityJobs: []string{"compatibility"}}
	requirement.RequiredSteps = map[string][]string{}
	requirement.RunnerLabels = map[string][]string{}
	requirement.LinuxJobs = []string{"changes", "check", "web", "build-macos", "test"}
	for _, job := range f.jobs {
		requirement.RunnerLabels[job.Name] = job.Labels
		if len(job.Steps) > 0 {
			requirement.RequiredSteps[job.Name] = []string{job.Steps[0].Name}
		}
		if job.Name == "test" {
			requirement.RequiredSteps[job.Name] = []string{"Run go test -timeout 30m ./..."}
		}
		if job.Name == "test-macos" {
			requirement.RequiredSteps[job.Name] = []string{"go test"}
		}
	}
	requirement.RunnerLabels["compatibility"] = []string{"ubuntu-latest"}
	requirement.RequiredSteps["compatibility"] = []string{"compatibility suite"}
	for _, revision := range []string{sha(1), sha(2)} {
		p := validPolicy()
		p.Owner = owner.Login
		p.Limits.MaxChanges = 2
		if revision == sha(2) {
			p.Profile = "stable"
			p.RollbackRevision = sha(1)
		}
		b, _ := json.Marshal(p)
		f.policy[revision] = b
	}
	other := requirement
	other.Pull = 1069
	other.Head = sha(12)
	other.PolicyDigest = Digest(f.policy[sha(2)])
	requirement.PolicyDigest = Digest(f.policy[sha(1)])
	f.root = AuthorityRoot{MaxRequests: 64, RiskReviewers: []Identity{owner}, Version: 1, RepositoryID: f.repo.ID, Repository: f.repo.FullName, ControlRef: "main", IntegrationRef: "develop", Dispatcher: f.run.Actor, CurrentControl: sha(1), ExpiresAt: clock.Add(24 * time.Hour), Controls: map[string]ControlRequirement{sha(1): requirement, sha(2): other}}
	key, pubErr := func() (ed25519.PrivateKey, error) { _, k, e := ed25519.GenerateKey(rand.Reader); return k, e }()
	if pubErr != nil {
		t.Fatal(pubErr)
	}
	payload, _ := json.Marshal(f.root)
	a, err := NewAdapter(key.Public().(ed25519.PublicKey), payload, ed25519.Sign(key, payload), "")
	if err != nil {
		t.Fatal(err)
	}
	a.clock = func() time.Time { return clock }
	f.adapter = a
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	a.base = server.URL
	a.client = server.Client()
	return f
}

func (f *githubFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	prefix := "/repos/" + f.root.Repository
	write := func(value any) {
		if f.tamper != nil {
			f.tamper(p, value)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			panic(err)
		}
	}
	if p == prefix {
		v := f.repo
		write(&v)
		return
	}
	if p == "/user" || strings.HasPrefix(p, "/users/") {
		if f.preflightFailure && p == "/user" {
			http.Error(w, "preflight", 503)
			return
		}
		v := f.root.Dispatcher
		if p != "/user" {
			v = f.root.Controls[f.root.CurrentControl].Owner
		}
		write(&v)
		return
	}
	for revision, c := range f.root.Controls {
		if p == fmt.Sprintf("%s/pulls/%d", prefix, c.Pull) {
			v := f.pull
			v.Number = c.Pull
			v.Merged = true
			v.MergedAt = clock
			v.MergeSHA = revision
			v.Head.SHA = c.Head
			v.Head.Repo = f.repo
			v.Base.Repo = f.repo
			v.Base.Ref = "main"
			v.User = Identity{ID: c.Owner.ID + 1, Login: "candidate"}
			write(&v)
			return
		}
		if p == fmt.Sprintf("%s/pulls/%d/reviews", prefix, c.Pull) {
			v := append([]reviewRecord{}, f.reviews...)
			v[0].State = "APPROVED"
			v[0].CommitID = c.Head
			v[0].SubmittedAt = clock.Add(-time.Minute)
			write(&v)
			return
		}
	}
	if strings.HasPrefix(p, prefix+"/contents/") {
		revision := r.URL.Query().Get("ref")
		name := strings.TrimPrefix(p, prefix+"/contents/")
		b := f.workflow
		if name == PolicyPath {
			var ok bool
			b, ok = f.policy[revision]
			if !ok {
				http.NotFound(w, r)
				return
			}
		}
		v := contentRecord{Type: "file", Encoding: "base64", Size: len(b), Content: base64.StdEncoding.EncodeToString(b)}
		write(&v)
		return
	}
	if strings.HasPrefix(p, prefix+"/git/ref/") {
		ref := strings.TrimPrefix(p, prefix+"/git/ref/")
		head := f.head
		if ref != "heads/develop" {
			var ok bool
			head, ok = f.refs[ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
		}
		v := struct{ Object struct{ SHA string } }{}
		v.Object.SHA = head
		write(&v)
		return
	}
	if p == prefix+"/git/refs" && r.Method == "POST" {
		var v struct{ Ref, SHA string }
		json.NewDecoder(r.Body).Decode(&v)
		ref := strings.TrimPrefix(v.Ref, "refs/")
		if _, ok := f.refs[ref]; ok {
			http.Error(w, "exists", 422)
			return
		}
		f.refs[ref] = v.SHA
		f.creates++
		w.WriteHeader(201)
		return
	}
	if strings.HasPrefix(p, prefix+"/git/commits/") {
		f.historyReads++
		sha := strings.TrimPrefix(p, prefix+"/git/commits/")
		c, ok := f.commits[sha]
		if !ok {
			http.NotFound(w, r)
			return
		}
		v := commitRecord{SHA: sha}
		v.Parents = append(v.Parents, struct{ SHA string }{c.Parent})
		write(&v)
		return
	}
	if strings.Contains(p, "/commits/") && strings.HasSuffix(p, "/pulls") {
		f.historyReads++
		sha := strings.TrimSuffix(strings.TrimPrefix(p, prefix+"/commits/"), "/pulls")
		c, ok := f.commits[sha]
		if !ok {
			http.NotFound(w, r)
			return
		}
		v := f.pull
		v.MergedAt = c.IntegratedAt
		v.MergeSHA = c.SHA
		v.Base.Ref = "develop"
		v.Base.Repo = f.repo
		write(&[]pullRecord{v})
		return
	}
	if strings.HasPrefix(p, prefix+"/compare/") {
		f.historyReads++
		pair := strings.Split(strings.TrimPrefix(p, prefix+"/compare/"), "...")
		c, ok := f.commits[pair[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		name := "docs/note.md"
		if c.Code {
			name = "internal/version/version.go"
		}
		if c.Compatibility {
			name = "internal/holder/tmux/holder.go"
		}
		v := map[string]any{"base_commit": map[string]string{"sha": pair[0]}, "merge_base_commit": map[string]string{"sha": pair[0]}, "status": "ahead", "files": []map[string]string{{"filename": name, "status": "modified"}}}
		write(v)
		return
	}
	workflow := f.root.Controls[f.root.CurrentControl].WorkflowID
	if p == fmt.Sprintf("%s/actions/workflows/%d/dispatches", prefix, workflow) && r.Method == "POST" {
		var request struct {
			Ref    string
			Inputs map[string]string
		}
		json.NewDecoder(r.Body).Decode(&request)
		f.posts++
		f.run.ID++
		f.run.HeadSHA = f.refs["heads/"+request.Ref]
		f.run.Title = request.Inputs["title"]
		f.run.Event = "workflow_dispatch"
		f.run.Actor = f.root.Dispatcher
		f.run.TriggeringActor = f.root.Dispatcher
		f.run.Status = "completed"
		f.run.Conclusion = "success"
		if f.lost {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				connection.Close()
			}
			return
		}
		w.WriteHeader(204)
		return
	}
	if p == fmt.Sprintf("%s/actions/workflows/%d/runs", prefix, workflow) {
		runs := []runRecord{}
		if f.posts > 0 && !f.delay {
			runs = append(runs, f.run)
			if f.duplicate {
				extra := f.run
				extra.ID++
				runs = append(runs, extra)
			}
		}
		total := len(runs)
		if f.unrelatedRuns > 0 {
			branch := r.URL.Query().Get("branch")
			frozen := false
			for ref, head := range f.refs {
				if branch == strings.TrimPrefix(ref, "heads/") && r.URL.Query().Get("head_sha") == head {
					frozen = true
				}
			}
			if !frozen {
				total += f.unrelatedRuns
			}
		}
		write(map[string]any{"total_count": total, "workflow_runs": runs})
		return
	}
	if strings.Contains(p, "/actions/runs/") {
		if strings.HasSuffix(p, "/jobs") {
			jobs := append([]jobRecord{}, f.jobs...)
			for i := range jobs {
				jobs[i].RunID = f.run.ID
				jobs[i].Attempt = 1
				jobs[i].Head = f.run.HeadSHA
				jobs[i].Status = "completed"
				jobs[i].Conclusion = "success"
				jobs[i].Steps = append([]stepRecord{}, jobs[i].Steps...)
				for k := range jobs[i].Steps {
					jobs[i].Steps[k].Status = "completed"
					jobs[i].Steps[k].Conclusion = "success"
				}
			}
			// Keep captured required Linux/macOS job names, add an explicit
			// mutated compatibility record for this test-only combined workflow.
			comp := jobs[0]
			comp.ID++
			comp.Name = "compatibility"
			comp.Labels = []string{"ubuntu-latest"}
			comp.Steps = []stepRecord{{Name: "compatibility suite", Status: "completed", Conclusion: "success"}}
			jobs = append(jobs, comp)
			total := len(jobs)
			if f.incompleteJobs {
				total++
			}
			write(map[string]any{"total_count": total, "jobs": jobs})
			return
		}
		v := f.run
		write(&v)
		return
	}
	http.NotFound(w, r)
}

func TestSignedRootAndActualReviewRequirements(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	if _, err := f.adapter.control(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(string, any){
		func(p string, v any) {
			if r, ok := v.(*repoRecord); ok {
				r.ID++
			}
		},
		func(p string, v any) {
			if r, ok := v.(*pullRecord); ok {
				r.Merged = false
			}
		},
		func(p string, v any) {
			if r, ok := v.(*[]reviewRecord); ok {
				(*r)[0].State = "COMMENTED"
			}
		},
		func(p string, v any) {
			if r, ok := v.(*[]reviewRecord); ok {
				(*r)[0].CommitID = sha(999)
			}
		},
		func(p string, v any) {
			if r, ok := v.(*[]reviewRecord); ok {
				(*r)[0].User.ID++
			}
		},
		func(p string, v any) {
			if r, ok := v.(*contentRecord); ok {
				r.Content = base64.StdEncoding.EncodeToString([]byte("candidate policy"))
				r.Size = len("candidate policy")
			}
		},
	} {
		f.mu.Lock()
		f.tamper = mutate
		f.mu.Unlock()
		if _, err := f.adapter.control(ctx); err == nil {
			t.Fatal("input declarations replaced actual approval/source evidence")
		}
	}
	f.mu.Lock()
	f.tamper = nil
	f.mu.Unlock()
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	payload, _ := json.Marshal(f.root)
	if _, err := NewAdapter(key, payload, make([]byte, 64), ""); err == nil {
		t.Fatal("unsigned candidate root accepted")
	}
}

func TestRunProvenanceRejectsDraftAndEveryIdentityMismatch(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	c, err := f.adapter.control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sweep := Sweep{ID: "fixed nonce", From: sha(100), Target: f.run.HeadSHA, Control: c.revision, Digest: c.selection.digest, Authority: c.authority, CompatibilityRequired: true}
	if _, err := f.adapter.completion(ctx, c, sweep, f.run.ID, 1); err == nil {
		t.Fatal("captured draft pull_request became full proof")
	}
	f.mu.Lock()
	f.run.Event = "workflow_dispatch"
	f.run.Title = sweepTitle(sweep)
	f.run.Status = "completed"
	f.run.Conclusion = "success"
	f.mu.Unlock()
	proof, err := f.adapter.completion(ctx, c, sweep, f.run.ID, 1)
	if err != nil || proof.MacOS != "success" {
		t.Fatal(proof, err)
	}
	for _, mutate := range []func(*runRecord){func(r *runRecord) { r.Repository.ID++ }, func(r *runRecord) { r.HeadRepository.ID++ }, func(r *runRecord) { r.RunAttempt++ }, func(r *runRecord) { r.HeadSHA = sha(999) }, func(r *runRecord) { r.WorkflowID++ }, func(r *runRecord) { r.Path = "candidate.yml" }, func(r *runRecord) { r.Event = "pull_request" }, func(r *runRecord) { r.Title = "candidate title" }, func(r *runRecord) { r.Actor.ID++ }, func(r *runRecord) { r.TriggeringActor.ID++ }} {
		f.mu.Lock()
		f.tamper = func(_ string, v any) {
			if r, ok := v.(*runRecord); ok {
				mutate(r)
			}
		}
		f.mu.Unlock()
		if _, err := f.adapter.completion(ctx, c, sweep, f.run.ID, 1); err == nil {
			t.Fatal("wrong provenance accepted")
		}
	}
	f.mu.Lock()
	f.tamper = func(_ string, v any) {
		if m, ok := v.(map[string]any); ok {
			if jobs, ok := m["jobs"].([]jobRecord); ok {
				for i := range jobs {
					if jobs[i].Name == "test-macos" {
						jobs[i].Conclusion = "skipped"
					}
				}
			}
		}
	}
	f.mu.Unlock()
	proof, err = f.adapter.completion(ctx, c, sweep, f.run.ID, 1)
	if err != nil || proof.MacOS == "success" {
		t.Fatal("skipped full job credited", proof, err)
	}
	f.mu.Lock()
	f.tamper = nil
	f.incompleteJobs = true
	f.mu.Unlock()
	if _, err := f.adapter.completion(ctx, c, sweep, f.run.ID, 1); err == nil {
		t.Fatal("incomplete job pages credited")
	}
}

var liveGitHub = flag.Bool("live-github", false, "read-only public GitHub identity/history/draft rejection probe")

func TestLiveGitHubReadOnly(t *testing.T) {
	if !*liveGitHub {
		t.Skip("explicit read-only live probe")
	}
	// This is NOT authority bootstrap or green release/main proof. It verifies
	// actual API parsing/identity/history and rejects a known partial PR run.
	a := &Adapter{root: AuthorityRoot{RepositoryID: 1454788309, Repository: "yousan/hataori", ControlRef: "main", IntegrationRef: "main", ExpiresAt: time.Now().Add(time.Hour)}, base: "https://api.github.com", client: &http.Client{Timeout: 30 * time.Second}, clock: time.Now}
	ctx := context.Background()
	if err := a.repository(ctx); err != nil {
		t.Fatal(err)
	}
	var object commitRecord
	if err := a.request(ctx, "GET", a.prefix()+"/git/commits/8e051963929541ad126f9fff3c6b9c32dc4e58b7", nil, &object); err != nil || object.SHA != "8e051963929541ad126f9fff3c6b9c32dc4e58b7" || len(object.Parents) == 0 {
		t.Fatal(object, err)
	}
	var merged []pullRecord
	if err := a.request(ctx, "GET", a.prefix()+"/commits/8e051963929541ad126f9fff3c6b9c32dc4e58b7/pulls?per_page=100", nil, &merged); err != nil || len(merged) != 1 || merged[0].MergedAt.IsZero() || merged[0].MergeSHA != object.SHA {
		t.Fatal(merged, err)
	}
	if _, err := a.history(ctx, "b9e6c4fcd4f43996a7340d1e36916bda3dada04f", object.SHA); err == nil {
		t.Fatal("undeclared high-risk review authority accepted")
	}
	s := selected(t, validPolicy())
	c := verifiedControl{selection: s, revision: s.revision, requirement: ControlRequirement{WorkflowID: 415474141, WorkflowPath: ".github/workflows/ci.yml"}}
	_, err := a.run(ctx, c, Sweep{Target: "33e45270d32e94b832f3ce2ea1ae55ca87ac1e9b", Control: s.revision, Digest: s.digest}, 46093553816, 1)
	if err == nil {
		t.Fatal("actual draft run credited")
	}
	t.Log("actual repository, immutable ancestry and GitHub merge time verified; undeclared authority and actual draft run rejected; no approval, dispatch or coverage credit")
}

func TestSuccessRequiresExecutedStepsRunnerAndCompleteScope(t *testing.T) {
	for _, scenario := range []string{"skipped-step", "missing-step", "wrong-runner", "no-runner", "missing-job", "unknown-job"} {
		t.Run(scenario, func(t *testing.T) {
			f := fixture(t)
			ctx := context.Background()
			c, err := f.adapter.control(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s := Sweep{ID: "executed scope", From: sha(100), Target: f.run.HeadSHA, Control: c.revision, Digest: c.selection.digest, Authority: c.authority, CompatibilityRequired: true}
			f.run.Event = "workflow_dispatch"
			f.run.Title = sweepTitle(s)
			f.run.Status = "completed"
			f.run.Conclusion = "success"
			f.tamper = func(_ string, v any) {
				m, ok := v.(map[string]any)
				if !ok {
					return
				}
				jobs, ok := m["jobs"].([]jobRecord)
				if !ok {
					return
				}
				for i := range jobs {
					if jobs[i].Name != "test-macos" {
						continue
					}
					switch scenario {
					case "skipped-step":
						for k := range jobs[i].Steps {
							if jobs[i].Steps[k].Name == "go test" {
								jobs[i].Steps[k].Conclusion = "skipped"
							}
						}
					case "missing-step":
						jobs[i].Steps = nil
					case "wrong-runner":
						jobs[i].Labels = []string{"ubuntu-latest"}
					case "no-runner":
						jobs[i].RunnerID = 0
					case "missing-job":
						jobs = append(jobs[:i], jobs[i+1:]...)
						m["total_count"] = len(jobs)
					case "unknown-job":
						jobs[i].Name = "candidate-success"
					}
					break
				}
				m["jobs"] = jobs
			}
			proof, err := f.adapter.completion(ctx, c, s, f.run.ID, 1)
			if err == nil && proof.MacOS == "success" {
				t.Fatal("unexecuted or unapproved scope credited", proof)
			}
		})
	}
}

func TestHistoryUsesImmutableMergeClockAndFailsClosed(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	f.adapter.clock = func() time.Time { return clock.Add(time.Hour) }
	h := history(1, clock)
	f.commits[h.Commits[0].SHA] = h.Commits[0]
	got, err := f.adapter.history(ctx, h.Root, h.Commits[0].SHA)
	if err != nil || !got.Commits[0].IntegratedAt.Equal(clock) {
		t.Fatal(got, err)
	}
	for _, scenario := range []string{"wrong-repo", "wrong-ref", "wrong-merge", "future-time", "truncated-diff", "unknown-status", "high-risk-without-review"} {
		t.Run(scenario, func(t *testing.T) {
			f.tamper = func(p string, v any) {
				if prs, ok := v.(*[]pullRecord); ok {
					switch scenario {
					case "wrong-repo":
						(*prs)[0].Base.Repo.ID++
					case "wrong-ref":
						(*prs)[0].Base.Ref = "candidate"
					case "wrong-merge":
						(*prs)[0].MergeSHA = sha(999)
					case "future-time":
						(*prs)[0].MergedAt = clock.Add(2 * time.Hour)
					}
				}
				if m, ok := v.(map[string]any); ok && strings.Contains(p, "/compare/") {
					switch scenario {
					case "truncated-diff":
						m["files"] = make([]map[string]string, 300)
					case "unknown-status":
						m["files"] = []map[string]string{{"filename": "README.md", "status": "unknown"}}
					case "high-risk-without-review":
						m["files"] = []map[string]string{{"filename": "internal/plugin/authority.go", "status": "modified"}}
					}
				}
			}
			got, err := f.adapter.history(ctx, h.Root, h.Commits[0].SHA)
			// An unknown diff entry escalates to high risk and needs an actual
			// exact-head review; the captured COMMENTED review cannot supply it.
			if err == nil {
				t.Fatal("unproved history/authority accepted", got)
			}
		})
	}
}
