package cipolicy

import (
	"encoding/json"
	"fmt"
	"time"
)

// Commit is a complete trusted first-parent integration observation, including
// docs commits. IntegratedAt is the coordinator's merge time, not PR-controlled
// author time. Code classification must use the trusted complete-diff classifier.
type Commit struct {
	SHA, Parent   string
	IntegratedAt  time.Time
	Code          bool
	Compatibility bool
	HighRisk      bool
}

type History struct {
	Root    string
	Commits []Commit
}

// Sweep freezes both ends and the approved control identity. ID belongs to the
// trusted coordinator, not a candidate or a reusable workflow's result label.
type Sweep struct {
	ID, From, Target, Control, Digest string
	Authority                         string
	CompatibilityRequired             bool
}

// State must be persisted by a single serialized trusted coordinator with CAS.
// Reconcile is a pure transition and cannot make concurrent storage atomic.
type State struct {
	Control, Digest, Watermark        string
	Authority                         string
	Root, ObservedTip, ObservedDigest string
	ObservedAt                        time.Time
	Sequence                          uint64
	Active, Pending                   *Sweep
	Failed                            bool
}

func NewState(s Selection, watermark string) (State, error) {
	if s.invalid != "" || s.revision == "" || !revisionOK(watermark) {
		return State{}, fmt.Errorf("approved control and proven watermark required")
	}
	h := History{Root: watermark}
	digest, err := historyDigest(h, -1)
	if err != nil {
		return State{}, err
	}
	return State{Control: s.revision, Digest: s.digest, Watermark: watermark,
		Root: watermark, ObservedTip: watermark, ObservedDigest: digest}, nil
}

// Completion is supplied only after the trusted Actions adapter verifies exact
// target/control/run provenance. All three applicable full scopes must succeed;
// missing, skipped, cancelled and partial results cannot discharge debt.
type Completion struct {
	Sweep                       Sweep
	Linux, MacOS, Compatibility string // optional compatibility uses "not_applicable"
}

type Recovery struct{ Owner, Reason string }

type Event struct {
	Now        time.Time
	History    History
	Completion *Completion
	// Force is a trusted early-coverage request, never an untrusted PR label.
	Force    bool
	Recovery *Recovery
}

type Decision struct {
	Policy    Report
	AllowCode bool
	Reason    string
	Uncovered int
	Oldest    time.Time
	Dispatch  *Sweep
}

// Reconcile never cancels Active. Pending is at most one cumulative successor;
// it has no run until dispatched, and is recomputed after coverage advances.
func Reconcile(old State, policy Selection, e Event) (State, Decision, error) {
	s := cloneState(old)
	d := Decision{Policy: policy.Report(e.Now)}
	fail := func(reason string) (State, Decision, error) {
		d.AllowCode, d.Reason = false, reason
		return old, d, fmt.Errorf("%s", reason)
	}
	if e.Now.IsZero() || s.Control != policy.revision || s.Digest != policy.digest || s.Control == "" {
		return fail("control identity changed or clock missing; coverage cannot be reused")
	}
	positions, err := validateHistory(e.History, e.Now)
	if err != nil {
		return fail(err.Error())
	}
	observed, observedOK := positions[s.ObservedTip]
	if e.History.Root != s.Root || !observedOK || e.Now.Before(s.ObservedAt) {
		return fail("previously observed integration history or clock regressed")
	}
	prefix, err := historyDigest(e.History, observed)
	if err != nil {
		return fail("integration history cannot be serialized")
	}
	if prefix != s.ObservedDigest {
		return fail("previously observed integration history or clock regressed")
	}
	w, ok := positions[s.Watermark]
	if !ok {
		return fail("watermark absent from complete history")
	}
	for _, sweep := range []*Sweep{s.Active, s.Pending} {
		if sweep == nil {
			continue
		}
		from, fromOK := positions[sweep.From]
		target, targetOK := positions[sweep.Target]
		if !fromOK || !targetOK || from >= target || sweep.From != s.Watermark ||
			sweep.Control != s.Control || sweep.Digest != s.Digest || sweep.Authority != s.Authority || sweep.ID == "" {
			return fail("invalid frozen sweep")
		}
		if sweep.CompatibilityRequired != compatibilityRequired(e.History, from, target) {
			return fail("frozen scope does not match cumulative classification")
		}
	}
	if s.Active == nil && s.Pending != nil {
		return fail("pending sweep without active proof")
	}
	if s.Active != nil && s.Pending != nil && positions[s.Pending.Target] <= positions[s.Active.Target] {
		return fail("pending sweep is not cumulative")
	}
	if c := e.Completion; c != nil {
		if s.Active == nil || c.Sweep != *s.Active {
			return fail("completion does not match frozen active sweep")
		}
		compatibilityOK := c.Compatibility == "not_applicable"
		if s.Active.CompatibilityRequired {
			compatibilityOK = c.Compatibility == "success"
		}
		if c.Linux == "success" && c.MacOS == "success" && compatibilityOK {
			s.Watermark = s.Active.Target
			w = positions[s.Watermark]
		} else {
			s.Failed = true
		}
		s.Active, s.Pending = nil, nil
	}
	if e.Recovery != nil {
		if !s.Failed || e.Recovery.Owner != d.Policy.Owner || e.Recovery.Reason == "" || d.Policy.FullOnly {
			return fail("recovery needs the trusted responsible owner and diagnosis")
		}
		s.Failed = false
	}
	s.ObservedTip = e.History.Root
	if len(e.History.Commits) > 0 {
		s.ObservedTip = e.History.Commits[len(e.History.Commits)-1].SHA
	}
	s.ObservedDigest, err = historyDigest(e.History, len(e.History.Commits)-1)
	if err != nil {
		return fail("integration history cannot be serialized")
	}
	s.ObservedAt = e.Now
	for i := w + 1; i < len(e.History.Commits); i++ {
		c := e.History.Commits[i]
		if c.Code {
			d.Uncovered++
			if d.Oldest.IsZero() {
				d.Oldest = c.IntegratedAt
			}
		}
	}
	if d.Policy.FullOnly {
		d.Reason = d.Policy.BlockedReason
		return s, d, nil
	}
	if s.Failed {
		d.Reason = "full sweep failed; diagnose before recovery"
		return s, d, nil
	}
	limits := d.Policy.Limits
	age := time.Duration(0)
	if !d.Oldest.IsZero() {
		age = e.Now.Sub(d.Oldest)
	}
	maxAge := time.Duration(limits.MaxAgeSeconds) * time.Second
	lead := time.Duration(limits.LeadTimeSeconds) * time.Second
	d.AllowCode = d.Uncovered < limits.MaxChanges && (d.Uncovered == 0 || age < maxAge)
	switch {
	case d.Uncovered >= limits.MaxChanges:
		d.Reason = "uncovered change ceiling reached"
	case d.Uncovered > 0 && age >= maxAge:
		d.Reason = "uncovered age ceiling reached"
	default:
		d.Reason = "within coverage budget"
	}
	// Empty and already-covered ranges never allocate a run, even on Force.
	if d.Uncovered == 0 {
		return s, d, nil
	}
	due := e.Force || d.Uncovered >= limits.MaxChanges-1 || age >= maxAge-lead
	if !due {
		return s, d, nil
	}
	target := e.History.Commits[len(e.History.Commits)-1].SHA
	if s.Active != nil {
		// A docs-only tail after Active creates no additional runtime debt.
		newCode := false
		for i := positions[s.Active.Target] + 1; i < len(e.History.Commits); i++ {
			newCode = newCode || e.History.Commits[i].Code
		}
		if newCode {
			s.Pending = &Sweep{ID: "pending", From: s.Watermark, Target: target, Control: s.Control, Digest: s.Digest, Authority: s.Authority,
				CompatibilityRequired: compatibilityRequired(e.History, w, len(e.History.Commits)-1)}
		}
		return s, d, nil
	}
	if s.Sequence == ^uint64(0) {
		return fail("sweep sequence exhausted")
	}
	s.Sequence++
	s.Active = &Sweep{ID: fmt.Sprintf("%s/%d", s.Control, s.Sequence), From: s.Watermark, Target: target, Control: s.Control, Digest: s.Digest, Authority: s.Authority,
		CompatibilityRequired: compatibilityRequired(e.History, w, len(e.History.Commits)-1)}
	copy := *s.Active
	d.Dispatch = &copy
	return s, d, nil
}

func cloneState(s State) State {
	if s.Active != nil {
		a := *s.Active
		s.Active = &a
	}
	if s.Pending != nil {
		p := *s.Pending
		s.Pending = &p
	}
	return s
}

func validateHistory(h History, now time.Time) (map[string]int, error) {
	if !revisionOK(h.Root) {
		return nil, fmt.Errorf("invalid history root")
	}
	positions := map[string]int{h.Root: -1}
	parent := h.Root
	var previous time.Time
	for i, c := range h.Commits {
		_, duplicate := positions[c.SHA]
		if !revisionOK(c.SHA) || duplicate || c.Parent != parent || c.IntegratedAt.IsZero() || (c.Compatibility && !c.Code) ||
			c.IntegratedAt.After(now) || (!previous.IsZero() && c.IntegratedAt.Before(previous)) {
			return nil, fmt.Errorf("incomplete, rewritten or invalid integration history")
		}
		positions[c.SHA], parent, previous = i, c.SHA, c.IntegratedAt
	}
	return positions, nil
}

func historyDigest(h History, last int) (string, error) {
	// Fixed struct encoding covers classification and merge time as well as
	// ancestry. A later event may only append to this previously seen prefix.
	prefix := History{Root: h.Root}
	if last < -1 || last >= len(h.Commits) {
		return "", fmt.Errorf("invalid history prefix")
	}
	if last >= 0 {
		prefix.Commits = h.Commits[:last+1]
	}
	b, err := json.Marshal(prefix)
	if err != nil {
		return "", err
	}
	return Digest(b), nil
}

func compatibilityRequired(h History, from, target int) bool {
	for i := from + 1; i <= target; i++ {
		if h.Commits[i].Compatibility {
			return true
		}
	}
	return false
}
