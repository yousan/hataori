package codex

import (
	"encoding/json"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// The app-server reports after each model request during a turn and never
// while idle; Every is the age after which the dashboard shows a value as old.
const limitsEvery = 5 * time.Minute

// tokenCounts holds the cumulative token counts from the app-server.
type tokenCounts struct {
	Input  int64 `json:"inputTokens"`
	Cached int64 `json:"cachedInputTokens"`
	Write  int64 `json:"cacheWriteInputTokens"`
	Output int64 `json:"outputTokens"`
	Total  int64 `json:"totalTokens"`
}

// minus returns the difference between two cumulative counts.
func (t tokenCounts) minus(b tokenCounts) tokenCounts {
	return tokenCounts{t.Input - b.Input, t.Cached - b.Cached, t.Write - b.Write, t.Output - b.Output, t.Total - b.Total}
}

// usageState tracks turn baselines and pending limit reports.
type usageState struct {
	model                   string
	lastTotal, baseline     tokenCounts
	seenTotal, haveBaseline bool
	at                      time.Time
	pendingLimits           []*agent.LimitReport
}

// beginUsage records the previous total as the new turn baseline.
func (c *codec) beginUsage() { c.baseline = c.lastTotal; c.haveBaseline = c.seenTotal }

// usage reports the session turn's cumulative usage and context size.
func (c *codec) usage(a *agent.Actions, raw json.RawMessage) {
	var p struct {
		TokenUsage struct {
			Total, Last        tokenCounts
			ModelContextWindow int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	u := p.TokenUsage
	if !c.haveBaseline {
		c.baseline = u.Total.minus(u.Last)
		c.haveBaseline = true
	}
	c.lastTotal = u.Total
	c.seenTotal = true
	if !c.running {
		return
	}
	t := u.Total.minus(c.baseline)
	// OpenAI's input tokens include cached input tokens.
	n := &agent.UsageCount{
		Model:       c.model,
		Input:       t.Input - t.Cached,
		CacheRead:   t.Cached,
		CacheWrite:  t.Write,
		Output:      t.Output,
		ContextUsed: u.Last.Total,
		ContextSize: u.ModelContextWindow,
	}
	c.emit(a, agent.Event{Kind: agent.Usage, Usage: n})
}

// readLimits requests the latest account limits.
func (c *codec) readLimits(a *agent.Actions) { c.write(a, RateLimitsRead, map[string]any{}, true) }

// timestamp records the newest line time and releases pending reports.
func (c *codec) timestamp(a *agent.Actions, ms *int64) {
	if ms == nil {
		return
	}
	t := time.UnixMilli(*ms)
	if t.After(c.at) {
		c.at = t
	}
	for _, r := range c.pendingLimits {
		r.At = c.at
		c.emit(a, agent.Event{Kind: agent.Limits, Limits: r})
	}
	c.pendingLimits = nil
}

// limitResponse recognizes and reports a rate-limits read response.
func (c *codec) limitResponse(a *agent.Actions, method string, raw json.RawMessage, line []byte) bool {
	var shape map[string]json.RawMessage
	_ = json.Unmarshal(raw, &shape)
	_, found := shape["rateLimits"]
	if method != RateLimitsRead && !(method == "" && found) {
		return false
	}
	r, err := DecodeRateLimits(raw, c.at)
	if err != nil {
		c.unknown(a, line)
		return true
	}
	r.Every = limitsEvery
	if c.at.IsZero() {
		c.pendingLimits = append(c.pendingLimits, r)
	} else {
		c.emit(a, agent.Event{Kind: agent.Limits, Limits: r})
	}
	return true
}
