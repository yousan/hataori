package placement

import (
	"math"
	"strings"
	"testing"
	"time"
)

func ready(now time.Time) Candidate {
	return Candidate{Key: "build", Configured: true, Allowed: true, AgentAllowed: true,
		Observation: Observation{Ready: true, Reconciled: true, Known: true, Installed: true, LoggedIn: true,
			At: now, HealthAt: now, ValidFor: 10 * time.Second, Load: .2, Memory: 1024, Disk: 2048}}
}

func TestEveryEligibilityGate(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name, reason string
		change       func(*Candidate)
	}{
		{"removed", "configured", func(c *Candidate) { c.Configured = false }},
		{"scope", "scope", func(c *Candidate) { c.Allowed = false }},
		{"agent", "permitted", func(c *Candidate) { c.AgentAllowed = false }},
		{"away", "ready", func(c *Candidate) { c.Observation.Ready = false }},
		{"inventory", "reconciled", func(c *Candidate) { c.Observation.Reconciled = false }},
		{"unknown", "unknown", func(c *Candidate) { c.Observation.Known = false }},
		{"stale", "stale", func(c *Candidate) { c.Observation.At = now.Add(-10 * time.Second) }},
		{"future", "stale", func(c *Candidate) { c.Observation.At = now.Add(time.Second) }},
		{"zero time", "stale", func(c *Candidate) { c.Observation.At = time.Time{} }},
		{"zero validity", "stale", func(c *Candidate) { c.Observation.ValidFor = 0 }},
		{"unbounded validity", "stale", func(c *Candidate) { c.Observation.ValidFor = 2 * time.Minute }},
		{"nan", "invalid", func(c *Candidate) { c.Observation.Load = math.NaN() }},
		{"infinite", "invalid", func(c *Candidate) { c.Observation.Load = math.Inf(1) }},
		{"negative load", "invalid", func(c *Candidate) { c.Observation.Load = -1 }},
		{"unknown memory", "invalid", func(c *Candidate) { c.Observation.Memory = 0 }},
		{"unknown disk", "invalid", func(c *Candidate) { c.Observation.Disk = 0 }},
		{"stale login", "health", func(c *Candidate) { c.Observation.HealthAt = now.Add(-10 * time.Second) }},
		{"unknown login", "health", func(c *Candidate) { c.Observation.HealthAt = time.Time{} }},
		{"missing", "installed", func(c *Candidate) { c.Observation.Installed = false }},
		{"logged out", "logged in", func(c *Candidate) { c.Observation.LoggedIn = false }},
		{"unknown slots", "accounting", func(c *Candidate) { c.Resident = -1 }},
		{"resident", "resident slots", func(c *Candidate) { c.Limits.MaxResident, c.Resident = 2, 2 }},
		{"starting", "starting slots", func(c *Candidate) { c.Limits.MaxStarting, c.Starting = 1, 1 }},
		{"load", "load limit", func(c *Candidate) { c.Limits.MaxLoad = .2 }},
		{"memory", "free memory", func(c *Candidate) { c.Limits.MinFreeMemoryBytes = 1025 }},
		{"disk", "free disk", func(c *Candidate) { c.Limits.MinFreeDiskBytes = 2049 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ready(now)
			tc.change(&c)
			if got := c.Refusal(now); !strings.Contains(got, tc.reason) {
				t.Fatalf("refusal %q, want %q", got, tc.reason)
			}
			other := ready(now)
			other.Key = "other"
			if key, _ := Choose(now, []Candidate{c, other}, c.Key); key != "" {
				t.Fatalf("explicit ineligible choice fell back to %q", key)
			}
		})
	}
	if got := ready(now).Refusal(now); got != "" {
		t.Fatal(got)
	}
}

func TestDeterministicRoomOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, test := range []string{"fraction", "load", "memory", "key", "unlimited"} {
		t.Run(test, func(t *testing.T) {
			a, b := ready(now), ready(now)
			a.Key, b.Key = "a", "b"
			switch test {
			case "fraction":
				a.Limits.MaxResident, a.Resident = 10, 5
				b.Limits.MaxResident, b.Resident = 100, 90
				a.Observation.Load = .9
			case "load":
				b.Observation.Load = .3
			case "memory":
				b.Observation.Memory = 100
			case "unlimited":
				b.Limits.MaxResident, b.Resident = 100, 1
				a.Observation.Load = .9
			}
			for _, cs := range [][]Candidate{{b, a}, {a, b}} {
				if key, _ := Choose(now, cs, ""); key != "a" {
					t.Fatalf("chose %q", key)
				}
			}
		})
	}
	if key, why := Choose(now, []Candidate{ready(now)}, "missing"); key != "" || why["missing"] == "" {
		t.Fatal(key, why)
	}
}
