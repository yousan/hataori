package cipolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *Adapter) verifyCandidate(ctx context.Context, record CandidateRecord) (*CandidateEvidence, verifiedControl, error) {
	tuple, c, pr, err := a.candidateTuple(ctx, record.Tuple.Request.Pull, false)
	if err != nil {
		return nil, c, err
	}
	if candidateID(tuple) != record.ID {
		return nil, c, fmt.Errorf("frozen candidate tuple changed")
	}
	r := tuple.Request
	reviews, err := a.candidateReviews(ctx, c, r, pr, a.clock())
	if err != nil {
		return nil, c, err
	}
	id, err := a.discoverRun(ctx, c, r.Sweep)
	if err != nil || id <= 0 {
		return nil, c, fmt.Errorf("unique frozen candidate run unavailable")
	}
	if record.Evidence != nil && record.Evidence.Run != id {
		return nil, c, fmt.Errorf("frozen candidate proof cannot be replaced")
	}
	var current runRecord
	if err = a.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d", a.prefix(), id), nil, &current); err != nil {
		return nil, c, err
	}
	if current.ID != id || current.RunAttempt != 1 || current.HeadBranch != strings.TrimPrefix(dispatchRef(r.Sweep), "heads/") || current.Status != "completed" || current.Conclusion != "success" || current.CreatedAt.IsZero() || current.CreatedAt.Before(c.approvedAt) || current.CreatedAt.Before(c.selection.policy.StartsAt) || current.UpdatedAt.Before(current.CreatedAt) || current.UpdatedAt.After(a.clock()) || !current.UpdatedAt.Before(r.ExpiresAt) {
		return nil, c, fmt.Errorf("candidate run incomplete, rerun or late")
	}
	completion, err := a.completion(ctx, c, r.Sweep, id, 1)
	if err != nil {
		return nil, c, err
	}
	if completion == nil || completion.Linux != "success" || completion.MacOS != "success" || (r.Sweep.CompatibilityRequired && completion.Compatibility != "success") {
		return nil, c, fmt.Errorf("full candidate proof incomplete")
	}
	jobs, err := a.jobs(ctx, r.Sweep, id, 1)
	if err != nil {
		return nil, c, err
	}
	minimum := map[string][]string{"contract": {"Validate frozen dispatch"}, "linux": {"Full Linux suite", "Cross-build macOS", "Upload frozen candidate builds"}, "macos": {"Full macOS suite"}, "web": {"Reproducible web build"}}
	labels := map[string][]string{"contract": {"ubuntu-latest"}, "linux": {"ubuntu-latest"}, "macos": {"macos-latest"}, "web": {"ubuntu-latest"}}
	if r.Sweep.CompatibilityRequired {
		for _, v := range compatibilityVersions {
			name := "compatibility (" + v + ")"
			minimum[name] = []string{"Full applicable tmux compatibility"}
			labels[name] = []string{"ubuntu-24.04"}
		}
	}
	ids, err := executedCandidateJobs(jobs, minimum, labels, r.ExpiresAt)
	if err != nil {
		return nil, c, err
	}
	artifacts, err := a.candidateArtifacts(ctx, current, r)
	if err != nil {
		return nil, c, err
	}
	checks, err := a.candidateChecks(ctx, r.Result, r.Checks, a.clock(), a.trustedJobChecks(jobs, minimum, labels))
	if err != nil {
		return nil, c, err
	}
	// This closes observed races only. These HTTP reads are not a GitHub merge
	// transaction and never mint a reusable permission or promotion capability.
	if err = a.candidateRefs(ctx, r, false, ""); err != nil {
		return nil, c, err
	}
	evidence := &CandidateEvidence{Run: id, Workflow: c.requirement.WorkflowID, Attempt: 1, Jobs: ids, Reviews: reviews, Checks: checks, Artifacts: artifacts, At: a.clock().UTC()}
	if record.Evidence != nil {
		evidence.At = record.Evidence.At
		before, _ := json.Marshal(record.Evidence)
		after, _ := json.Marshal(evidence)
		if string(before) != string(after) {
			return nil, c, fmt.Errorf("frozen evidence identity changed; invalidate explicitly")
		}
	}
	return evidence, c, nil
}

func executedCandidateJobs(jobs map[string]jobRecord, steps, labels map[string][]string, deadline time.Time) ([]int64, error) {
	var ids []int64
	for name, required := range steps {
		j, ok := jobs[name]
		if !ok || j.ID <= 0 || j.Status != "completed" || j.Conclusion != "success" || j.RunnerID <= 0 || j.StartedAt.IsZero() || !j.CompletedAt.After(j.StartedAt) || !j.CompletedAt.Before(deadline) || !jobExecution(j, required, labels[name]) {
			return nil, fmt.Errorf("required full job/step/runner did not execute: %s", name)
		}
		ids = append(ids, j.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (a *Adapter) candidateMainRun(ctx context.Context, workflow int64, path, merge string, actor Identity) (runRecord, error) {
	var found runRecord
	total, count := -1, 0
	seen := map[int64]bool{}
	for page := 1; page <= 10; page++ {
		var batch struct {
			Total int         `json:"total_count"`
			Runs  []runRecord `json:"workflow_runs"`
		}
		p := fmt.Sprintf("%s/actions/workflows/%d/runs?event=push&branch=%s&head_sha=%s&per_page=100&page=%d", a.prefix(), workflow, url.QueryEscape(a.root.ControlRef), merge, page)
		if err := a.request(ctx, "GET", p, nil, &batch); err != nil {
			return found, err
		}
		if total < 0 {
			total = batch.Total
		}
		if total != batch.Total || total < 0 || total > 1000 {
			return found, fmt.Errorf("unstable main run enumeration")
		}
		for _, run := range batch.Runs {
			if run.ID <= 0 || seen[run.ID] {
				return found, fmt.Errorf("ambiguous main run identity")
			}
			seen[run.ID] = true
			count++
			if run.WorkflowID != workflow || run.Path != path || run.HeadSHA != merge || run.HeadBranch != a.root.ControlRef || run.Event != "push" || run.Repository.ID != a.root.RepositoryID || run.Repository.FullName != a.root.Repository || run.HeadRepository.ID != a.root.RepositoryID || run.Actor != actor || run.TriggeringActor != actor || run.RunAttempt != 1 || run.Status != "completed" || run.Conclusion != "success" || run.CreatedAt.IsZero() || run.UpdatedAt.Before(run.CreatedAt) || run.UpdatedAt.After(a.clock()) {
				return found, fmt.Errorf("actual-main run incomplete or unrelated")
			}
			if found.ID != 0 {
				return found, fmt.Errorf("multiple main runs; no silent retry selection")
			}
			found = run
		}
		if len(batch.Runs) < 100 {
			if count != total || found.ID <= 0 {
				return found, fmt.Errorf("main run enumeration incomplete")
			}
			return found, nil
		}
	}
	return found, fmt.Errorf("main run enumeration incomplete")
}

func (a *Adapter) verifyCandidateMain(ctx context.Context, record CandidateRecord) (*CandidateMainEvidence, verifiedControl, error) {
	var c verifiedControl
	if record.Evidence == nil || record.Status != "verified" && record.Status != "main_verified" {
		return nil, c, fmt.Errorf("candidate has no verified full proof")
	}
	tuple, c, pr, err := a.candidateTuple(ctx, record.Tuple.Request.Pull, true)
	if err != nil {
		return nil, c, err
	}
	if candidateID(tuple) != record.ID || !record.Evidence.At.Before(pr.MergedAt) {
		return nil, c, fmt.Errorf("candidate proof must predate merge")
	}
	r := tuple.Request
	if err = a.candidateObject(ctx, pr.MergeSHA, r); err != nil {
		return nil, c, err
	}
	reviews, err := a.candidateReviews(ctx, c, r, pr, pr.MergedAt)
	if err != nil {
		return nil, c, err
	}
	for p, want := range r.MainFiles {
		b, e := a.content(ctx, pr.MergeSHA, p)
		if e != nil || Digest(b) != want {
			return nil, c, fmt.Errorf("actual-main execution source changed")
		}
	}
	ci, err := a.candidateMainRun(ctx, r.MainCI, ".github/workflows/ci.yml", pr.MergeSHA, r.MainActor)
	if err != nil {
		return nil, c, err
	}
	if ci.ID == record.Evidence.Run || ci.CreatedAt.Before(pr.MergedAt) {
		return nil, c, fmt.Errorf("candidate proof cannot substitute for main")
	}
	jobs, err := a.jobs(ctx, Sweep{Target: pr.MergeSHA}, ci.ID, 1)
	if err != nil {
		return nil, c, err
	}
	steps := map[string][]string{"changes": {"Require complete diff base"}, "check": {"compile tests without running them", "core and adapter boundary"}, "test": {"Run go test -timeout 30m ./..."}, "test-macos": {"go test"}, "web": {"the build is reproducible"}, "build-macos": {"build"}}
	labels := map[string][]string{}
	for name := range steps {
		labels[name] = []string{"ubuntu-latest"}
	}
	labels["test-macos"] = []string{"macos-latest"}
	if len(jobs) != len(steps) {
		return nil, c, fmt.Errorf("main CI job manifest changed")
	}
	ids, err := executedCandidateJobs(jobs, steps, labels, a.clock().Add(time.Nanosecond))
	if err != nil {
		return nil, c, err
	}
	trustedChecks := a.trustedJobChecks(jobs, steps, labels)
	runs := []int64{ci.ID}
	if r.Sweep.CompatibilityRequired {
		tmux, err := a.candidateMainRun(ctx, r.MainTmux, ".github/workflows/tmux.yml", pr.MergeSHA, r.MainActor)
		if err != nil {
			return nil, c, err
		}
		if tmux.ID == ci.ID || tmux.ID == record.Evidence.Run || tmux.CreatedAt.Before(pr.MergedAt) {
			return nil, c, fmt.Errorf("main compatibility proof unrelated")
		}
		tmuxJobs, err := a.jobs(ctx, Sweep{Target: pr.MergeSHA}, tmux.ID, 1)
		if err != nil {
			return nil, c, err
		}
		full, labels := map[string][]string{}, map[string][]string{}
		for _, v := range compatibilityVersions {
			name := "tmux (" + v + ")"
			if v != "3.2" && v != "3.2a" && v != "3.4" && v != "3.8" {
				name = "tmux (" + v + ", ^TestPipeCommandPreservesLiteralPaths$)"
			}
			full[name], labels[name] = []string{"Test the holder"}, []string{"ubuntu-24.04"}
		}
		if len(tmuxJobs) != len(full)+1 || tmuxJobs["tmux changes"].Conclusion != "success" {
			return nil, c, fmt.Errorf("main compatibility manifest changed")
		}
		more, err := executedCandidateJobs(tmuxJobs, full, labels, a.clock().Add(time.Nanosecond))
		if err != nil {
			return nil, c, err
		}
		for id, name := range a.trustedJobChecks(tmuxJobs, full, labels) {
			trustedChecks[id] = name
		}
		ids = append(ids, more...)
		runs = append(runs, tmux.ID)
	}
	checks, err := a.candidateChecks(ctx, pr.MergeSHA, r.Checks, a.clock(), trustedChecks)
	if err != nil {
		return nil, c, err
	}
	if err = a.candidateRefs(ctx, r, true, pr.MergeSHA); err != nil {
		return nil, c, err
	}
	return &CandidateMainEvidence{Merge: pr.MergeSHA, Tree: r.Tree, Runs: runs, Jobs: ids, Reviews: reviews, Checks: checks, At: a.clock().UTC()}, c, nil
}

// Actions checks share one app identity across workflows. Bind them to actual
// executed jobs of the already verified workflow, not just app/name/head.
func (a *Adapter) trustedJobChecks(jobs map[string]jobRecord, steps, labels map[string][]string) map[int64]string {
	out := map[int64]string{}
	prefix := "https://api.github.com" + a.prefix() + "/check-runs/"
	for name, required := range steps {
		j := jobs[name]
		if j.Conclusion != "success" || !jobExecution(j, required, labels[name]) || !strings.HasPrefix(j.CheckRunURL, prefix) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(j.CheckRunURL, prefix), 10, 64)
		if err == nil && id > 0 {
			if _, duplicate := out[id]; duplicate {
				return map[int64]string{}
			}
			out[id] = name
		}
	}
	return out
}
