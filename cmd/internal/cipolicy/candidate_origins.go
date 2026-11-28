package cipolicy

import (
	"context"
	"fmt"
)

// The source checkpoint is distinct from main B. A previous released source
// can be an ancestor of B without B being on the integration first-parent line.
// Every new source commit must have actual integration-merge provenance. Manual
// writes and incomplete ancestry cannot disappear behind an approved release PR.
func (a *Adapter) candidateOrigins(ctx context.Context, r CandidateRequirement) ([]CandidateOrigin, error) {
	if r.IntegrationFrom != r.Base {
		var ancestry struct {
			Base      struct{ SHA string } `json:"base_commit"`
			MergeBase struct{ SHA string } `json:"merge_base_commit"`
			Status    string
		}
		if err := a.request(ctx, "GET", a.prefix()+"/compare/"+r.IntegrationFrom+"..."+r.Base+"?per_page=1", nil, &ancestry); err != nil {
			return nil, err
		}
		if ancestry.Base.SHA != r.IntegrationFrom || ancestry.MergeBase.SHA != r.IntegrationFrom || ancestry.Status != "ahead" {
			return nil, fmt.Errorf("source checkpoint is not covered by main base")
		}
	}
	var origins []CandidateOrigin
	seen := map[string]bool{}
	for target := r.Head; target != r.IntegrationFrom; {
		if seen[target] || len(origins) >= 1000 {
			return nil, fmt.Errorf("source ancestry incomplete")
		}
		seen[target] = true
		var object commitRecord
		if err := a.request(ctx, "GET", a.prefix()+"/git/commits/"+target, nil, &object); err != nil {
			return nil, err
		}
		if object.SHA != target || len(object.Parents) != 2 || !revisionOK(object.Parents[0].SHA) || !revisionOK(object.Parents[1].SHA) {
			return nil, fmt.Errorf("source is not an integration merge")
		}
		var pulls []pullRecord
		if err := a.request(ctx, "GET", a.prefix()+"/commits/"+target+"/pulls?per_page=100", nil, &pulls); err != nil {
			return nil, err
		}
		if len(pulls) >= 100 {
			return nil, fmt.Errorf("source pull enumeration incomplete")
		}
		var found *pullRecord
		for i := range pulls {
			p := &pulls[i]
			if p.MergeSHA != target || p.MergedAt.IsZero() {
				continue
			}
			if found != nil || p.Base.Repo.ID != a.root.RepositoryID || p.Head.Repo.ID != a.root.RepositoryID || p.Base.Ref != a.root.IntegrationRef || p.Head.SHA != object.Parents[1].SHA || p.MergedAt.After(a.clock()) {
				return nil, fmt.Errorf("source integration identity ambiguous")
			}
			found = p
		}
		if found == nil || found.Number <= 0 {
			return nil, fmt.Errorf("manual/unrecognized source write")
		}
		changes, err := a.diff(ctx, object.Parents[0].SHA, target)
		if err != nil {
			return nil, err
		}
		required := append([]Identity{}, a.root.RequiredPullReviews[found.Number]...)
		if grant, ok := a.root.Routes[found.Number]; ok {
			if grant.Head != found.Head.SHA || grant.Base != object.Parents[0].SHA || grant.Ref != a.root.IntegrationRef {
				return nil, fmt.Errorf("source grant does not match integration")
			}
			required = append(required, grant.Reviews...)
		}
		if Classify(changes, true).HighRisk {
			if len(a.root.RiskReviewers) == 0 {
				return nil, fmt.Errorf("high-risk review requirements unestablished")
			}
			required = append(required, a.root.RiskReviewers...)
		}
		var ids []int64
		if len(required) > 0 {
			reviews, err := a.reviews(ctx, found.Number)
			if err != nil {
				return nil, err
			}
			if err = reviewRequirements(reviews, required, found.Head.SHA, found.MergedAt, found.User.ID); err != nil {
				return nil, err
			}
			for _, who := range required {
				var latest int64
				for _, review := range reviews {
					if review.User == who && review.ID > latest {
						latest = review.ID
					}
				}
				ids = append(ids, latest)
			}
		}
		origins = append(origins, CandidateOrigin{Pull: found.Number, Merge: target, Head: found.Head.SHA, Reviews: ids})
		target = object.Parents[0].SHA
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("empty candidate source range")
	}
	return origins, nil
}
