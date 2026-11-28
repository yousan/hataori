package cipolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var clock = time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type policies map[string][]byte

func (p policies) PolicyAt(revision string) ([]byte, error) {
	b, ok := p[revision]
	if !ok {
		return nil, fmt.Errorf("missing revision")
	}
	return b, nil
}

func validPolicy() Policy {
	return Policy{Version: 1, Profile: "throughput", Owner: "CI maintainer", Reason: "bounded integration experiment",
		StartsAt: clock.Add(-time.Hour), ExpiresAt: clock.Add(7 * 24 * time.Hour), RollbackRevision: sha(2),
		Limits: Limits{MaxChanges: 10, MaxAgeSeconds: 6 * 60 * 60, LeadTimeSeconds: 30 * 60}}
}

func selected(t *testing.T, p Policy) Selection {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := Resolve(policies{sha(1): b}, Trust{Revision: sha(1), Approved: map[string]string{sha(1): Digest(b), sha(2): Digest([]byte("rollback"))}}, clock)
	if s.Report(clock).FullOnly {
		t.Fatal(s.Report(clock))
	}
	return s
}

func TestInvalidControlsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Policy)
	}{
		{"version", func(p *Policy) { p.Version = 2 }},
		{"profile", func(p *Policy) { p.Profile = "candidate-cheap" }},
		{"owner", func(p *Policy) { p.Owner = " " }},
		{"reason", func(p *Policy) { p.Reason = "" }},
		{"future", func(p *Policy) { p.StartsAt = clock.Add(time.Second) }},
		{"expired", func(p *Policy) { p.ExpiresAt = clock }},
		{"unbounded", func(p *Policy) { p.ExpiresAt = time.Time{} }},
		{"rollback", func(p *Policy) { p.RollbackRevision = sha(99) }},
		{"unknown rollback route", func(p *Policy) { p.RollbackRoute = "candidate-cheap" }},
		{"contradictory rollback route", func(p *Policy) { p.RollbackRoute = "main-full" }},
		{"revision route missing target", func(p *Policy) { p.RollbackRoute = "revision"; p.RollbackRevision = "" }},
		{"self rollback", func(p *Policy) { p.RollbackRevision = sha(1) }},
		{"count", func(p *Policy) { p.Limits.MaxChanges = 0 }},
		{"age", func(p *Policy) { p.Limits.MaxAgeSeconds = -1 }},
		{"overflow", func(p *Policy) { p.Limits.MaxAgeSeconds = 1 << 62 }},
		{"lead", func(p *Policy) { p.Limits.LeadTimeSeconds = p.Limits.MaxAgeSeconds }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validPolicy()
			tc.edit(&p)
			b, _ := json.Marshal(p)
			s := Resolve(policies{sha(1): b}, Trust{Revision: sha(1), Approved: map[string]string{sha(1): Digest(b), sha(2): Digest(nil)}}, clock)
			if !s.Report(clock).FullOnly {
				t.Fatal("invalid control enabled partial admission", s.Report(clock))
			}
		})
	}
	valid, _ := json.Marshal(validPolicy())
	for _, b := range [][]byte{[]byte(`{`), append(append([]byte{}, valid...), []byte(` {}`)...), []byte(strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(valid), `"version":1`, `"version":1,"unknown":true`, 1)), []byte(strings.Replace(string(valid), `"max_changes":10`, `"max_changes":10,"max_changes":100`, 1))} {
		s := Resolve(policies{sha(1): b}, Trust{Revision: sha(1), Approved: map[string]string{sha(1): Digest(b), sha(2): Digest(nil)}}, clock)
		if !s.Report(clock).FullOnly {
			t.Fatal("malformed/unknown JSON accepted", string(b))
		}
	}
	for _, trust := range []Trust{{}, {Revision: sha(3), Approved: map[string]string{sha(1): Digest(valid)}}, {Revision: sha(1), Approved: map[string]string{sha(1): Digest(nil)}}} {
		if !Resolve(policies{sha(1): valid}, trust, clock).Report(clock).FullOnly {
			t.Fatal("unapproved/mismatched control accepted")
		}
	}
	if !Resolve(nil, Trust{}, clock).Report(clock).FullOnly {
		t.Fatal("nil source enabled admission")
	}
	if !(Selection{}).Report(clock).FullOnly {
		t.Fatal("zero selection enabled admission")
	}
}

func TestExpiryIsRecheckedAndMetadataIsPinned(t *testing.T) {
	s := selected(t, validPolicy())
	r := s.Report(clock)
	if r.Profile != "throughput" || r.Owner != "CI maintainer" || r.Reason == "" || r.RollbackRevision != sha(2) || r.Revision != sha(1) || r.Digest == "" || r.ExpiresAt.IsZero() {
		t.Fatal(r)
	}
	if !s.Report(r.ExpiresAt).FullOnly {
		t.Fatal("selection remained usable at expiry")
	}
	if !s.Report(r.StartsAt.Add(-time.Nanosecond)).FullOnly {
		t.Fatal("selection usable before start")
	}
}

func TestNoncanonicalAliasesCannotOverrideApprovedLimits(t *testing.T) {
	valid, _ := json.Marshal(validPolicy())
	for _, pair := range [][2]string{
		{`"max_changes":10`, `"max_changes":10,"MAX_CHANGES":100`},
		{`"profile":"throughput"`, `"profile":"stable","Profile":"throughput"`},
		{`"limits":`, `"Limits":`},
		{`"version":1`, `"Version":1`},
		{`"max_changes":10`, `"max_changeſ":100`},
	} {
		b := []byte(strings.Replace(string(valid), pair[0], pair[1], 1))
		s := Resolve(policies{sha(1): b}, Trust{Revision: sha(1), Approved: map[string]string{sha(1): Digest(b), sha(2): Digest(nil)}}, clock)
		if !s.Report(clock).FullOnly {
			t.Fatal("noncanonical approved JSON enabled partial admission", string(b))
		}
	}
}

func TestGitControlIgnoresCandidateAndSupportsApprovedRollback(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "Policy test")
	git("config", "user.email", "policy@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "seed")
	root := git("rev-parse", "HEAD")
	write := func(p Policy) []byte {
		t.Helper()
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, ".github"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, PolicyPath), b, 0600); err != nil {
			t.Fatal(err)
		}
		return b
	}
	p := validPolicy()
	p.Profile = "stable"
	p.RollbackRevision = ""
	p.RollbackRoute = "main-full"
	p.Limits.MaxChanges = 2
	stableBytes := write(p)
	git("add", ".")
	git("commit", "-qm", "approved stable")
	stable := git("rev-parse", "HEAD")
	p = validPolicy()
	p.RollbackRevision = stable
	throughputBytes := write(p)
	git("add", ".")
	git("commit", "-qm", "approved throughput")
	approved := git("rev-parse", "HEAD")
	p.Limits.MaxChanges = 999999
	p.ExpiresAt = clock.Add(1000 * 24 * time.Hour)
	write(p)
	git("add", ".")
	git("commit", "-qm", "candidate tries cheaper policy")
	candidate := git("rev-parse", "HEAD")
	// A candidate's changed policy, current HEAD and any PR labels are outside
	// Resolve's authority inputs. The trusted anchor alone selects control.
	trust := Trust{Revision: approved, Approved: map[string]string{approved: Digest(throughputBytes), stable: Digest(stableBytes)}}
	s := Resolve(GitSource{repo}, trust, clock)
	if r := s.Report(clock); r.FullOnly || r.Limits.MaxChanges != 10 || r.Revision != approved || r.RollbackRevision != stable {
		t.Fatal(r)
	}
	trust.Revision = candidate
	if !Resolve(GitSource{repo}, trust, clock).Report(clock).FullOnly {
		t.Fatal("candidate elected its own policy")
	}
	trust.Revision = stable
	if r := Resolve(GitSource{repo}, trust, clock).Report(clock); r.FullOnly || r.Profile != "stable" || r.Limits.MaxChanges != 2 {
		t.Fatal("approved rollback failed", r)
	}
	trust.Revision = root
	if !Resolve(GitSource{repo}, trust, clock).Report(clock).FullOnly {
		t.Fatal("unapproved seed without policy must fail closed")
	}
	if _, err := (GitSource{repo}).PolicyAt("HEAD"); err == nil {
		t.Fatal("mutable revision accepted")
	}
	if _, err := (GitSource{repo}).PolicyAt(git("rev-parse", approved+"^{tree}")); err == nil {
		t.Fatal("noncommit control accepted")
	}
}
