// Package codex holds what hataori reads from Codex's app-server. The
// adapter itself comes with the Codex phase (design, section 8); this file
// is the part of it that reads the remaining usage
// (docs/notes/agent-usage-limits.md).
package codex

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// RateLimitsRead is the method that reads the account's remaining usage.
// RateLimitsUpdated is the notification that it changed: a hint to read
// again, since a read made after it can be newer.
const (
	RateLimitsRead    = "account/rateLimits/read"
	RateLimitsUpdated = "account/rateLimits/updated"
)

type window struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int64   `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

// DecodeRateLimits reads the result of RateLimitsRead, answered at at: the
// windows primary and secondary of rateLimits, in that order, leaving out
// one that is null. Every is left for the adapter, which decides how often
// it reads.
func DecodeRateLimits(result json.RawMessage, at time.Time) (*agent.LimitReport, error) {
	var r struct {
		RateLimits *struct {
			Primary   *window `json:"primary"`
			Secondary *window `json:"secondary"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return nil, fmt.Errorf("codex: %s: %w", RateLimitsRead, err)
	}
	if r.RateLimits == nil {
		return nil, fmt.Errorf("codex: %s: no rateLimits", RateLimitsRead)
	}
	rep := &agent.LimitReport{At: at, Windows: []agent.LimitWindow{}}
	for _, w := range []struct {
		name string
		w    *window
	}{{"primary", r.RateLimits.Primary}, {"secondary", r.RateLimits.Secondary}} {
		if w.w == nil || w.w.UsedPercent == nil {
			continue
		}
		lw := agent.LimitWindow{Name: w.name, Used: int(*w.w.UsedPercent + 0.5)}
		if w.w.WindowDurationMins != nil {
			lw.Minutes = *w.w.WindowDurationMins
		}
		if w.w.ResetsAt != nil {
			lw.Resets = time.Unix(*w.w.ResetsAt, 0)
		}
		rep.Windows = append(rep.Windows, lw)
	}
	return rep, nil
}
