package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// subagent is a subagent that has started and not ended.
type subagent struct {
	pos   uint64 // its start event
	of    int64  // the session it runs in
	name  string
	since time.Time
	tools int // tool calls started in it
}

// subagents follows e for the subagent lines and returns the lines it
// gives: one when a subagent starts and one when it ends. A subagent whose
// end is not reported is closed when a turn ends that does not say it still
// runs, or when its session's agent is stopped. A turn that ends on a
// question closes none: the agent goes on once it is answered. A subagent's
// own text is never posted.
func (r *reader) subagents(e control.Event) []Row {
	var d struct {
		Session          int64
		ID, Name, Parent string
		Outcome          string
		Subagents        []string // those that still run
		To               control.State
	}
	json.Unmarshal(e.Data, &d)
	open := r.subs[e.Thread]
	line := func(part int, text string) Row {
		return Row{Event: e.Pos, Part: part, Conv: r.conv(e.Thread), Message: surface.Message{Text: text, Quiet: true}}
	}
	// closed ends the session's subagents that e does not say still run.
	closed := func(how string) []Row {
		var left []*subagent
		for id, s := range open {
			if s.of == d.Session && !slices.Contains(d.Subagents, id) {
				left = append(left, s)
				delete(open, id)
			}
		}
		sort.Slice(left, func(i, j int) bool { return left[i].pos < left[j].pos })
		var rows []Row
		for i, s := range left {
			rows = append(rows, line(10+i, endLine(s, e.Time, how)))
		}
		return rows
	}
	switch e.Kind {
	case "subagent_started":
		if open == nil {
			open = map[string]*subagent{}
			r.subs[e.Thread] = open
		}
		name := d.Name
		if name == "" {
			name = d.ID
		}
		open[d.ID] = &subagent{pos: e.Pos, of: d.Session, name: name, since: e.Time}
		return []Row{line(0, "Subagent "+name+" started.")}
	case "tool_started":
		if s := open[d.Parent]; d.Parent != "" && s != nil {
			s.tools++
		}
	case "subagent_ended":
		s := open[d.ID]
		if s == nil {
			// Closed already, or never started: its one end
			// line is posted, or cannot say how long it ran.
			return nil
		}
		delete(open, d.ID)
		return []Row{line(0, endLine(s, e.Time, ""))}
	case "turn_ended":
		if d.Outcome != "blocked" {
			return closed(" with its turn")
		}
	case "state":
		switch d.To {
		case control.Asleep, control.Closed, control.Removed, control.Failed:
			return closed(" with its agent")
		}
	}
	return nil
}

func endLine(s *subagent, at time.Time, how string) string {
	calls := strconv.Itoa(s.tools) + " tool calls"
	if s.tools == 1 {
		calls = "1 tool call"
	}
	return fmt.Sprintf("Subagent %s ended%s after %s, %s.", s.name, how, duration(at.Sub(s.since)), calls)
}

// duration is d as "1m 05s".
func duration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}
