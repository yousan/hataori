package cipolicy

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Baseline struct {
	expires, at      time.Time
	control          verifiedControl
	repositoryID     int64
	ref, key, target string
	proofSeal        string
}

// Bootstrap verifies the signed fixed baseline against real Actions data. Store
// initialization is one-shot; another target cannot reset a populated lane.
func (a *Adapter) Bootstrap(ctx context.Context) (Baseline, error) {
	return a.bootstrapRun(ctx, a.root.Bootstrap.RunID)
}

func (a *Adapter) bootstrapRun(ctx context.Context, run int64) (Baseline, error) {
	ctx = a.budget(ctx)
	c, err := a.bootstrapControl(ctx)
	if err != nil {
		return Baseline{}, err
	}
	b := a.root.Bootstrap
	if !revisionOK(b.Target) || b.Sweep.Target != b.Target || !b.Sweep.CompatibilityRequired || b.Sweep.ID == "" || !revisionOK(b.Sweep.From) {
		return Baseline{}, fmt.Errorf("invalid fixed bootstrap proof")
	}
	completion, err := a.completion(ctx, c, b.Sweep, run, b.Attempt)
	if err != nil {
		return Baseline{}, err
	}
	if completion == nil || completion.Linux != "success" || completion.MacOS != "success" || completion.Compatibility != "success" {
		return Baseline{}, fmt.Errorf("baseline full proof incomplete")
	}
	var ref struct{ Object struct{ SHA string } }
	if err := a.request(ctx, "GET", a.prefix()+"/git/ref/heads/"+a.root.IntegrationRef, nil, &ref); err != nil {
		return Baseline{}, err
	}
	if _, err := a.history(ctx, b.Target, ref.Object.SHA); err != nil {
		return Baseline{}, err
	}
	return Baseline{control: c, repositoryID: a.root.RepositoryID, ref: a.root.IntegrationRef, key: a.keyDigest, target: b.Target, expires: a.root.ExpiresAt, at: a.clock()}, nil
}

// Diagnose authenticates the responsible account; a supplied owner string alone
// cannot clear failure. This grants no provider waiver or merge permission.
func (a *Adapter) Diagnose(ctx context.Context, o Observation, reason string) (Observation, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return Observation{}, fmt.Errorf("diagnosis required")
	}
	var who Identity
	if err := a.request(ctx, "GET", "/user", nil, &who); err != nil {
		return Observation{}, err
	}
	if who != o.control.requirement.Owner || o.key != a.keyDigest || o.repositoryID != a.root.RepositoryID {
		return Observation{}, fmt.Errorf("unauthenticated recovery owner")
	}
	o.event.Recovery = &Recovery{Owner: who.Login, Reason: reason}
	return o, nil
}
