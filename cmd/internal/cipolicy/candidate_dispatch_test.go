package cipolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Real captured run/review/job/artifact shapes come from candidateFixture.
// These explicit server mutations model transport/crash boundaries; no fixture
// result is a claim that the inactive real workflow was dispatched or approved.
type candidateHTTP struct {
	mu                       sync.Mutex
	posts                    int
	visible, lost, duplicate bool
	onRef                    func()
	inputs                   map[string]string
}

func (h *candidateHTTP) count() int { h.mu.Lock(); defer h.mu.Unlock(); return h.posts }

func candidateDispatchFixture(t *testing.T) (*Coordinator, *githubFixture, *candidateSeam, *candidateHTTP) {
	t.Helper()
	c, f, seam := candidateFixture(t)
	seam.now = clock.Add(30 * time.Second)
	h := &candidateHTTP{visible: true}
	var capturedWorkflow map[string]any
	capture(t, "workflow", &capturedWorkflow)
	original := f.gitServe
	prefix := c.adapter.prefix()
	workflow := c.adapter.root.Controls[c.adapter.root.CurrentControl].WorkflowID
	list := fmt.Sprintf("%s/actions/workflows/%d/runs", prefix, workflow)
	dispatch := fmt.Sprintf("%s/actions/workflows/%d/dispatches", prefix, workflow)
	f.gitServe = func(w http.ResponseWriter, r *http.Request) bool {
		p := r.URL.Path
		if p == prefix+"/git/ref/"+dispatchRef(c.adapter.root.Candidate.Sweep) {
			h.mu.Lock()
			hook := h.onRef
			h.mu.Unlock()
			if hook != nil {
				hook()
			}
		}
		if r.Method == "POST" && p == prefix+"/git/refs" {
			return false
		}
		if r.Method == "POST" && p == dispatch {
			var body struct {
				Ref    string
				Inputs map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "bad request", 400)
				return true
			}
			record, e := candidateAt(r.Context(), c.store.db, 1200)
			i, ie := candidateIntent(r.Context(), c.store.db, record)
			if e != nil || ie != nil || i.Status != "uncertain" {
				t.Errorf("POST preceded durable uncertainty: %v %v %v", i, e, ie)
			}
			want := c.adapter.root.Candidate.Sweep
			if body.Ref != strings.TrimPrefix(dispatchRef(want), "heads/") || body.Inputs["target"] != want.Target || body.Inputs["from"] != want.From || body.Inputs["intent"] != want.ID || body.Inputs["authority"] != want.Authority || body.Inputs["control"] != want.Control || body.Inputs["digest"] != want.Digest || body.Inputs["title"] != sweepTitle(want) || body.Inputs["purpose"] != "sweep" || body.Inputs["scope"] != "true" {
				t.Error("actual dispatch request escaped frozen tuple", body)
			}
			f.mu.Lock()
			target := f.refs["heads/"+body.Ref]
			f.mu.Unlock()
			h.mu.Lock()
			seam.mu.Lock()
			seam.run.Title, seam.run.HeadBranch, seam.run.HeadSHA = body.Inputs["title"], body.Ref, target
			seam.run.Status, seam.run.Conclusion = "in_progress", ""
			seam.run.CreatedAt, seam.run.UpdatedAt = seam.now, seam.now
			seam.mu.Unlock()
			h.posts++
			h.inputs = body.Inputs
			lost := h.lost
			h.mu.Unlock()
			if lost {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
				} else {
					conn.Close()
				}
			} else {
				w.WriteHeader(204)
			}
			return true
		}
		if p == fmt.Sprintf("%s/actions/workflows/%d", prefix, workflow) && r.Method == "GET" {
			v := map[string]any{}
			for k, item := range capturedWorkflow {
				v[k] = item
			}
			v["id"], v["path"], v["state"] = workflow, ProofWorkflow, "active"
			if f.tamper != nil {
				f.tamper(p, v)
			}
			json.NewEncoder(w).Encode(v)
			return true
		}
		seam.mu.Lock()
		currentRun := seam.run
		seam.mu.Unlock()
		if p == fmt.Sprintf("%s/actions/runs/%d/attempts/1", prefix, currentRun.ID) {
			originalAttempt := currentRun
			originalAttempt.RunAttempt = 1
			json.NewEncoder(w).Encode(originalAttempt)
			return true
		}
		if p == fmt.Sprintf("%s/actions/runs/%d", prefix, currentRun.ID) {
			json.NewEncoder(w).Encode(currentRun)
			return true
		}
		if p == list {
			h.mu.Lock()
			visible := h.visible && h.posts > 0
			duplicate := h.duplicate
			h.mu.Unlock()
			seam.mu.Lock()
			run := seam.run
			seam.mu.Unlock()
			runs := []runRecord{}
			if visible {
				runs = append(runs, run)
				if duplicate {
					run.ID++
					runs = append(runs, run)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
			return true
		}
		return original(w, r)
	}
	return c, f, seam, h
}
func prepareCandidateDispatch(t *testing.T, c *Coordinator) {
	t.Helper()
	for _, op := range []string{"candidate-freeze", "candidate-prepare"} {
		out, err := c.Execute(context.Background(), op, 1200, 0)
		if err != nil {
			t.Fatal(op, err)
		}
		if out.Candidate == nil || out.Candidate.PromotionAllowed || out.Candidate.ReleaseAllowed || out.Candidate.UpgradeAllowed || out.Route != nil || out.State != nil {
			t.Fatal("candidate request granted unrelated authority", out)
		}
	}
}
func candidateTwin(t *testing.T, c *Coordinator) *Coordinator {
	t.Helper()
	var file string
	if err := c.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&file); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.clock = c.store.clock
	return NewCoordinator(c.adapter, s)
}
func finishCandidate(seam *candidateSeam) {
	seam.mu.Lock()
	defer seam.mu.Unlock()
	seam.now = clock.Add(3 * time.Minute)
	seam.run.Status, seam.run.Conclusion = "completed", "success"
	seam.run.UpdatedAt = clock.Add(2 * time.Minute)
}

func TestCandidateActualDispatchRestartConnectsFullEvidence(t *testing.T) {
	c, _, seam, h := candidateDispatchFixture(t)
	ctx := context.Background()
	prepareCandidateDispatch(t, c)
	if h.count() != 0 {
		t.Fatal("prepare dispatched")
	}
	if _, err := c.store.Read(ctx); err == nil {
		t.Fatal("candidate preparation initialized integration lane")
	}
	out, err := c.Execute(ctx, "candidate-pump", 1200, 0)
	if err != nil || out.Intent == nil || out.Intent.Status != "bound" || out.Intent.Attempt != 1 || h.count() != 1 {
		t.Fatal(out, err, h.count())
	}
	id := out.Intent.RunID
	other := candidateTwin(t, c)
	if _, err = other.Execute(ctx, "candidate-verify", 1200, 0); err == nil {
		t.Fatal("running receipt became full proof")
	}
	finishCandidate(seam)
	out, err = other.Execute(ctx, "candidate-verify", 1200, 0)
	if err != nil || out.Candidate.Record.Evidence.Run != id || out.Candidate.Dispatch.RunID != id || out.Candidate.PromotionAllowed || out.Candidate.ReleaseAllowed || out.Candidate.UpgradeAllowed {
		t.Fatal("restart disconnected original receipt", out, err)
	}
	var b bytes.Buffer
	writeCLI(&b, out, nil)
	var closed struct{ Closed bool }
	json.Unmarshal(b.Bytes(), &closed)
	if !closed.Closed {
		t.Fatal("successful proof/dispatch opened CLI gate")
	}
	if _, err = other.Execute(ctx, "candidate-pump", 1200, 0); err != nil || h.count() != 1 {
		t.Fatal("bound proof resent", err, h.count())
	}
}

func TestCandidateLostResponseAndPreSendCrashNeverResend(t *testing.T) {
	for _, scenario := range []string{"lost-response", "crash-before-post", "prepared-restart"} {
		t.Run(scenario, func(t *testing.T) {
			c, _, seam, h := candidateDispatchFixture(t)
			ctx := context.Background()
			prepareCandidateDispatch(t, c)
			h.visible = false
			switch scenario {
			case "lost-response":
				h.lost = true
				if _, err := c.Execute(ctx, "candidate-pump", 1200, 0); !errors.Is(err, ErrAmbiguous) {
					t.Fatal(err)
				}
			case "crash-before-post":
				// The process can die after the durable claim, before the HTTP send.
				if _, err := c.store.db.Exec("UPDATE candidate_intent SET status='uncertain'"); err != nil {
					t.Fatal(err)
				}
			}
			other := candidateTwin(t, c)
			before := h.count()
			if scenario == "prepared-restart" {
				h.visible = true
				if _, err := other.Execute(ctx, "candidate-pump", 1200, 0); err != nil || h.count() != 1 {
					t.Fatal(err, h.count())
				}
				return
			}
			for _, op := range []string{"candidate-dispatch-reconcile", "candidate-pump"} {
				if _, err := other.Execute(ctx, op, 1200, 0); !errors.Is(err, ErrAmbiguous) {
					t.Fatal(op, err)
				}
			}
			if h.count() != before {
				t.Fatal("uncertain dispatch repeated POST", before, h.count())
			}
			if _, err := other.execute(ctx, "candidate-invalidate", 0, 0, "do not abandon sole unresolved proof"); !errors.Is(err, ErrAmbiguous) {
				t.Fatal("uncertainty invalidated", err)
			}
			if scenario == "crash-before-post" {
				return
			} // Unknown remains quarantined, not exactly-once.
			h.mu.Lock()
			h.visible = true
			h.lost = false
			h.mu.Unlock()
			finishCandidate(seam)
			if _, err := other.Execute(ctx, "candidate-verify", 1200, 0); err == nil {
				t.Fatal("unbound outbox became full proof")
			}
			out, err := other.Execute(ctx, "candidate-dispatch-reconcile", 1200, 0)
			if err != nil || out.Intent.Status != "bound" || h.count() != before {
				t.Fatal(out, err, h.count())
			}
			if _, err = other.Execute(ctx, "candidate-verify", 1200, 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCandidateConcurrentPumpsAndInvalidationSerialize(t *testing.T) {
	for _, scenario := range []string{"two-pumps", "invalidation"} {
		t.Run(scenario, func(t *testing.T) {
			c, _, _, h := candidateDispatchFixture(t)
			ctx := context.Background()
			prepareCandidateDispatch(t, c)
			if scenario == "invalidation" {
				var once sync.Once
				h.onRef = func() {
					once.Do(func() {
						if _, err := c.execute(ctx, "candidate-invalidate", 0, 0, "withdraw definitely unsent candidate"); err != nil {
							t.Error(err)
						}
					})
				}
				if _, err := c.Execute(ctx, "candidate-pump", 1200, 0); !errors.Is(err, ErrConflict) {
					t.Fatal(err)
				}
				row, _ := candidateAt(ctx, c.store.db, 1200)
				i, _ := candidateIntent(ctx, c.store.db, row)
				if row.Status != "invalidated" || i.Status != "retired" || h.count() != 0 {
					t.Fatal(row.Status, i, h.count())
				}
				return
			}
			// Both coordinators pass no-run discovery before either may claim/send.
			var lock sync.Mutex
			gets := 0
			gate := make(chan struct{})
			h.onRef = func() {
				lock.Lock()
				gets++
				n := gets
				if n == 2 {
					close(gate)
				}
				lock.Unlock()
				if n <= 2 {
					<-gate
				}
			}
			other := candidateTwin(t, c)
			errs := make(chan error, 2)
			for _, co := range []*Coordinator{c, other} {
				go func(v *Coordinator) { _, e := v.Execute(ctx, "candidate-pump", 1200, 0); errs <- e }(co)
			}
			for range 2 {
				if err := <-errs; err != nil && !errors.Is(err, ErrAmbiguous) {
					t.Fatal(err)
				}
			}
			if h.count() != 1 {
				t.Fatal("concurrent coordinators duplicated POST", h.count())
			}
			if _, err := c.Execute(ctx, "candidate-dispatch-reconcile", 1200, 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCandidateDispatchRejectsInvalidAuthorityBeforePOST(t *testing.T) {
	for _, scenario := range []string{"expired-candidate", "expired-root", "missing-approval", "head-moved", "base-moved", "result-moved", "wrong-key", "workflow-disabled", "frozen-ref-moved", "expiry-at-claim", "approval-during-preflight", "head-during-preflight"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, seam, h := candidateDispatchFixture(t)
			ctx := context.Background()
			prepareCandidateDispatch(t, c)
			switch scenario {
			case "expired-candidate":
				seam.now = c.adapter.root.Candidate.ExpiresAt
			case "expired-root":
				c.adapter.root.ExpiresAt = seam.now
			case "missing-approval":
				f.candidateReviews = f.candidateReviews[:1]
			case "head-moved":
				f.candidate.Head.SHA = sha(777)
			case "base-moved":
				f.refs["heads/main"] = sha(777)
			case "result-moved":
				f.candidate.MergeSHA = sha(777)
			case "wrong-key":
				c.adapter.keyDigest = Digest([]byte("other key"))
			case "workflow-disabled":
				f.tamper = func(p string, v any) {
					if strings.Contains(p, "/actions/workflows/") {
						if m, ok := v.(map[string]any); ok {
							m["state"] = "disabled_manually"
						}
					}
				}
			case "frozen-ref-moved":
				f.refs[dispatchRef(c.adapter.root.Candidate.Sweep)] = sha(777)
			case "expiry-at-claim":
				c.store.clock = func() time.Time { return c.adapter.root.Candidate.ExpiresAt }
			case "approval-during-preflight":
				var once sync.Once
				h.onRef = func() { once.Do(func() { f.candidateReviews = f.candidateReviews[:1] }) }
			case "head-during-preflight":
				var once sync.Once
				h.onRef = func() { once.Do(func() { f.candidate.Head.SHA = sha(777) }) }
			}
			if _, err := c.Execute(ctx, "candidate-pump", 1200, 0); err == nil {
				t.Fatal("invalid candidate sent", scenario)
			}
			record, _ := candidateAt(ctx, c.store.db, 1200)
			i, _ := candidateIntent(ctx, c.store.db, record)
			if h.count() != 0 || i.Status != "prepared" || record.Status != "frozen" {
				t.Fatal("rejected dispatch changed proof state", h.count(), i, record.Status)
			}
		})
	}
}

func TestCandidateReconcileIsHistoricalAndRefusesWrongOriginalRun(t *testing.T) {
	for _, scenario := range []string{"historical", "rerun", "duplicate", "wrong-target", "wrong-actor", "wrong-workflow", "wrong-event", "wrong-ref", "after-expiry", "expired-root"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, seam, h := candidateDispatchFixture(t)
			ctx := context.Background()
			prepareCandidateDispatch(t, c)
			h.visible = false
			if _, err := c.Execute(ctx, "candidate-pump", 1200, 0); !errors.Is(err, ErrAmbiguous) {
				t.Fatal(err)
			}
			h.visible = true
			finishCandidate(seam)
			switch scenario {
			case "historical":
				seam.now = c.adapter.root.Candidate.ExpiresAt
				f.candidate.Head.SHA = sha(777)
				f.candidateReviews = nil
				f.refs[dispatchRef(c.adapter.root.Candidate.Sweep)] = sha(777)
			case "rerun":
				seam.run.RunAttempt = 2
			case "duplicate":
				h.duplicate = true
			case "wrong-target":
				seam.run.HeadSHA = sha(777)
			case "wrong-actor":
				seam.run.Actor.ID++
			case "wrong-workflow":
				seam.run.WorkflowID++
			case "wrong-event":
				seam.run.Event = "pull_request"
			case "wrong-ref":
				seam.run.HeadBranch = "untrusted-ref"
			case "after-expiry":
				seam.now = c.adapter.root.Candidate.ExpiresAt.Add(time.Minute)
				seam.run.CreatedAt = c.adapter.root.Candidate.ExpiresAt
				seam.run.UpdatedAt = seam.now
			case "expired-root":
				c.adapter.root.ExpiresAt = seam.now
			}
			other := candidateTwin(t, c)
			out, err := other.Execute(ctx, "candidate-dispatch-reconcile", 1200, 0)
			if scenario == "historical" {
				if err != nil || out.Intent.Status != "bound" || out.Candidate.Record.Evidence != nil || out.Candidate.PromotionAllowed {
					t.Fatal("historical observation failed/granted validation", out, err)
				}
				if _, err = other.Execute(ctx, "candidate-verify", 1200, 0); err == nil {
					t.Fatal("expired/moved/unapproved history became validation")
				}
			} else {
				if err == nil {
					t.Fatal("wrong original run bound", scenario)
				}
				row, _ := candidateAt(ctx, c.store.db, 1200)
				i, _ := candidateIntent(ctx, c.store.db, row)
				if i.Status != "uncertain" {
					t.Fatal("invalid run replaced uncertainty", i)
				}
			}
			if h.count() != 1 {
				t.Fatal("reconcile sent POST", h.count())
			}
		})
	}
}

func TestCandidateBoundProofAndRetiredIDCannotBeReplaced(t *testing.T) {
	c, _, seam, h := candidateDispatchFixture(t)
	ctx := context.Background()
	prepareCandidateDispatch(t, c)
	if _, err := c.Execute(ctx, "candidate-pump", 1200, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.execute(ctx, "candidate-invalidate", 0, 0, "running proof is not retired"); err == nil {
		t.Fatal("running bound proof invalidated")
	}
	finishCandidate(seam)
	if _, err := c.Execute(ctx, "candidate-verify", 1200, 0); err != nil {
		t.Fatal(err)
	}
	original := seam.run.ID
	seam.run.ID++
	if _, err := c.Execute(ctx, "candidate-dispatch-reconcile", 1200, 0); err == nil {
		t.Fatal("original bound run replaced")
	}
	seam.run.ID = original
	if _, err := c.execute(ctx, "candidate-invalidate", 0, 0, "completed candidate superseded; retain evidence"); err != nil {
		t.Fatal(err)
	}
	c.adapter.root.Candidate.ExpiresAt = c.adapter.root.Candidate.ExpiresAt.Add(time.Minute)
	if _, err := c.Execute(ctx, "candidate-freeze", 1200, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "candidate-prepare", 1200, 0); err == nil {
		t.Fatal("retained intent ID reused")
	}
	if h.count() != 1 {
		t.Fatal("invalidation/prepare dispatched replacement")
	}
}

func TestCandidateDispatchCLIRequiresExternalAuthority(t *testing.T) {
	for _, op := range []string{"candidate-prepare", "candidate-pump", "candidate-dispatch-reconcile"} {
		dir := filepath.Join(t.TempDir(), "absent-authority")
		var b bytes.Buffer
		status := RunCLI(context.Background(), []string{op, dir, "--pull", "1200"}, &b, "unused")
		if status != 1 || !strings.Contains(b.String(), `"Closed":true`) {
			t.Fatal("CLI route did not reach authority loader", op, status, b.String())
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("missing authority created state", err)
		}
	}
}

func TestCandidateAdoptsExistingOriginalWithoutAnotherPOST(t *testing.T) {
	c, f, seam, h := candidateDispatchFixture(t)
	seam.now = clock.Add(3 * time.Minute)
	h.posts = 1
	f.refs[dispatchRef(c.adapter.root.Candidate.Sweep)] = c.adapter.root.Candidate.Result
	prepareCandidateDispatch(t, c)
	out, err := c.Execute(context.Background(), "candidate-pump", 1200, 0)
	if err != nil || out.Intent.Status != "bound" || h.count() != 1 {
		t.Fatal("existing exact run resent", out, err, h.count())
	}
}

func TestCandidateReconcileRechecksCurrentAuthorityAtCommit(t *testing.T) {
	for _, bound := range []string{"root", "policy"} {
		t.Run(bound, func(t *testing.T) {
			c, _, _, h := candidateDispatchFixture(t)
			prepareCandidateDispatch(t, c)
			h.visible = false
			if _, err := c.Execute(context.Background(), "candidate-pump", 1200, 0); !errors.Is(err, ErrAmbiguous) {
				t.Fatal(err)
			}
			h.visible = true
			deadline := c.adapter.root.ExpiresAt
			if bound == "policy" {
				deadline = validPolicy().ExpiresAt
				c.adapter.root.ExpiresAt = deadline.Add(time.Hour)
			}
			c.store.clock = func() time.Time { return deadline }
			if _, err := c.Execute(context.Background(), "candidate-dispatch-reconcile", 1200, 0); err == nil {
				t.Fatal("expired authority bound candidate receipt", bound)
			}
			row, _ := candidateAt(context.Background(), c.store.db, 1200)
			i, _ := candidateIntent(context.Background(), c.store.db, row)
			if i.Status != "uncertain" || h.count() != 1 {
				t.Fatal("expired CAS changed/sent intent", i, h.count())
			}
		})
	}
}

// Exercise the approved-source closure size, rather than a one-file fixture
// budget. The HTTP payloads remain explicit variants, not live dispatch proof.
func TestCandidateDispatchExecutionClosureBudget(t *testing.T) {
	c, f, _, h := candidateDispatchFixture(t)
	root := &c.adapter.root
	requirement := root.Controls[root.CurrentControl]
	for n := 0; n < 27; n++ {
		requirement.ExecutionFiles[fmt.Sprintf(".github/ci/approved-%d.py", n)] = Digest(f.workflow)
	}
	root.Controls[root.CurrentControl] = requirement
	root.Candidate.Sweep.Authority = c.adapter.authorityDigest()
	var requests atomic.Int64
	previous := c.adapter.client.Transport
	if previous == nil {
		previous = http.DefaultTransport
	}
	c.adapter.client.Transport = candidateCountingTransport{previous, &requests}
	prepareCandidateDispatch(t, c)
	requests.Store(0)
	out, err := c.Execute(context.Background(), "candidate-pump", 1200, 0)
	if err != nil || out.Intent == nil || out.Intent.Status != "bound" || h.count() != 1 {
		t.Fatal(out, err, h.count(), requests.Load())
	}
	t.Logf("candidate pump: %d HTTP requests for %d approved execution files (budget %d)", requests.Load(), len(requirement.ExecutionFiles), root.MaxRequests)
}

type candidateCountingTransport struct {
	next     http.RoundTripper
	requests *atomic.Int64
}

func (c candidateCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return c.next.RoundTrip(r)
}
