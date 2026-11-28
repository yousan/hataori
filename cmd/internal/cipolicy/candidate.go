package cipolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CandidateRequirement is an independently signed request, not PR metadata or
// a candidate-provided manifest. It authorizes evidence collection only. There
// is deliberately no promotion sender or release/upgrade permission here.
type CandidateRequirement struct {
	Pull                                int
	SourceRef, Base, Head, Result, Tree string
	IntegrationFrom                     string
	ExpiresAt                           time.Time
	Sweep                               Sweep
	Reviews                             []Identity
	Dependencies, MainFiles             map[string]string
	MainCI, MainTmux                    int64
	MainActor                           Identity
	Checks                              []CandidateCheck
}

type CandidateCheck struct {
	Name  string
	AppID int64
}

type CandidateTuple struct {
	Request                     CandidateRequirement
	Repository                  int64
	Key, Control, Policy, Scope string
	Origins                     []CandidateOrigin
}

type CandidateOrigin struct {
	Pull        int
	Merge, Head string
	Reviews     []int64
}

type ArtifactEvidence struct {
	ID, Run, Size int64
	Name, Digest  string
}

type CandidateEvidence struct {
	Run, Workflow         int64
	Attempt               int
	Jobs, Reviews, Checks []int64
	Artifacts             []ArtifactEvidence
	At                    time.Time
}

type CandidateMainEvidence struct {
	Merge, Tree                 string
	Runs, Jobs, Reviews, Checks []int64
	At                          time.Time
}

type CandidateRecord struct {
	ID, Status string
	Revision   int64
	Tuple      CandidateTuple
	Evidence   *CandidateEvidence     `json:",omitempty"`
	Main       *CandidateMainEvidence `json:",omitempty"`
}

type CandidateReport struct {
	Record                                           CandidateRecord
	PromotionAllowed, ReleaseAllowed, UpgradeAllowed bool
	Boundary                                         string
}

var candidateDependencies = []string{"go.mod", "go.sum", "web/package.json", "web/package-lock.json", "web/.nvmrc", ".github/ci/tmux-sha256.txt"}
var candidateMainFiles = []string{".github/workflows/ci.yml", ".github/workflows/tmux.yml", ".github/filters.yml"}

func candidateID(t CandidateTuple) string { b, _ := json.Marshal(t); return Digest(b) }

func candidateShape(r *CandidateRequirement) error {
	if r == nil || r.Pull <= 0 || !safeBranch(r.SourceRef) || r.SourceRef == "main" || !revisionOK(r.Base) || !revisionOK(r.Head) || !revisionOK(r.Result) || !revisionOK(r.Tree) || !revisionOK(r.IntegrationFrom) || r.IntegrationFrom == r.Head || r.Base == r.Head || r.Base == r.Result || r.ExpiresAt.IsZero() || len(r.Reviews) == 0 || r.MainCI <= 0 || r.MainTmux <= 0 || r.MainCI == r.MainTmux || r.MainActor.ID <= 0 || r.MainActor.Login == "" || !strings.HasPrefix(r.Sweep.ID, "candidate-") || len(r.Sweep.ID) <= len("candidate-") || r.Sweep.From != r.Base || r.Sweep.Target != r.Result {
		return fmt.Errorf("incomplete frozen candidate request")
	}
	for _, m := range []map[string]string{r.Dependencies, r.MainFiles} {
		for p, d := range m {
			if !safePath(p) || !digestOK(d) {
				return fmt.Errorf("invalid frozen dependency")
			}
		}
	}
	for _, p := range candidateDependencies {
		if !digestOK(r.Dependencies[p]) {
			return fmt.Errorf("candidate dependency missing")
		}
	}
	for _, p := range candidateMainFiles {
		if !digestOK(r.MainFiles[p]) {
			return fmt.Errorf("main execution source missing")
		}
	}
	seen := map[string]bool{}
	for _, c := range r.Checks {
		key := fmt.Sprintf("%d:%s", c.AppID, c.Name)
		if c.AppID <= 0 || c.Name == "" || seen[key] {
			return fmt.Errorf("ambiguous domain check")
		}
		seen[key] = true
	}
	return nil
}

func (a *Adapter) candidateTuple(ctx context.Context, pull int, merged bool) (CandidateTuple, verifiedControl, pullRecord, error) {
	var tuple CandidateTuple
	var pr pullRecord
	c, err := a.control(ctx)
	if err != nil {
		return tuple, c, pr, err
	}
	r := a.root.Candidate
	if err = candidateShape(r); err != nil {
		return tuple, c, pr, err
	}
	if r.Pull != pull || r.SourceRef == a.root.ControlRef || r.SourceRef == a.root.IntegrationRef || !a.clock().Before(a.root.ExpiresAt) || c.selection.Report(a.clock()).FullOnly || r.ExpiresAt.After(c.selection.policy.ExpiresAt) || r.ExpiresAt.After(a.root.ExpiresAt) || (!merged && !a.clock().Before(r.ExpiresAt)) {
		return tuple, c, pr, fmt.Errorf("candidate authority expired or mismatched")
	}
	if r.Sweep.Control != c.revision || r.Sweep.Digest != c.selection.digest || r.Sweep.Authority != c.authority || c.requirement.WorkflowPath != ProofWorkflow {
		return tuple, c, pr, fmt.Errorf("candidate execution/control scope changed")
	}
	if err = a.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", a.prefix(), pull), nil, &pr); err != nil {
		return tuple, c, pr, err
	}
	if pr.Number != pull || pr.Draft || pr.Head.Repo.ID != a.root.RepositoryID || pr.Base.Repo.ID != a.root.RepositoryID || pr.Base.Ref != a.root.ControlRef || pr.Head.Ref != r.SourceRef || pr.Head.SHA != r.Head || (!merged && (pr.State != "open" || pr.Merged || pr.Base.SHA != r.Base || pr.MergeSHA != r.Result)) || (merged && (!pr.Merged || pr.State != "closed" || pr.MergedAt.IsZero() || !pr.MergedAt.Before(r.ExpiresAt))) {
		return tuple, c, pr, fmt.Errorf("frozen candidate PR changed")
	}
	if err = a.candidateRefs(ctx, *r, merged, pr.MergeSHA); err != nil {
		return tuple, c, pr, err
	}
	if err = a.candidateObject(ctx, r.Result, *r); err != nil {
		return tuple, c, pr, err
	}
	changes, err := a.diff(ctx, r.Base, r.Result)
	if err != nil {
		return tuple, c, pr, err
	}
	plan := Classify(changes, true)
	if plan.Compatibility != r.Sweep.CompatibilityRequired {
		return tuple, c, pr, fmt.Errorf("complete-diff compatibility scope changed")
	}
	policy, err := a.content(ctx, r.Result, PolicyPath)
	if err != nil || Digest(policy) != c.selection.digest {
		return tuple, c, pr, fmt.Errorf("candidate policy differs from approved control")
	}
	tuple.Origins, err = a.candidateOrigins(ctx, *r)
	if err != nil {
		return tuple, c, pr, err
	}
	for _, m := range []map[string]string{r.Dependencies, r.MainFiles} {
		for p, digest := range m {
			for _, rev := range []string{c.revision, r.Result} {
				b, e := a.content(ctx, rev, p)
				if e != nil || Digest(b) != digest {
					return tuple, c, pr, fmt.Errorf("frozen dependency/main execution changed")
				}
			}
		}
	}
	// Copy through canonical JSON: returned tuples cannot alias the root maps.
	b, _ := json.Marshal(*r)
	json.Unmarshal(b, &tuple.Request)
	tuple.Repository, tuple.Key, tuple.Control, tuple.Policy, tuple.Scope = a.root.RepositoryID, a.keyDigest, c.revision, c.selection.digest, c.authority
	return tuple, c, pr, nil
}

func (a *Adapter) candidateRefs(ctx context.Context, r CandidateRequirement, merged bool, merge string) error {
	refs := map[string]string{a.root.ControlRef: r.Base, r.SourceRef: r.Head}
	if merged {
		refs = map[string]string{a.root.ControlRef: merge}
	}
	for name, want := range refs {
		var got struct{ Object struct{ SHA string } }
		if err := a.request(ctx, "GET", a.prefix()+"/git/ref/heads/"+name, nil, &got); err != nil {
			return err
		}
		if !revisionOK(want) || got.Object.SHA != want {
			return fmt.Errorf("candidate base/head moved")
		}
	}
	return nil
}

func (a *Adapter) candidateObject(ctx context.Context, revision string, r CandidateRequirement) error {
	var object commitRecord
	if !revisionOK(revision) {
		return fmt.Errorf("immutable merge object missing")
	}
	if err := a.request(ctx, "GET", a.prefix()+"/git/commits/"+revision, nil, &object); err != nil {
		return err
	}
	if object.SHA != revision || object.Tree.SHA != r.Tree || len(object.Parents) != 2 || object.Parents[0].SHA != r.Base || object.Parents[1].SHA != r.Head {
		return fmt.Errorf("candidate parents/tree changed")
	}
	return nil
}

func (a *Adapter) candidateReviews(ctx context.Context, c verifiedControl, r CandidateRequirement, pr pullRecord, at time.Time) ([]int64, error) {
	reviews, err := a.reviews(ctx, r.Pull)
	if err != nil {
		return nil, err
	}
	required := append(append([]Identity{}, r.Reviews...), a.root.RequiredPullReviews[r.Pull]...)
	// Release validation conservatively retains domain reviewers even when a
	// candidate is classified ordinary. Candidate labels cannot omit them.
	required = append(required, a.root.RiskReviewers...)
	if err = reviewRequirements(reviews, required, r.Head, at, pr.User.ID); err != nil {
		return nil, err
	}
	var ids []int64
	for _, who := range required {
		var latest int64
		for _, review := range reviews {
			if review.User == who && review.ID > latest {
				latest = review.ID
			}
		}
		ids = append(ids, latest)
	}
	return ids, nil
}

type candidateArtifact struct {
	ID           int64
	Name, Digest string
	Size         int64 `json:"size_in_bytes"`
	Expired      bool
	CreatedAt    time.Time `json:"created_at"`
	WorkflowRun  struct {
		ID               int64
		RepositoryID     int64  `json:"repository_id"`
		HeadRepositoryID int64  `json:"head_repository_id"`
		Head             string `json:"head_sha"`
	} `json:"workflow_run"`
}

func (a *Adapter) candidateArtifacts(ctx context.Context, run runRecord, r CandidateRequirement) ([]ArtifactEvidence, error) {
	var all []candidateArtifact
	total := -1
	for page := 1; page <= 100; page++ {
		var batch struct {
			Total     int `json:"total_count"`
			Artifacts []candidateArtifact
		}
		if err := a.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d/artifacts?per_page=100&page=%d", a.prefix(), run.ID, page), nil, &batch); err != nil {
			return nil, err
		}
		if total < 0 {
			total = batch.Total
		}
		if total != batch.Total || total < 0 {
			return nil, fmt.Errorf("unstable artifact enumeration")
		}
		all = append(all, batch.Artifacts...)
		if len(batch.Artifacts) < 100 {
			break
		}
		if page == 100 {
			return nil, fmt.Errorf("artifact enumeration incomplete")
		}
	}
	if len(all) != total {
		return nil, fmt.Errorf("artifact enumeration incomplete")
	}
	var found []ArtifactEvidence
	seen := map[int64]bool{}
	for _, artifact := range all {
		if artifact.ID <= 0 || seen[artifact.ID] {
			return nil, fmt.Errorf("ambiguous artifact identity")
		}
		seen[artifact.ID] = true
		if artifact.Name != "candidate-macos-"+r.Result {
			continue
		}
		if artifact.Expired || artifact.Size <= 0 || !strings.HasPrefix(artifact.Digest, "sha256:") || !digestOK(strings.TrimPrefix(artifact.Digest, "sha256:")) || artifact.WorkflowRun.ID != run.ID || artifact.WorkflowRun.RepositoryID != a.root.RepositoryID || artifact.WorkflowRun.HeadRepositoryID != a.root.RepositoryID || artifact.WorkflowRun.Head != r.Result || artifact.CreatedAt.Before(run.CreatedAt) || !artifact.CreatedAt.Before(r.ExpiresAt) {
			return nil, fmt.Errorf("artifact run/digest/expiry mismatch")
		}
		found = append(found, ArtifactEvidence{artifact.ID, run.ID, artifact.Size, artifact.Name, artifact.Digest})
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("candidate artifact missing or ambiguous")
	}
	return found, nil
}

func (a *Adapter) candidateChecks(ctx context.Context, head string, required []CandidateCheck, deadline time.Time, trusted map[int64]string) ([]int64, error) {
	if len(required) == 0 {
		return nil, nil
	}
	type check struct {
		ID                       int64
		Name, Status, Conclusion string
		Head                     string `json:"head_sha"`
		App                      struct {
			ID   int64
			Slug string
		}
		CompletedAt time.Time `json:"completed_at"`
	}
	var all []check
	total := -1
	for page := 1; page <= 100; page++ {
		var batch struct {
			Total  int     `json:"total_count"`
			Checks []check `json:"check_runs"`
		}
		if err := a.request(ctx, "GET", fmt.Sprintf("%s/commits/%s/check-runs?filter=all&per_page=100&page=%d", a.prefix(), head, page), nil, &batch); err != nil {
			return nil, err
		}
		if total < 0 {
			total = batch.Total
		}
		if total != batch.Total || total < 0 {
			return nil, fmt.Errorf("unstable check enumeration")
		}
		all = append(all, batch.Checks...)
		if len(batch.Checks) < 100 {
			break
		}
		if page == 100 {
			return nil, fmt.Errorf("check enumeration incomplete")
		}
	}
	if len(all) != total {
		return nil, fmt.Errorf("check enumeration incomplete")
	}
	var ids []int64
	seen := map[int64]bool{}
	for _, item := range all {
		if item.ID <= 0 || seen[item.ID] {
			return nil, fmt.Errorf("ambiguous check ID")
		}
		seen[item.ID] = true
	}
	for _, want := range required {
		var latest check
		for _, item := range all {
			if item.Name == want.Name && item.App.ID == want.AppID && item.ID > latest.ID {
				latest = item
			}
		}
		if latest.ID <= 0 || latest.Head != head || latest.App.Slug == "" || latest.App.Slug == "github-actions" && trusted[latest.ID] != want.Name || latest.Status != "completed" || latest.Conclusion != "success" || latest.CompletedAt.IsZero() || latest.CompletedAt.After(deadline) {
			return nil, fmt.Errorf("required exact-head domain check missing")
		}
		ids = append(ids, latest.ID)
	}
	return ids, nil
}

func candidateAt(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, pull int) (CandidateRecord, error) {
	var record CandidateRecord
	var tuple, evidence, main []byte
	err := db.QueryRowContext(ctx, "SELECT id,revision,status,tuple,evidence,main_evidence FROM candidate WHERE pull=? ORDER BY rowid DESC LIMIT 1", pull).Scan(&record.ID, &record.Revision, &record.Status, &tuple, &evidence, &main)
	if err != nil {
		return record, err
	}
	if err = json.Unmarshal(tuple, &record.Tuple); err != nil {
		return record, err
	}
	if len(evidence) > 0 {
		if err = json.Unmarshal(evidence, &record.Evidence); err != nil {
			return record, err
		}
	}
	if len(main) > 0 {
		if err = json.Unmarshal(main, &record.Main); err != nil {
			return record, err
		}
	}
	if record.ID != candidateID(record.Tuple) {
		return record, fmt.Errorf("candidate tuple seal mismatch")
	}
	return record, nil
}

func (c *Coordinator) candidateOperation(ctx context.Context, operation string, pull int, reason string) (Outcome, error) {
	a, s := c.adapter, c.store
	ctx = a.budget(ctx)
	out := Outcome{Operation: operation}
	if operation == "candidate-invalidate" && a.root.Candidate != nil {
		pull = a.root.Candidate.Pull
	}
	if operation == "candidate-freeze" {
		tuple, control, _, err := a.candidateTuple(ctx, pull, false)
		if err != nil {
			return out, err
		}
		b, _ := json.Marshal(tuple)
		err = s.transaction(ctx, func(conn *sql.Conn) error {
			if !s.clock().Before(a.root.ExpiresAt) || control.selection.Report(s.clock()).FullOnly || !s.clock().Before(tuple.Request.ExpiresAt) {
				return fmt.Errorf("candidate expired before freeze")
			}
			var previous []byte
			e := conn.QueryRowContext(ctx, "SELECT tuple FROM candidate ORDER BY rowid LIMIT 1").Scan(&previous)
			if e == nil {
				var first CandidateTuple
				if json.Unmarshal(previous, &first) != nil || first.Key != tuple.Key || first.Repository != tuple.Repository {
					return fmt.Errorf("candidate store repository/key changed")
				}
			} else if e != sql.ErrNoRows {
				return e
			}
			old, err := candidateAt(ctx, conn, pull)
			if err == nil {
				if old.ID == candidateID(tuple) && old.Status != "invalidated" {
					return nil
				}
				if old.Status != "invalidated" || old.ID == candidateID(tuple) {
					return fmt.Errorf("candidate already frozen; invalidate explicitly")
				}
				err = sql.ErrNoRows
			}
			if err != sql.ErrNoRows {
				return err
			}
			_, err = conn.ExecContext(ctx, "INSERT INTO candidate(id,pull,revision,status,tuple) VALUES(?,?,1,'frozen',?)", candidateID(tuple), pull, b)
			return err
		})
		if err != nil {
			return out, err
		}
	} else {
		record, err := candidateAt(ctx, s.db, pull)
		if err != nil {
			return out, err
		}
		if record.Tuple.Key != a.keyDigest || record.Tuple.Repository != a.root.RepositoryID {
			return out, fmt.Errorf("candidate authority changed")
		}
		switch operation {
		case "candidate-status": // Historical evidence only, never a gate permit.
			_, err = a.control(ctx)
		case "candidate-invalidate":
			control, e := a.control(ctx)
			if e != nil {
				return out, e
			}
			var who Identity
			if strings.TrimSpace(reason) == "" || len(reason) > 4096 {
				return out, fmt.Errorf("invalidation diagnosis required")
			}
			if err = a.request(ctx, "GET", "/user", nil, &who); err != nil || who != control.requirement.Owner {
				return out, fmt.Errorf("responsible owner required")
			}
			// Retain tuple/evidence and an auditable reason; no remote operation.
			err = s.transaction(ctx, func(conn *sql.Conn) error {
				if !s.clock().Before(a.root.ExpiresAt) || control.selection.Report(s.clock()).FullOnly {
					return fmt.Errorf("candidate authority expired before invalidation")
				}
				row, e := conn.ExecContext(ctx, "UPDATE candidate SET revision=revision+1,status='invalidated' WHERE id=? AND revision=? AND status IN ('frozen','verified')", record.ID, record.Revision)
				if e != nil {
					return e
				}
				n, e := row.RowsAffected()
				if e != nil || n != 1 {
					return ErrConflict
				}
				_, e = conn.ExecContext(ctx, "INSERT INTO candidate_invalidation VALUES(?,?,?,?)", record.ID, who.Login, reason, s.clock().UTC().Format(time.RFC3339Nano))
				return e
			})
		case "candidate-verify", "candidate-main":
			if record.Status == "invalidated" {
				return out, fmt.Errorf("candidate invalidated")
			}
			var evidence any
			var control verifiedControl
			column, status := "evidence", "verified"
			if operation == "candidate-verify" {
				evidence, control, err = a.verifyCandidate(ctx, record)
			} else {
				evidence, control, err = a.verifyCandidateMain(ctx, record)
				column, status = "main_evidence", "main_verified"
			}
			if err != nil {
				return out, err
			}
			b, _ := json.Marshal(evidence)
			err = s.transaction(ctx, func(conn *sql.Conn) error {
				if !s.clock().Before(a.root.ExpiresAt) || control.selection.Report(s.clock()).FullOnly {
					return fmt.Errorf("candidate authority expired before evidence commit")
				}
				if operation == "candidate-verify" && !s.clock().Before(record.Tuple.Request.ExpiresAt) {
					return fmt.Errorf("candidate expired before evidence commit")
				}
				row, e := conn.ExecContext(ctx, "UPDATE candidate SET revision=revision+1,status=?,"+column+"=? WHERE id=? AND revision=? AND status!='invalidated'", status, b, record.ID, record.Revision)
				if e != nil {
					return e
				}
				n, e := row.RowsAffected()
				if e != nil || n != 1 {
					return ErrConflict
				}
				return nil
			})
		default:
			return out, fmt.Errorf("unknown candidate operation")
		}
		if err != nil {
			return out, err
		}
	}
	record, err := candidateAt(ctx, s.db, pull)
	out.Candidate = &CandidateReport{Record: record, Boundary: "historical evidence only; no GitHub atomic promotion/provider contract proved; existing main/release/upgrade gates remain"}
	return out, err
}
