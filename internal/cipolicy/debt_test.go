package cipolicy

import (
	"reflect"
	"testing"
	"time"
)

func history(count int, at time.Time) History {
	h := History{Root: sha(100)}
	parent := h.Root
	for i := 0; i < count; i++ {
		c := Commit{SHA: sha(101 + i), Parent: parent, IntegratedAt: at.Add(time.Duration(i) * time.Minute), Code: true}
		h.Commits = append(h.Commits, c)
		parent = c.SHA
	}
	return h
}

func initial(t *testing.T, s Selection, h History) State {
	t.Helper()
	st, err := NewState(s, h.Root)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func step(t *testing.T, st State, s Selection, e Event) (State, Decision) {
	t.Helper()
	next, d, err := Reconcile(st, s, e)
	if err != nil {
		t.Fatal(err)
	}
	return next, d
}

func green(sweep *Sweep) *Completion {
	c := &Completion{Sweep: *sweep, Linux: "success", MacOS: "success", Compatibility: "not_applicable"}
	if sweep.CompatibilityRequired {
		c.Compatibility = "success"
	}
	return c
}

func TestFrozenActiveAndCumulativePendingKeepNewDebt(t *testing.T) {
	s := selected(t, validPolicy())
	h := history(1, clock)
	st := initial(t, s, h)
	st, d := step(t, st, s, Event{Now: clock, History: h, Force: true})
	if d.Dispatch == nil || st.Active.From != h.Root || st.Active.Target != sha(101) {
		t.Fatal(st, d)
	}
	frozen := *st.Active
	for count := 2; count <= 4; count++ {
		h = history(count, clock)
		next, d := step(t, st, s, Event{Now: clock.Add(10 * time.Minute), History: h, Force: true})
		if *next.Active != frozen || d.Dispatch != nil || next.Pending == nil || next.Pending.Target != sha(100+count) || next.Pending.From != h.Root {
			t.Fatal(next, d)
		}
		if st.Pending != nil && st.Pending.Target == next.Pending.Target {
			t.Fatal("input state aliased")
		}
		st = next
	}
	// The older green H advances only to H, preserving the age/count of H+1..N.
	st, d = step(t, st, s, Event{Now: clock.Add(20 * time.Minute), History: h, Completion: green(st.Active)})
	if st.Watermark != sha(101) || d.Uncovered != 3 || !d.Oldest.Equal(clock.Add(time.Minute)) || st.Active != nil {
		t.Fatal(st, d)
	}
	st, d = step(t, st, s, Event{Now: clock.Add(20 * time.Minute), History: h, Force: true})
	if d.Dispatch == nil || d.Dispatch.From != sha(101) || d.Dispatch.Target != sha(104) {
		t.Fatal(st, d)
	}
	st, d = step(t, st, s, Event{Now: clock.Add(21 * time.Minute), History: h, Completion: green(st.Active), Force: true})
	if st.Watermark != sha(104) || d.Uncovered != 0 || d.Dispatch != nil || st.Active != nil || st.Pending != nil {
		t.Fatal(st, d)
	}
	for i := 0; i < 3; i++ {
		st, d = step(t, st, s, Event{Now: clock.Add(time.Hour), History: h, Force: true})
		if d.Dispatch != nil {
			t.Fatal("already validated range dispatched")
		}
	}
}

func TestEmptyDocsAndAlreadyActiveRangesNeverDispatch(t *testing.T) {
	s := selected(t, validPolicy())
	for _, count := range []int{0, 3} {
		h := history(count, clock)
		for i := range h.Commits {
			h.Commits[i].Code = false
		}
		st, d := step(t, initial(t, s, h), s, Event{Now: clock.Add(time.Hour), History: h, Force: true})
		if d.Dispatch != nil || !d.AllowCode || st.Active != nil || d.Uncovered != 0 {
			t.Fatal(st, d)
		}
	}
	h := history(1, clock)
	st, d := step(t, initial(t, s, h), s, Event{Now: clock, History: h, Force: true})
	frozen := *d.Dispatch
	h = history(3, clock)
	h.Commits[1].Code = false
	h.Commits[2].Code = false
	st, d = step(t, st, s, Event{Now: clock.Add(time.Hour), History: h, Force: true})
	if *st.Active != frozen || st.Pending != nil || d.Dispatch != nil {
		t.Fatal("docs tail duplicated active proof", st, d)
	}
}

func TestAdmissionBoundsAndLateSchedule(t *testing.T) {
	p := validPolicy()
	p.Limits = Limits{MaxChanges: 3, MaxAgeSeconds: 3600, LeadTimeSeconds: 600}
	s := selected(t, p)
	for _, tc := range []struct {
		name       string
		count      int
		now        time.Time
		allow, due bool
	}{
		{"fresh", 1, clock.Add(10 * time.Minute), true, false},
		{"count lead", 2, clock.Add(10 * time.Minute), true, true},
		{"count ceiling", 3, clock.Add(10 * time.Minute), false, true},
		{"time lead", 1, clock.Add(50 * time.Minute), true, true},
		{"exact time ceiling", 1, clock.Add(time.Hour), false, true},
		{"late cron", 1, clock.Add(2 * time.Hour), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := history(tc.count, clock)
			_, d := step(t, initial(t, s, h), s, Event{Now: tc.now, History: h})
			if d.AllowCode != tc.allow || (d.Dispatch != nil) != tc.due {
				t.Fatal(d)
			}
		})
	}
	h := history(3, clock)
	st, d := step(t, initial(t, s, h), s, Event{Now: clock.Add(2 * time.Hour), History: h})
	active := *d.Dispatch
	for i := 0; i < 3; i++ {
		st, d = step(t, st, s, Event{Now: clock.Add(3 * time.Hour), History: h})
		if d.AllowCode || d.Dispatch != nil || *st.Active != active {
			t.Fatal("late schedule cancelled/duplicated sole proof", st, d)
		}
	}
}

func TestFailureRetainsWatermarkAndRequiresDiagnosedRecovery(t *testing.T) {
	s := selected(t, validPolicy())
	h := history(2, clock)
	st, _ := step(t, initial(t, s, h), s, Event{Now: clock.Add(time.Hour), History: h, Force: true})
	for _, status := range []string{"failure", "skipped", "cancelled", ""} {
		c := green(st.Active)
		c.MacOS = status
		next, d := step(t, st, s, Event{Now: clock.Add(time.Hour), History: h, Completion: c, Force: true})
		if !next.Failed || next.Watermark != h.Root || d.AllowCode || d.Dispatch != nil || d.Uncovered != 2 {
			t.Fatal(next, d)
		}
		next, d = step(t, next, s, Event{Now: clock.Add(2 * time.Hour), History: h, Force: true})
		if d.Dispatch != nil || d.AllowCode {
			t.Fatal("automatic unchanged retry", d)
		}
		for _, r := range []*Recovery{{Owner: "candidate", Reason: "retry"}, {Owner: s.policy.Owner}} {
			if _, _, err := Reconcile(next, s, Event{Now: clock.Add(2 * time.Hour), History: h, Recovery: r}); err == nil {
				t.Fatal("unauthorized/undiagnosed recovery")
			}
		}
		next, d = step(t, next, s, Event{Now: clock.Add(2 * time.Hour), History: h, Recovery: &Recovery{Owner: s.policy.Owner, Reason: "isolated repair validated"}, Force: true})
		if next.Failed || d.Dispatch == nil || next.Active.From != h.Root || next.Active.ID == st.Active.ID {
			t.Fatal(next, d)
		}
	}
}

func TestCompletionAndHistoryMustMatchFrozenIdentity(t *testing.T) {
	s := selected(t, validPolicy())
	h := history(2, clock)
	st, _ := step(t, initial(t, s, h), s, Event{Now: clock.Add(time.Hour), History: h, Force: true})
	for _, edit := range []func(*Completion){func(c *Completion) { c.Sweep.Target = sha(999) }, func(c *Completion) { c.Sweep.From = sha(999) }, func(c *Completion) { c.Sweep.Control = sha(999) }, func(c *Completion) { c.Sweep.Digest = "wrong" }, func(c *Completion) { c.Sweep.ID = "stale attempt" }} {
		c := green(st.Active)
		edit(c)
		next, _, err := Reconcile(st, s, Event{Now: clock.Add(time.Hour), History: h, Completion: c})
		if err == nil || !reflect.DeepEqual(next, st) {
			t.Fatal("moved/spoofed completion mutated debt")
		}
	}
	for _, edit := range []func(*History){func(h *History) { h.Commits[1].Parent = h.Root }, func(h *History) { h.Commits[1].SHA = h.Commits[0].SHA }, func(h *History) { h.Commits[0].IntegratedAt = clock.Add(2 * time.Hour) }, func(h *History) { h.Commits[1].IntegratedAt = clock.Add(-time.Hour) }, func(h *History) { h.Root = sha(999) }} {
		bad := history(2, clock)
		edit(&bad)
		if _, _, err := Reconcile(st, s, Event{Now: clock.Add(time.Hour), History: bad}); err == nil {
			t.Fatal("rewritten/incomplete history accepted")
		}
	}
	changed := selected(t, validPolicy())
	changed.revision = sha(9)
	if _, _, err := Reconcile(st, changed, Event{Now: clock.Add(time.Hour), History: h}); err == nil {
		t.Fatal("coverage reused under different control")
	}
}

func TestExpiredPolicyPreservesActiveProofButBlocksAdmission(t *testing.T) {
	p := validPolicy()
	p.ExpiresAt = clock.Add(time.Hour)
	s := selected(t, p)
	h := history(1, clock)
	st, _ := step(t, initial(t, s, h), s, Event{Now: clock, History: h, Force: true})
	frozen := *st.Active
	st, d := step(t, st, s, Event{Now: p.ExpiresAt, History: h, Force: true})
	if d.AllowCode || !d.Policy.FullOnly || d.Dispatch != nil || *st.Active != frozen {
		t.Fatal(st, d)
	}
	st, d = step(t, st, s, Event{Now: p.ExpiresAt, History: h, Completion: green(st.Active)})
	if st.Watermark != sha(101) || d.AllowCode || !d.Policy.FullOnly || d.Dispatch != nil {
		t.Fatal(st, d)
	}
}

func TestPreviouslyObservedDebtCannotDisappear(t *testing.T) {
	s := selected(t, validPolicy())
	h := history(2, clock)
	st, d := step(t, initial(t, s, h), s, Event{Now: clock.Add(10 * time.Minute), History: h})
	if d.Dispatch != nil || d.Uncovered != 2 {
		t.Fatal(d)
	}
	for _, edit := range []func(*History){
		func(h *History) { h.Commits = nil },
		func(h *History) { h.Commits = h.Commits[:1] },
		func(h *History) { h.Commits[0].Code = false },
		func(h *History) { h.Commits[0].IntegratedAt = clock.Add(30 * time.Second) },
		func(h *History) { h.Commits[0].Compatibility = true },
	} {
		bad := history(2, clock)
		edit(&bad)
		next, decision, err := Reconcile(st, s, Event{Now: clock.Add(11 * time.Minute), History: bad})
		if err == nil || decision.AllowCode || !reflect.DeepEqual(next, st) {
			t.Fatal("observed debt erased/rewritten", next, decision, err)
		}
	}
	if _, d, err := Reconcile(st, s, Event{Now: clock.Add(9 * time.Minute), History: h}); err == nil || d.AllowCode {
		t.Fatal("coordinator clock rewind accepted")
	}
	h = history(3, clock)
	_, d = step(t, st, s, Event{Now: clock.Add(12 * time.Minute), History: h})
	if d.Uncovered != 3 || !d.Oldest.Equal(clock) {
		t.Fatal("valid append lost debt", d)
	}
}

func TestCompatibilityApplicabilityIsFrozenFromCompleteRange(t *testing.T) {
	s := selected(t, validPolicy())
	for _, applicable := range []bool{false, true} {
		h := history(2, clock)
		// Compatibility code first, ordinary last: the entire range matters.
		h.Commits[0].Compatibility = applicable
		st, d := step(t, initial(t, s, h), s, Event{Now: clock.Add(time.Hour), History: h, Force: true})
		if d.Dispatch.CompatibilityRequired != applicable {
			t.Fatal("last commit hid required scope")
		}
		completed, result := step(t, st, s, Event{Now: clock.Add(time.Hour), History: h, Completion: green(st.Active)})
		if completed.Failed || result.Uncovered != 0 {
			t.Fatal("applicable successful scopes did not discharge debt", completed, result)
		}
		for _, status := range []string{"", "skipped", "cancelled", "failure"} {
			c := green(st.Active)
			c.Compatibility = status
			next, d := step(t, st, s, Event{Now: clock.Add(time.Hour), History: h, Completion: c})
			if !next.Failed || d.AllowCode || next.Watermark != h.Root {
				t.Fatal("missing/skipped applicable evidence accepted", applicable, status, next, d)
			}
		}
		// An after-dispatch scope edit cannot change the frozen requirement.
		c := green(st.Active)
		c.Sweep.CompatibilityRequired = !applicable
		if _, _, err := Reconcile(st, s, Event{Now: clock.Add(time.Hour), History: h, Completion: c}); err == nil {
			t.Fatal("completion rewrote scope")
		}
	}
}

func TestUnserializableTimeCannotCollapseHistoryFingerprint(t *testing.T) {
	s := selected(t, validPolicy())
	for _, at := range []time.Time{
		time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		clock.Add(-time.Hour).In(time.FixedZone("invalid JSON offset", 25*60*60)),
	} {
		h := history(1, at)
		initialState := initial(t, s, h)
		next, d, err := Reconcile(initialState, s, Event{Now: clock, History: h})
		if err == nil || d.AllowCode || !reflect.DeepEqual(next, initialState) {
			t.Fatal("unserializable first observation accepted", next, d, err)
		}
		// Even a second event cannot collapse both fingerprints to Digest(nil)
		// and turn that previously observed code into documentation.
		h.Commits[0].Code = false
		next, d, err = Reconcile(next, s, Event{Now: clock.Add(time.Minute), History: h})
		if err == nil || d.AllowCode || !reflect.DeepEqual(next, initialState) {
			t.Fatal("invalid-time rewrite enabled admission", next, d, err)
		}
	}
}
