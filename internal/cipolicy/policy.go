// Package cipolicy plans integration coverage. It performs no dispatch, merge,
// cancellation, deployment, or release authorization.
package cipolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const PolicyPath = ".github/ci-policy.json"

// Trust must come from the trusted coordinator's approved control store, never
// candidate files, labels, workflow inputs, or artifacts. Digests pin policy
// bytes as well as immutable Git revisions. This package cannot grant approval.
type Trust struct {
	Revision string
	Approved map[string]string // full Git revision -> policy SHA-256
}

type Source interface {
	PolicyAt(revision string) ([]byte, error)
}

// GitSource reads committed objects, never the working tree. Repo is selected
// by the trusted caller; revisions are validated before invoking Git.
type GitSource struct{ Repo string }

func (s GitSource) PolicyAt(revision string) ([]byte, error) {
	if !revisionOK(revision) {
		return nil, fmt.Errorf("invalid control revision")
	}
	kind, err := exec.Command("git", "-C", s.Repo, "cat-file", "-t", revision).Output()
	if err != nil || string(kind) != "commit\n" {
		return nil, fmt.Errorf("control revision is not an available commit")
	}
	return exec.Command("git", "-C", s.Repo, "show", revision+":"+PolicyPath).Output()
}

type Limits struct {
	MaxChanges      int   `json:"max_changes"`
	MaxAgeSeconds   int64 `json:"max_age_seconds"`
	LeadTimeSeconds int64 `json:"lead_time_seconds"`
}

type Policy struct {
	Version          int       `json:"version"`
	Profile          string    `json:"profile"`
	Owner            string    `json:"owner"`
	Reason           string    `json:"reason"`
	StartsAt         time.Time `json:"starts_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	RollbackRevision string    `json:"rollback_revision"`
	Limits           Limits    `json:"limits"`
}

// Selection is sealed: only Resolve can establish a valid approved policy.
// Copying a report or supplying a candidate Policy cannot enable admission.
type Selection struct {
	policy   Policy
	revision string
	digest   string
	invalid  string
}

type Report struct {
	Profile, Owner, Reason, Revision, Digest, RollbackRevision string
	StartsAt, ExpiresAt                                        time.Time
	Limits                                                     Limits
	FullOnly                                                   bool
	BlockedReason                                              string
}

func (s Selection) Report(now time.Time) Report {
	r := Report{Profile: s.policy.Profile, Owner: s.policy.Owner, Reason: s.policy.Reason,
		Revision: s.revision, Digest: s.digest, RollbackRevision: s.policy.RollbackRevision,
		StartsAt: s.policy.StartsAt, ExpiresAt: s.policy.ExpiresAt, Limits: s.policy.Limits}
	r.BlockedReason = s.invalid
	if r.BlockedReason == "" {
		switch {
		case s.revision == "":
			r.BlockedReason = "no approved control"
		case now.Before(s.policy.StartsAt):
			r.BlockedReason = "policy has not started"
		case !now.Before(s.policy.ExpiresAt):
			r.BlockedReason = "policy expired"
		}
	}
	if r.BlockedReason != "" {
		r.Profile, r.FullOnly = "closed", true
	}
	return r
}

func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Resolve accepts no candidate profile/label input. Invalid, unknown, missing,
// unapproved or expired controls fail closed; they never enable partial gates.
// A rollback selects another approved immutable revision through the same path.
func Resolve(source Source, trust Trust, now time.Time) Selection {
	s := Selection{revision: trust.Revision}
	fail := func(reason string) Selection { s.invalid = reason; return s }
	expected, ok := trust.Approved[trust.Revision]
	if source == nil || !revisionOK(trust.Revision) || !ok || !digestOK(expected) {
		return fail("unapproved control revision")
	}
	b, err := source.PolicyAt(trust.Revision)
	if err != nil {
		return fail("approved policy unavailable")
	}
	s.digest = Digest(b)
	if s.digest != expected {
		return fail("approved policy digest mismatch")
	}
	if err := uniqueKeys(b); err != nil {
		return fail("invalid policy JSON: " + err.Error())
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&s.policy); err != nil {
		return fail("invalid policy schema: " + err.Error())
	}
	p := s.policy
	if p.Version != 1 || (p.Profile != "throughput" && p.Profile != "stable") {
		return fail("unknown policy version/profile")
	}
	if strings.TrimSpace(p.Owner) == "" || strings.TrimSpace(p.Reason) == "" ||
		p.StartsAt.IsZero() || !p.ExpiresAt.After(p.StartsAt) {
		return fail("policy needs owner, reason and bounded validity")
	}
	if !revisionOK(p.RollbackRevision) || p.RollbackRevision == trust.Revision || !digestOK(trust.Approved[p.RollbackRevision]) {
		return fail("rollback revision is not separately approved")
	}
	// Bound arithmetic before converting seconds to time.Duration.
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	l := p.Limits
	if l.MaxChanges <= 0 || l.MaxAgeSeconds <= 0 || l.MaxAgeSeconds > maxSeconds ||
		l.LeadTimeSeconds < 0 || l.LeadTimeSeconds >= l.MaxAgeSeconds {
		return fail("invalid coverage limits")
	}
	if r := s.Report(now); r.FullOnly {
		return fail(r.BlockedReason)
	}
	return s
}

func revisionOK(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func digestOK(s string) bool { return len(s) == 64 && revisionOK(s) }

// Reject duplicate keys and trailing JSON, including inside nested objects.
func uniqueKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var read func() error
	read = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			seen := map[string]bool{}
			for d.More() {
				if delim == '{' {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] || !canonicalKey(name) {
						return fmt.Errorf("duplicate/invalid key")
					}
					seen[name] = true
				}
				if err := read(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := read(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

// encoding/json accepts case-insensitive aliases. Policy control accepts only
// these exact schema spellings, including when an alias appears only once.
func canonicalKey(name string) bool {
	switch name {
	case "version", "profile", "owner", "reason", "starts_at", "expires_at", "rollback_revision", "limits", "max_changes", "max_age_seconds", "lead_time_seconds":
		return true
	}
	return false
}
