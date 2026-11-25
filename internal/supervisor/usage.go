package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// usageWindows starts a wake's line of an agent's usage windows; the
// agent's name follows.
const usageWindows = "Usage windows of"

// usedWithin is how long ago a thread of the scope used an agent whose
// usage windows a wake still shows.
const usedWithin = 24 * time.Hour

// agentsUsed returns the agents that ran a turn of a thread of the scope,
// the supervisor's own included, since then.
func agentsUsed(ctx context.Context, c *core.Core, scope string, since time.Time) ([]string, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT DISTINCT s.agent FROM threads h
		JOIN sessions s ON s.thread_id = h.id JOIN turns t ON t.session_id = s.id
		WHERE h.scope = ? AND (t.ended_at IS NULL OR t.ended_at > ?)`, scope, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("supervisor: the agents used: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("supervisor: the agents used: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UsageWindows is the part of a wake that says how much of each agent's
// account is used (#798): a line for each of the agents used that reported
// its limits, with each window as the agent names it; "" when none did. A
// report older than the agent's own interval says how old it is at now.
func UsageWindows(limits []control.AgentLimits, used []string, now time.Time) string {
	var b strings.Builder
	for _, l := range limits {
		if len(l.Windows) == 0 || !slices.Contains(used, l.Agent) {
			continue
		}
		var ws []string
		for _, w := range l.Windows {
			s := fmt.Sprintf("%s %d%% used", strings.Join(strings.Fields(w.Name), " "), w.Used)
			switch {
			case w.Resets == nil:
			case w.Resets.After(now):
				s += ", resets " + dueTime(w.Resets.In(now.Location()))
			default: // what is used is from before it
				s += ", reset " + dueTime(w.Resets.In(now.Location()))
			}
			ws = append(ws, s)
		}
		if b.Len() == 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s %s: %s.", usageWindows, l.Agent, strings.Join(ws, "; "))
		if age := now.Sub(l.At); age > time.Duration(l.Every)*time.Second {
			fmt.Fprintf(&b, " Reported %s ago.", span(age))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
