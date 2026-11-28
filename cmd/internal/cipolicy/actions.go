package cipolicy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// AuthorityRoot is signed by an independently provisioned authority key. Neither
// key nor root may come from a PR checkout/input/artifact. The durable lane pins
// the key and repository identity; a new root cannot reset that lane's debt.
// Approval declarations are requirements, not evidence: GitHub is queried too.
type AuthorityRoot struct {
	Candidate           *CandidateRequirement `json:",omitempty"`
	CoordinatorDigest   string
	RoutingEnabled      bool
	IntegrationWrite    IntegrationWriteContract
	Routes              map[int]RouteGrant
	MaxRequests         int
	RiskReviewers       []Identity
	RequiredPullReviews map[int][]Identity

	Version                                                int
	RepositoryID                                           int64
	Repository, ControlRef, IntegrationRef, CurrentControl string
	Dispatcher                                             Identity
	ExpiresAt                                              time.Time
	Controls                                               map[string]ControlRequirement
	Bootstrap                                              BootstrapRequirement
}

type ControlRequirement struct {
	Pull                                    int
	Head, PolicyDigest                      string
	Owner                                   Identity
	Reviewers                               []Identity // every required provider/authority identity is retained
	WorkflowID                              int64
	WorkflowPath                            string
	ExecutionFiles                          map[string]string // approved execution closure: path -> SHA-256
	LinuxJobs, MacOSJobs, CompatibilityJobs []string
	RequiredSteps, RunnerLabels             map[string][]string
	OptionalSkippedJobs                     []string
	DevelopmentJobs                         []string
}

type Identity struct {
	ID    int64
	Login string
}
type BootstrapRequirement struct {
	Target  string
	RunID   int64
	Attempt int
	Sweep   Sweep
}

type Adapter struct {
	root        AuthorityRoot
	keyDigest   string
	client      *http.Client
	base, token string
	gitBase     string
	clock       func() time.Time
}

// NewAdapter verifies an independently signed canonical root. Production API
// traffic is fixed to GitHub HTTPS; redirects cannot redirect credentials.
// Provisioning the pinned public key is an external trust bootstrap, not a PR
// approval or an instruction to activate this adapter.
func NewAdapter(key ed25519.PublicKey, payload, signature []byte, token string) (*Adapter, error) {
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, payload, signature) {
		return nil, fmt.Errorf("untrusted authority root")
	}
	var root AuthorityRoot
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	if err := d.Decode(&root); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(root)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, fmt.Errorf("noncanonical authority root")
	}
	if root.MaxRequests < 16 || root.MaxRequests > 500 || len(root.RiskReviewers) == 0 || root.Version != 1 || root.RepositoryID <= 0 || len(strings.Split(root.Repository, "/")) != 2 ||
		strings.ContainsAny(root.Repository, "?#%\\") || path.Clean(root.Repository) != root.Repository || root.IntegrationRef == "" || root.ControlRef == "" || root.ControlRef == root.IntegrationRef || root.Dispatcher.ID <= 0 || root.Dispatcher.Login == "" ||
		strings.ContainsAny(root.IntegrationRef, "?#%\\") || !revisionOK(root.CurrentControl) || root.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("invalid authority root")
	}
	for revision, c := range root.Controls {
		if !revisionOK(revision) || c.Pull <= 0 || !revisionOK(c.Head) || !digestOK(c.PolicyDigest) || c.Owner.ID <= 0 || c.Owner.Login == "" || len(c.Reviewers) == 0 || c.WorkflowID <= 0 || !safePath(c.WorkflowPath) || len(c.LinuxJobs) == 0 || len(c.MacOSJobs) == 0 || len(c.CompatibilityJobs) == 0 || len(c.ExecutionFiles) == 0 {
			return nil, fmt.Errorf("incomplete control requirements")
		}
		if !digestOK(c.ExecutionFiles[c.WorkflowPath]) {
			return nil, fmt.Errorf("workflow absent from approved execution closure")
		}
		for p, d := range c.ExecutionFiles {
			if !safePath(p) || !digestOK(d) {
				return nil, fmt.Errorf("invalid execution closure")
			}
		}
		seen := map[int64]bool{}
		for _, r := range c.Reviewers {
			if r.ID <= 0 || r.Login == "" || seen[r.ID] {
				return nil, fmt.Errorf("invalid required reviewer")
			}
			seen[r.ID] = true
		}
		jobs := map[string]bool{}
		for _, list := range [][]string{c.LinuxJobs, c.MacOSJobs, c.CompatibilityJobs} {
			for _, name := range list {
				if name == "" || jobs[name] || len(c.RequiredSteps[name]) == 0 || len(c.RunnerLabels[name]) == 0 {
					return nil, fmt.Errorf("ambiguous required job")
				}
				jobs[name] = true
			}
		}

		for _, name := range c.DevelopmentJobs {
			if name == "" || len(c.RequiredSteps[name]) == 0 || len(c.RunnerLabels[name]) == 0 {
				return nil, fmt.Errorf("incomplete development feedback requirements")
			}
		}
	}
	if _, ok := root.Controls[root.CurrentControl]; !ok {
		return nil, fmt.Errorf("current control unapproved")
	}
	if root.Candidate != nil {
		if err := candidateShape(root.Candidate); err != nil {
			return nil, err
		}
	}
	return &Adapter{root: root, keyDigest: Digest(key), base: "https://api.github.com", token: token, clock: time.Now,
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("API redirect refused") }}}, nil
}

func safePath(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.HasPrefix(p, "../") && !strings.ContainsAny(p, "?#%\\")
}
func (a *Adapter) prefix() string { return "/repos/" + a.root.Repository }
func (a *Adapter) request(ctx context.Context, method, p string, body any, out any) error {
	req, err := a.prepareRequest(ctx, method, p, body)
	if err != nil {
		return err
	}
	return a.send(req, out)
}
func (a *Adapter) prepareRequest(ctx context.Context, method, p string, body any) (*http.Request, error) {
	if b, ok := ctx.Value(budgetKey{}).(*apiBudget); ok && b.used.Add(1) > b.max {
		return nil, fmt.Errorf("trusted API request budget exhausted")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+p, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
func (a *Adapter) send(req *http.Request, out any) error {
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode, method: req.Method, path: req.URL.Path}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil || len(b) > 8<<20 {
		return fmt.Errorf("incomplete API response")
	}
	if out == nil {
		return nil
	}
	if len(b) == 0 {
		return fmt.Errorf("missing API response")
	}
	return json.Unmarshal(b, out)
}

type repoRecord struct {
	ID            int64
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}
type pullRecord struct {
	Number   int
	State    string
	Draft    bool
	Merged   bool
	MergedAt time.Time `json:"merged_at"`
	MergeSHA string    `json:"merge_commit_sha"`
	User     Identity
	Head     struct {
		Ref  string
		SHA  string
		Repo repoRecord
	}
	Base struct {
		SHA  string
		Ref  string
		Repo repoRecord
	}
}
type reviewRecord struct {
	ID          int64
	User        Identity
	State       string
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}
type commitRecord struct {
	SHA     string
	Tree    struct{ SHA string }
	Parents []struct{ SHA string }
}
type contentRecord struct {
	Type, Encoding, Content, SHA string
	Size                         int
}
type runRecord struct {
	HeadBranch                      string    `json:"head_branch"`
	CreatedAt                       time.Time `json:"created_at"`
	UpdatedAt                       time.Time `json:"updated_at"`
	ID                              int64
	RunAttempt                      int    `json:"run_attempt"`
	WorkflowID                      int64  `json:"workflow_id"`
	HeadSHA                         string `json:"head_sha"`
	Event, Path, Status, Conclusion string
	Title                           string `json:"display_title"`
	Repository                      repoRecord
	HeadRepository                  repoRecord `json:"head_repository"`
	Actor                           Identity
	TriggeringActor                 Identity `json:"triggering_actor"`
}
type jobRecord struct {
	CheckRunURL              string    `json:"check_run_url"`
	StartedAt                time.Time `json:"started_at"`
	CompletedAt              time.Time `json:"completed_at"`
	ID                       int64
	RunID                    int64  `json:"run_id"`
	Attempt                  int    `json:"run_attempt"`
	Head                     string `json:"head_sha"`
	Name, Status, Conclusion string
	RunnerID                 int64 `json:"runner_id"`
	Labels                   []string
	Steps                    []stepRecord
}
type stepRecord struct{ Name, Status, Conclusion string }

func (a *Adapter) repository(ctx context.Context) error {
	if !a.clock().Before(a.root.ExpiresAt) {
		return fmt.Errorf("authority root expired")
	}
	var repo repoRecord
	if err := a.request(ctx, "GET", a.prefix(), nil, &repo); err != nil {
		return err
	}
	if repo.ID != a.root.RepositoryID || repo.FullName != a.root.Repository || repo.DefaultBranch != a.root.ControlRef {
		return fmt.Errorf("repository identity mismatch")
	}
	return nil
}

func (a *Adapter) content(ctx context.Context, revision, p string) ([]byte, error) {
	if !revisionOK(revision) || !safePath(p) {
		return nil, fmt.Errorf("mutable or unsafe source")
	}
	var c contentRecord
	if err := a.request(ctx, "GET", a.prefix()+"/contents/"+p+"?ref="+revision, nil, &c); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil || c.Type != "file" || c.Encoding != "base64" || len(b) != c.Size {
		return nil, fmt.Errorf("incomplete immutable source")
	}
	return b, nil
}

func (a *Adapter) reviews(ctx context.Context, pull int) ([]reviewRecord, error) {
	var all []reviewRecord
	for page := 1; page <= 100; page++ {
		var batch []reviewRecord
		if err := a.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d/reviews?per_page=100&page=%d", a.prefix(), pull, page), nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("review enumeration incomplete")
}

// verifiedControl cannot be manufactured by a candidate-supplied Trust value.
type verifiedControl struct {
	approvedAt  time.Time
	selection   Selection
	requirement ControlRequirement
	revision    string
	authority   string
}

// Expiry refreshes do not alter scope. Every gate, execution and review binding
// does: it must use the explicit serialized control-switch path, never silently
// change a frozen proof or reuse its old coverage.
func (a *Adapter) authorityDigest() string {
	r := a.root
	r.ExpiresAt = time.Time{}
	r.Bootstrap = BootstrapRequirement{}
	r.Routes = nil
	r.RoutingEnabled = false
	// A candidate is its own frozen, sealed request, like an owner grant. It
	// cannot change integration gate scope or reset coverage debt.
	r.Candidate = nil
	b, _ := json.Marshal(r)
	return Digest(b)
}

type controlSource map[string][]byte

func (s controlSource) PolicyAt(r string) ([]byte, error) {
	b, ok := s[r]
	if !ok {
		return nil, fmt.Errorf("unverified control")
	}
	return b, nil
}

func (a *Adapter) control(ctx context.Context) (verifiedControl, error) {
	if err := a.repository(ctx); err != nil {
		return verifiedControl{}, err
	}
	policies := controlSource{}
	approved := map[string]string{}
	var approvedAt time.Time
	for revision, requirement := range a.root.Controls {
		var pr pullRecord
		if err := a.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", a.prefix(), requirement.Pull), nil, &pr); err != nil {
			return verifiedControl{}, err
		}
		if pr.Number != requirement.Pull || !pr.Merged || pr.MergedAt.IsZero() || pr.MergeSHA != revision || pr.Head.SHA != requirement.Head || pr.Base.Repo.ID != a.root.RepositoryID || pr.Base.Ref != a.root.ControlRef || pr.Head.Repo.ID != a.root.RepositoryID {
			return verifiedControl{}, fmt.Errorf("control is not the approved merged repository revision")
		}
		reviews, err := a.reviews(ctx, requirement.Pull)
		if err != nil {
			return verifiedControl{}, err
		}
		if err := reviewRequirements(reviews, requirement.Reviewers, requirement.Head, pr.MergedAt, pr.User.ID); err != nil {
			return verifiedControl{}, err
		}
		if revision == a.root.CurrentControl {
			approvedAt = pr.MergedAt
		}
		var owner Identity
		if err := a.request(ctx, "GET", "/users/"+url.PathEscape(requirement.Owner.Login), nil, &owner); err != nil {
			return verifiedControl{}, err
		}
		if owner != requirement.Owner {
			return verifiedControl{}, fmt.Errorf("owner identity mismatch")
		}
		b, err := a.content(ctx, revision, PolicyPath)
		if err != nil {
			return verifiedControl{}, err
		}
		if Digest(b) != requirement.PolicyDigest {
			return verifiedControl{}, fmt.Errorf("policy object mismatch")
		}
		for p, digest := range requirement.ExecutionFiles {
			b, err := a.content(ctx, revision, p)
			if err != nil || Digest(b) != digest {
				return verifiedControl{}, fmt.Errorf("approved execution object mismatch")
			}
		}
		policies[revision], approved[revision] = b, requirement.PolicyDigest
	}
	s := resolveApproved(policies, Trust{Revision: a.root.CurrentControl, Approved: approved})
	if s.invalid != "" {
		return verifiedControl{}, fmt.Errorf("control closed: %s", s.invalid)
	}
	c := a.root.Controls[a.root.CurrentControl]
	if s.policy.Owner != c.Owner.Login {
		return verifiedControl{}, fmt.Errorf("policy owner is not the verified responsible identity")
	}
	return verifiedControl{selection: s, requirement: c, revision: a.root.CurrentControl, authority: a.authorityDigest(), approvedAt: approvedAt}, nil
}

func sweepTitle(s Sweep) string {
	b, _ := json.Marshal(s)
	return "integration-sweep " + Digest(b)
}

func (a *Adapter) run(ctx context.Context, c verifiedControl, s Sweep, id int64, attempt int) (runRecord, error) {
	var r runRecord
	if id <= 0 || attempt != 1 {
		return r, fmt.Errorf("unrequested run attempt")
	}
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d/attempts/%d", a.prefix(), id, attempt), nil, &r); err != nil {
		return r, err
	}
	if err := a.runIdentity(r, c, s, id, attempt); err != nil {
		return r, err
	}
	return r, a.executionClosure(ctx, c, s.Target)
}

func (a *Adapter) runIdentity(r runRecord, c verifiedControl, s Sweep, id int64, attempt int) error {
	if r.ID != id || r.RunAttempt != attempt || r.Repository.ID != a.root.RepositoryID || r.Repository.FullName != a.root.Repository || r.HeadRepository.ID != a.root.RepositoryID || r.HeadSHA != s.Target || r.WorkflowID != c.requirement.WorkflowID || r.Path != c.requirement.WorkflowPath || r.Event != "workflow_dispatch" || r.Title != sweepTitle(s) || s.Control != c.revision || s.Digest != c.selection.digest || s.Authority != c.authority || r.Actor != a.root.Dispatcher || r.TriggeringActor != a.root.Dispatcher {
		return fmt.Errorf("run repository/event/attempt/target/control/workflow/scope mismatch")
	}
	return nil
}

// Only immutable target contents are shared. Mutable approvals/run state are
// freshly read by each caller; a dispatch receipt is not full-test evidence.
func (a *Adapter) executionClosure(ctx context.Context, c verifiedControl, target string) error {
	for p, digest := range c.requirement.ExecutionFiles {
		b, err := a.content(ctx, target, p)
		if err != nil || Digest(b) != digest {
			return fmt.Errorf("target modified approved execution closure")
		}
	}
	return nil
}

func (a *Adapter) completion(ctx context.Context, c verifiedControl, s Sweep, id int64, attempt int) (*Completion, error) {
	r, err := a.run(ctx, c, s, id, attempt)
	if err != nil {
		return nil, err
	}
	if r.Status != "completed" {
		return nil, nil
	}
	jobs, err := a.jobs(ctx, s, id, attempt)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, list := range [][]string{c.requirement.LinuxJobs, c.requirement.MacOSJobs, c.requirement.CompatibilityJobs, c.requirement.DevelopmentJobs} {
		for _, name := range list {
			known[name] = true
		}
	}
	for _, name := range c.requirement.OptionalSkippedJobs {
		if j, ok := jobs[name]; ok && j.Status == "completed" && j.Conclusion == "skipped" {
			known[name] = true
		}
	}
	for name := range jobs {
		if !known[name] {
			return nil, fmt.Errorf("job outside approved scope manifest")
		}
	}
	scope := func(names []string) string {
		for _, name := range names {
			j, ok := jobs[name]
			if !ok || j.Status != "completed" || j.Conclusion != "success" || j.RunnerID <= 0 || !jobExecution(j, c.requirement.RequiredSteps[name], c.requirement.RunnerLabels[name]) {
				return "failure"
			}
		}
		if r.Conclusion != "success" {
			return "failure"
		}
		return "success"
	}
	result := &Completion{Sweep: s, Linux: scope(c.requirement.LinuxJobs), MacOS: scope(c.requirement.MacOSJobs), Compatibility: "not_applicable"}
	if s.CompatibilityRequired {
		result.Compatibility = scope(c.requirement.CompatibilityJobs)
	}
	return result, nil
}

func (a *Adapter) jobs(ctx context.Context, s Sweep, id int64, attempt int) (map[string]jobRecord, error) {
	jobs := map[string]jobRecord{}
	ids := map[int64]bool{}
	total := -1
	for page := 1; page <= 100; page++ {
		var batch struct {
			Total int `json:"total_count"`
			Jobs  []jobRecord
		}
		if err := a.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d/attempts/%d/jobs?per_page=100&page=%d", a.prefix(), id, attempt, page), nil, &batch); err != nil {
			return nil, err
		}
		if total < 0 {
			total = batch.Total
		}
		if total != batch.Total || total < 0 {
			return nil, fmt.Errorf("unstable job enumeration")
		}
		for _, j := range batch.Jobs {
			if j.RunID != id || j.Attempt != attempt || j.Head != s.Target || j.ID <= 0 || ids[j.ID] || jobs[j.Name].ID != 0 {
				return nil, fmt.Errorf("ambiguous job provenance")
			}
			ids[j.ID] = true
			jobs[j.Name] = j
		}
		if len(batch.Jobs) < 100 {
			if len(ids) != total {
				return nil, fmt.Errorf("incomplete job enumeration")
			}
			break
		}
		if page == 100 {
			return nil, fmt.Errorf("job enumeration limit")
		}
	}
	return jobs, nil
}

// Observe never accepts external history, merge times, classification, success
// strings or owner strings. It fetches immutable source and GitHub merge data.
type Observation struct {
	expires      time.Time
	control      verifiedControl
	event        Event
	repositoryID int64
	ref, key     string
	runID        int64
	attempt      int
}

func (a *Adapter) Observe(ctx context.Context, snap Snapshot) (Observation, error) {
	ctx = a.budget(ctx)
	if snap.seal == "" || snapshotSeal(snap) != snap.seal || snap.repo != a.root.RepositoryID || snap.key != a.keyDigest || snap.ref != a.root.IntegrationRef {
		return Observation{}, fmt.Errorf("history snapshot is not the durable authenticated lane")
	}
	c, err := a.control(ctx)
	if err != nil {
		return Observation{}, err
	}
	var ref struct{ Object struct{ SHA string } }
	if err := a.request(ctx, "GET", a.prefix()+"/git/ref/heads/"+a.root.IntegrationRef, nil, &ref); err != nil {
		return Observation{}, err
	}
	from := snap.State.ObservedTip
	prefix := snap.History.Commits
	if c.authority != snap.State.Authority {
		// New mandatory review identities apply to the complete retained range.
		// A previous scope's authenticated prefix does not prove those reviews.
		from = snap.State.Root
		prefix = nil
	}
	suffix, err := a.history(ctx, from, ref.Object.SHA)
	if err != nil {
		return Observation{}, err
	}
	h := History{Root: snap.State.Root, Commits: append(append([]Commit{}, prefix...), suffix.Commits...)}
	e := Event{Now: a.clock(), History: h}
	var runID int64
	var attempt int
	if snap.Intent != nil && snap.Intent.Status == "bound" {
		runID = snap.Intent.RunID
		attempt = snap.Intent.Attempt
		e.Completion, err = a.completion(ctx, c, snap.Intent.Sweep, runID, attempt)
		if err != nil {
			return Observation{}, err
		}
	}
	return Observation{control: c, event: e, repositoryID: a.root.RepositoryID, ref: a.root.IntegrationRef, key: a.keyDigest, runID: runID, attempt: attempt, expires: a.root.ExpiresAt}, nil
}

func (a *Adapter) history(ctx context.Context, root, target string) (History, error) {
	if !revisionOK(root) || !revisionOK(target) {
		return History{}, fmt.Errorf("immutable history endpoints required")
	}
	var reversed []Commit
	seen := map[string]bool{}
	for target != root {
		if seen[target] || len(reversed) >= 10000 {
			return History{}, fmt.Errorf("history incomplete or cyclic")
		}
		seen[target] = true
		var object commitRecord
		if err := a.request(ctx, "GET", a.prefix()+"/git/commits/"+target, nil, &object); err != nil {
			return History{}, err
		}
		if object.SHA != target || len(object.Parents) == 0 || !revisionOK(object.Parents[0].SHA) {
			return History{}, fmt.Errorf("invalid immutable ancestry")
		}
		var prs []pullRecord
		if err := a.request(ctx, "GET", a.prefix()+"/commits/"+target+"/pulls?per_page=100", nil, &prs); err != nil {
			return History{}, err
		}
		if len(prs) >= 100 {
			return History{}, fmt.Errorf("merge enumeration incomplete")
		}
		var merged *pullRecord
		for i := range prs {
			p := &prs[i]
			if p.MergedAt.IsZero() || p.MergeSHA != target {
				continue
			}
			if merged != nil || p.Base.Repo.ID != a.root.RepositoryID || p.Base.Ref != a.root.IntegrationRef {
				return History{}, fmt.Errorf("ambiguous integration merge")
			}
			merged = p
		}
		if merged == nil {
			return History{}, fmt.Errorf("no authoritative integration merge time")
		}
		changes, err := a.diff(ctx, object.Parents[0].SHA, target)
		if err != nil {
			return History{}, err
		}
		plan := Classify(changes, true)
		required := append([]Identity{}, a.root.RequiredPullReviews[merged.Number]...)
		if plan.HighRisk {
			if len(a.root.RiskReviewers) == 0 {
				return History{}, fmt.Errorf("high-risk review requirements unestablished")
			}
			required = append(required, a.root.RiskReviewers...)
		}
		if len(required) > 0 {
			reviews, err := a.reviews(ctx, merged.Number)
			if err != nil {
				return History{}, err
			}
			if err := reviewRequirements(reviews, required, merged.Head.SHA, merged.MergedAt, merged.User.ID); err != nil {
				return History{}, err
			}
		}
		reversed = append(reversed, Commit{SHA: target, Parent: object.Parents[0].SHA, IntegratedAt: merged.MergedAt, Code: !plan.DocsOnly, Compatibility: plan.Compatibility, HighRisk: plan.HighRisk})
		target = object.Parents[0].SHA
	}
	h := History{Root: root}
	for i := len(reversed) - 1; i >= 0; i-- {
		h.Commits = append(h.Commits, reversed[i])
	}
	if _, err := validateHistory(h, a.clock()); err != nil {
		return History{}, err
	}
	return h, nil
}

func (a *Adapter) diff(ctx context.Context, base, head string) ([]Change, error) {
	var comparison struct {
		Base      struct{ SHA string } `json:"base_commit"`
		MergeBase struct{ SHA string } `json:"merge_base_commit"`
		Status    string
		Files     []struct {
			Filename, Status string
			Previous         string `json:"previous_filename"`
		}
	}
	if err := a.request(ctx, "GET", a.prefix()+"/compare/"+base+"..."+head+"?per_page=100", nil, &comparison); err != nil {
		return nil, err
	}
	// Compare's files list is capped at 300. Refuse the boundary rather than
	// credit a possibly truncated listing. Selection may escalate, never skip.
	if comparison.Base.SHA != base || comparison.MergeBase.SHA != base || comparison.Status != "ahead" || len(comparison.Files) >= 300 {
		return nil, fmt.Errorf("diff completeness unproved")
	}
	changes := make([]Change, 0, len(comparison.Files))
	for _, f := range comparison.Files {
		changes = append(changes, Change{Path: f.Filename, Previous: f.Previous, Status: f.Status})
	}
	return changes, nil
}

type Intent struct {
	Sweep   Sweep
	RunID   int64
	Attempt int
	Status  string
}

// Discovery does not claim approval. Only the subsequent current-run and full
// completion verification credit the unique ID, without rereading the entire
// immutable execution closure once per discovery/verification phase.
func (a *Adapter) discoverRun(ctx context.Context, c verifiedControl, s Sweep) (int64, error) {
	var found int64
	total := -1
	seen := map[int64]bool{}
	for page := 1; page <= 10; page++ {
		var batch struct {
			Total int         `json:"total_count"`
			Runs  []runRecord `json:"workflow_runs"`
		}
		p := fmt.Sprintf("%s/actions/workflows/%d/runs?event=workflow_dispatch&branch=%s&head_sha=%s&per_page=100&page=%d", a.prefix(), c.requirement.WorkflowID, url.QueryEscape(strings.TrimPrefix(dispatchRef(s), "heads/")), s.Target, page)
		if err := a.request(ctx, "GET", p, nil, &batch); err != nil {
			return 0, err
		}
		if total < 0 {
			total = batch.Total
		}
		if total != batch.Total || total < 0 || total > 1000 {
			return 0, fmt.Errorf("candidate run enumeration unstable")
		}
		for _, run := range batch.Runs {
			if run.ID <= 0 || seen[run.ID] {
				return 0, fmt.Errorf("candidate run identity ambiguous")
			}
			seen[run.ID] = true
			if run.Title != sweepTitle(s) {
				continue
			}
			if found != 0 {
				return 0, fmt.Errorf("duplicate frozen candidate run")
			}
			found = run.ID
		}
		if len(batch.Runs) < 100 {
			if len(seen) != total {
				return 0, fmt.Errorf("candidate run enumeration incomplete")
			}
			return found, nil
		}
	}
	return 0, fmt.Errorf("candidate run enumeration incomplete")
}

func (a *Adapter) find(ctx context.Context, c verifiedControl, s Sweep) (int64, error) {
	id, err := a.discoverRun(ctx, c, s)
	if err != nil || id == 0 {
		return id, err
	}
	_, err = a.run(ctx, c, s, id, 1)
	return id, err
}

type dispatchRequest struct {
	request *http.Request
}

func dispatchRef(s Sweep) string { return "heads/ci-sweeps/" + Digest([]byte(s.ID)) }

func (a *Adapter) prepareDispatch(ctx context.Context, c verifiedControl, s Sweep) (dispatchRequest, error) {
	var actor Identity
	if err := a.request(ctx, "GET", "/user", nil, &actor); err != nil {
		return dispatchRequest{}, err
	}
	if actor != a.root.Dispatcher {
		return dispatchRequest{}, fmt.Errorf("untrusted dispatcher credential")
	}
	var workflow struct {
		ID          int64
		Path, State string
	}
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/actions/workflows/%d", a.prefix(), c.requirement.WorkflowID), nil, &workflow); err != nil {
		return dispatchRequest{}, err
	}
	if workflow.ID != c.requirement.WorkflowID || workflow.Path != c.requirement.WorkflowPath || workflow.State != "active" {
		return dispatchRequest{}, fmt.Errorf("registered workflow contract mismatch")
	}
	if err := a.executionClosure(ctx, c, s.Target); err != nil {
		return dispatchRequest{}, err
	}
	ref := dispatchRef(s)
	var record struct{ Object struct{ SHA string } }
	err := a.request(ctx, "GET", a.prefix()+"/git/ref/"+ref, nil, &record)
	if err != nil {
		var httpErr *apiError
		if !errors.As(err, &httpErr) || httpErr.status != 404 {
			return dispatchRequest{}, err
		}
		// Git ref creation is keyed by one immutable name. A lost response is
		// reconciled with GET; retrying creation cannot create a second ref/run.
		createErr := a.request(ctx, "POST", a.prefix()+"/git/refs", map[string]string{"ref": "refs/" + ref, "sha": s.Target}, nil)
		if err := a.request(ctx, "GET", a.prefix()+"/git/ref/"+ref, nil, &record); err != nil {
			if createErr != nil {
				return dispatchRequest{}, createErr
			}
			return dispatchRequest{}, err
		}
	}
	if record.Object.SHA != s.Target {
		return dispatchRequest{}, fmt.Errorf("frozen dispatch ref moved; quarantine")
	}
	body := map[string]any{"ref": strings.TrimPrefix(ref, "heads/"), "inputs": map[string]string{"purpose": "sweep", "intent": s.ID, "from": s.From, "target": s.Target, "control": s.Control, "digest": s.Digest, "authority": s.Authority, "scope": strconv.FormatBool(s.CompatibilityRequired), "title": sweepTitle(s)}}
	request, err := a.prepareRequest(ctx, "POST", fmt.Sprintf("%s/actions/workflows/%d/dispatches", a.prefix(), c.requirement.WorkflowID), body)
	return dispatchRequest{request: request}, err
}

func (a *Adapter) dispatch(prepared dispatchRequest) error {
	return a.send(prepared.request, nil)
}

type apiError struct {
	status       int
	method, path string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub %s %s: HTTP %d", e.method, e.path, e.status)
}

type budgetKey struct{}
type apiBudget struct {
	used atomic.Int64
	max  int64
}

func (a *Adapter) budget(ctx context.Context) context.Context {
	return context.WithValue(ctx, budgetKey{}, &apiBudget{max: int64(a.root.MaxRequests)})
}
func reviewRequirements(reviews []reviewRecord, required []Identity, head string, merged time.Time, author int64) error {
	if !revisionOK(head) || merged.IsZero() {
		return fmt.Errorf("immutable reviewed head unavailable")
	}
	latest := map[int64]reviewRecord{}
	seen := map[int64]bool{}
	for _, r := range reviews {
		if r.ID <= 0 || seen[r.ID] || r.SubmittedAt.IsZero() {
			return fmt.Errorf("invalid review enumeration")
		}
		seen[r.ID] = true
		if r.ID > latest[r.User.ID].ID {
			latest[r.User.ID] = r
		}
	}
	for _, who := range required {
		r := latest[who.ID]
		if who.ID <= 0 || who.Login == "" || r.User != who || r.User.ID == author || r.State != "APPROVED" || r.CommitID != head || r.SubmittedAt.After(merged) {
			return fmt.Errorf("mandatory provider/authority exact-head approval missing")
		}
	}
	return nil
}

func jobExecution(j jobRecord, steps, labels []string) bool {
	for _, label := range labels {
		found := false
		for _, got := range j.Labels {
			found = found || got == label
		}
		if !found {
			return false
		}
	}
	observed := map[string]stepRecord{}
	for _, s := range j.Steps {
		if _, duplicate := observed[s.Name]; duplicate {
			return false
		}
		observed[s.Name] = s
	}
	for _, name := range steps {
		s, ok := observed[name]
		if !ok || s.Status != "completed" || s.Conclusion != "success" {
			return false
		}
	}
	return true
}
