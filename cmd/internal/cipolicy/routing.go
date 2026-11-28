package cipolicy

import (
	"context"
	"fmt"
	"time"
)

// RouteGrant is an independently signed, exact-revision opt-in requirement.
// The referenced actual owner comment and feedback are verified too. A label
// or caller-supplied owner cannot retarget another worker's PR.
type RouteGrant struct {
	Owner           Identity
	Head, Base, Ref string
	ExpiresAt       time.Time
	ConsentID       int64
	Reviews         []Identity
	FeedbackRun     int64
	Feedback        Sweep
}
type RouteResult struct {
	Pull                                                             int
	Route, Reason, Boundary                                          string
	Allowed, Integrated, DoneAllowed, ReleaseAllowed, UpgradeAllowed bool
	Plan                                                             *Plan `json:",omitempty"`
}

func consentText(repo int64, pull int, g RouteGrant) string {
	return fmt.Sprintf("CI integration opt-in\nrepository=%d\npull=%d\nhead=%s\nbase=%s\nref=%s\nexpires=%s", repo, pull, g.Head, g.Base, g.Ref, g.ExpiresAt.UTC().Format(time.RFC3339))
}
func (a *Adapter) route(ctx context.Context, snap Snapshot, pull int, done bool, d *Decision) (RouteResult, error) {
	r := RouteResult{Pull: pull, Route: "main", Boundary: "existing main final-head and actual main gates"}
	grant, opted := a.root.Routes[pull]
	if !a.root.RoutingEnabled || !opted {
		r.Reason = "owner has not adopted integration; preserve existing main route"
		return r, nil
	}
	r.Route = "integration"
	r.Boundary = "integration only; no release or upgrade authority"
	if pull <= 0 || (!done && !a.clock().Before(grant.ExpiresAt)) || grant.ExpiresAt.IsZero() || grant.Ref != a.root.IntegrationRef || !revisionOK(grant.Head) || !revisionOK(grant.Base) || grant.Owner.ID <= 0 || grant.Owner.Login == "" || grant.ConsentID <= 0 {
		return r, fmt.Errorf("invalid or expired owner opt-in")
	}
	ctx = a.budget(ctx)
	var p pullRecord
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", a.prefix(), pull), nil, &p); err != nil {
		return r, err
	}
	if p.Number != pull || p.Draft || p.Head.SHA != grant.Head || p.Head.Repo.ID != a.root.RepositoryID || p.Base.Repo.ID != a.root.RepositoryID || p.Base.Ref != grant.Ref || p.Base.SHA != grant.Base {
		return r, fmt.Errorf("owner opt-in does not match current PR")
	}
	var consent struct {
		ID        int64
		User      Identity
		Body      string
		IssueURL  string    `json:"issue_url"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/issues/comments/%d", a.prefix(), grant.ConsentID), nil, &consent); err != nil {
		return r, err
	}
	if done && !p.Merged {
		r.Reason = "not actually integrated"
		return r, nil
	}
	if done && (p.MergedAt.IsZero() || !p.MergedAt.Before(grant.ExpiresAt) || !consent.CreatedAt.Before(p.MergedAt) || !consent.UpdatedAt.Before(p.MergedAt)) {
		return r, fmt.Errorf("owner adoption must predate integration")
	}
	if consent.ID != grant.ConsentID || consent.User != grant.Owner || consent.Body != consentText(a.root.RepositoryID, pull, grant) || consent.IssueURL != "https://api.github.com"+fmt.Sprintf("%s/issues/%d", a.prefix(), pull) || consent.CreatedAt.IsZero() || consent.UpdatedAt.IsZero() || consent.UpdatedAt.Before(consent.CreatedAt) || consent.UpdatedAt.After(a.clock()) || consent.CreatedAt.After(a.clock()) || !consent.CreatedAt.Before(grant.ExpiresAt) {
		return r, fmt.Errorf("actual exact-revision owner consent missing")
	}
	reviews, err := a.reviews(ctx, pull)
	if err != nil {
		return r, err
	}
	deadline := a.clock()
	if done {
		deadline = p.MergedAt
	}
	if err = reviewRequirements(reviews, append(append([]Identity{}, grant.Reviews...), a.root.RequiredPullReviews[pull]...), grant.Head, deadline, p.User.ID); err != nil {
		return r, err
	}
	if done {
		if d == nil || d.Policy.FullOnly || !a.clock().Before(a.root.ExpiresAt) {
			r.Reason = "expired or closed policy cannot authorize Done"
			return r, nil
		}
		if !p.Merged || p.MergedAt.IsZero() {
			r.Reason = "not actually integrated"
			return r, nil
		}
		merge, watermark := -2, -1
		if snap.State.Watermark == snap.History.Root {
			watermark = -1
		}
		for i, c := range snap.History.Commits {
			if c.SHA == p.MergeSHA {
				merge = i
			}
			if c.SHA == snap.State.Watermark {
				watermark = i
			}
		}
		if merge < 0 || merge > watermark || snap.State.Failed {
			r.Reason = "actual integration lacks complete applicable full coverage"
			return r, nil
		}
		r.Integrated = true
		r.DoneAllowed = true
		r.Allowed = true
		r.Reason = "integrated with complete coverage; release/main checks remain separate"
		return r, nil
	}
	if p.State != "open" || p.Merged || d == nil || !d.AllowCode || d.Policy.FullOnly {
		r.Reason = "fresh integration admission closed"
		return r, nil
	}
	var tip struct{ Object struct{ SHA string } }
	if err = a.request(ctx, "GET", a.prefix()+"/git/ref/heads/"+a.root.IntegrationRef, nil, &tip); err != nil {
		return r, err
	}
	if tip.Object.SHA != grant.Base || snap.State.ObservedTip != grant.Base {
		return r, fmt.Errorf("integration base moved; obtain a fresh exact-base opt-in")
	}
	changes, err := a.diff(ctx, grant.Base, grant.Head)
	if err != nil {
		return r, err
	}
	plan := Classify(changes, true)
	r.Plan = &plan
	if plan.Full || plan.HighRisk {
		r.Reason = "high-risk changes retain existing full ready-head route"
		r.Route = "main"
		return r, nil
	}
	control, err := a.control(ctx)
	if err != nil {
		return r, err
	}
	if grant.Feedback.Target != grant.Head || grant.Feedback.From != grant.Base || len(control.requirement.DevelopmentJobs) == 0 {
		return r, fmt.Errorf("targeted feedback contract missing")
	}
	if err = a.feedback(ctx, control, grant.Feedback, grant.FeedbackRun); err != nil {
		return r, err
	}
	r.Allowed = true
	r.Reason = "exact owner opt-in and executed feedback verified; recheck admission immediately before integration"
	return r, nil
}

func (a *Adapter) feedback(ctx context.Context, c verifiedControl, s Sweep, id int64) error {
	r, err := a.run(ctx, c, s, id, 1)
	if err != nil {
		return err
	}
	if r.Status != "completed" || r.Conclusion != "success" {
		return fmt.Errorf("feedback incomplete")
	}
	jobs, err := a.jobs(ctx, s, id, 1)
	if err != nil {
		return err
	}
	for _, name := range c.requirement.DevelopmentJobs {
		j, ok := jobs[name]
		if !ok || j.Status != "completed" || j.Conclusion != "success" || j.RunnerID <= 0 || !jobExecution(j, c.requirement.RequiredSteps[name], c.requirement.RunnerLabels[name]) {
			return fmt.Errorf("targeted feedback did not execute")
		}
	}
	return nil
}
