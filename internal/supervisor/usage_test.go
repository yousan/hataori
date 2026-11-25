package supervisor

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// A window whose reset is over says so, one without a reset has none, and
// an agent without windows or that is not used has no line.
func TestUsageWindows(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	before, after := now.Add(-time.Minute), now.Add(time.Hour)
	ls := []control.AgentLimits{
		{Agent: "a", At: now.Add(-5 * time.Minute), Every: 300, Windows: []control.LimitWindow{{Name: "x", Used: 100, Resets: &before}, {Name: "y\nz", Used: 0, Resets: &after}}},
		{Agent: "b", At: now.Add(-5*time.Minute - time.Second), Every: 300, Windows: []control.LimitWindow{{Name: "x", Used: 3}}},
		{Agent: "c", At: now, Every: 300},
		{Agent: "d", At: now, Every: 300, Windows: []control.LimitWindow{{Name: "x", Used: 3}}},
	}
	want := "\nUsage windows of a: x 100% used, reset 2026-01-02 11:59 UTC; y z 0% used, resets 2026-01-02 13:00 UTC.\n" +
		"Usage windows of b: x 3% used. Reported 5 minutes ago.\n"
	if got := UsageWindows(ls, []string{"c", "b", "a"}, now); got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if got := UsageWindows(ls, nil, now); got != "" {
		t.Errorf("no agent used: %q", got)
	}
}
