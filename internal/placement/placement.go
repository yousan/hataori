// Package placement ranks machine observations; it grants no filesystem or
// worker authority. Callers reserve the selected room in their transaction.
package placement

import (
	"cmp"
	"math"
	"slices"
	"time"
)

type Limits struct {
	MaxResident, MaxStarting             int
	MaxLoad                              float64
	MinFreeMemoryBytes, MinFreeDiskBytes int64
}

// Observation is a fresh, reconciled view of a configured installation. Zero
// timestamps and unknown metrics never describe an empty machine.
type Observation struct {
	Ready, Reconciled, Known, Installed, LoggedIn bool
	At, HealthAt                                  time.Time
	ValidFor                                      time.Duration
	Load                                          float64
	Memory, Disk                                  int64
}

type Candidate struct {
	Key                               string
	Configured, Allowed, AgentAllowed bool
	Limits                            Limits
	Observation                       Observation
	Resident, Starting                int
}

func (c Candidate) Refusal(now time.Time) string {
	o, l := c.Observation, c.Limits
	switch {
	case !c.Configured:
		return "machine is not configured"
	case !c.Allowed:
		return "machine is not permitted by the scope"
	case !c.AgentAllowed:
		return "agent is not permitted on the machine"
	case !o.Ready:
		return "machine is not ready"
	case !o.Reconciled:
		return "machine inventory is not reconciled"
	case !o.Known || o.ValidFor <= 0 || o.ValidFor > time.Minute || o.At.IsZero() || now.Before(o.At) || now.Sub(o.At) >= o.ValidFor:
		return "resource observation is unknown or stale"
	case math.IsNaN(o.Load) || math.IsInf(o.Load, 0) || o.Load < 0 || o.Memory <= 0 || o.Disk <= 0:
		return "resource observation is invalid"
	case o.HealthAt.IsZero() || now.Before(o.HealthAt) || now.Sub(o.HealthAt) >= o.ValidFor:
		return "agent health is unknown or stale"
	case !o.Installed:
		return "agent is not installed"
	case !o.LoggedIn:
		return "agent is not logged in"
	case c.Resident < 0 || c.Starting < 0:
		return "resident accounting is unknown"
	case l.MaxResident > 0 && c.Resident >= l.MaxResident:
		return "resident slots are full"
	case l.MaxStarting > 0 && c.Starting >= l.MaxStarting:
		return "starting slots are full"
	case l.MaxLoad > 0 && o.Load >= l.MaxLoad:
		return "normalized load limit reached"
	case o.Memory < l.MinFreeMemoryBytes:
		return "free memory is below the configured minimum"
	case o.Disk < l.MinFreeDiskBytes:
		return "free disk is below the configured minimum"
	}
	return ""
}

func (c Candidate) room() float64 {
	if c.Limits.MaxResident == 0 {
		return 1
	}
	return float64(c.Limits.MaxResident-c.Resident) / float64(c.Limits.MaxResident)
}

// Choose returns the best eligible key and every ineligibility reason. An
// explicit request checks that machine alone: it never falls back.
func Choose(now time.Time, candidates []Candidate, explicit string) (string, map[string]string) {
	reasons := map[string]string{}
	var eligible []Candidate
	for _, c := range candidates {
		if explicit != "" && c.Key != explicit {
			continue
		}
		if reason := c.Refusal(now); reason != "" {
			reasons[c.Key] = reason
		} else {
			eligible = append(eligible, c)
		}
	}
	if explicit != "" && len(eligible) == 0 && reasons[explicit] == "" {
		reasons[explicit] = "machine is not configured"
	}
	slices.SortFunc(eligible, func(a, b Candidate) int {
		if n := cmp.Compare(b.room(), a.room()); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Observation.Load, b.Observation.Load); n != 0 {
			return n
		}
		if n := cmp.Compare(b.Observation.Memory, a.Observation.Memory); n != 0 {
			return n
		}
		return cmp.Compare(a.Key, b.Key)
	})
	if len(eligible) == 0 {
		return "", reasons
	}
	return eligible[0].Key, reasons
}
