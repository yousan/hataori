package chat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// subagent is a subagent that has started and not ended.
type subagent struct {
	pos   uint64 // its start event
	name  string
	since time.Time
	tools int // tool calls started in it
}

// subagents follows e for the subagent lines and returns the lines it
// gives: one when a subagent starts, one when it ends, and one for each
// subagent still running when its turn ends. A subagent's own text is
// never posted.
func (r *reader) subagents(e control.Event) []Row {
	var d struct{ ID, Name, Parent string }
	json.Unmarshal(e.Data, &d)
	open := r.subs[e.Thread]
	line := func(part int, text string) Row {
		return Row{Event: e.Pos, Part: part, Conv: convOf(e.Thread), Message: surface.Message{Text: text, Quiet: true}}
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
		open[d.ID] = &subagent{pos: e.Pos, name: name, since: e.Time}
		return []Row{line(0, "Subagent "+name+" started.")}
	case "tool_started":
		if s := open[d.Parent]; d.Parent != "" && s != nil {
			s.tools++
		}
	case "subagent_ended":
		s := open[d.ID]
		if s == nil {
			return []Row{line(0, "Subagent "+d.ID+" ended.")}
		}
		delete(open, d.ID)
		return []Row{line(0, endLine(s, e.Time, ""))}
	case "turn_ended":
		var left []*subagent
		for _, s := range open {
			left = append(left, s)
		}
		sort.Slice(left, func(i, j int) bool { return left[i].pos < left[j].pos })
		var rows []Row
		for i, s := range left {
			rows = append(rows, line(10+i, endLine(s, e.Time, " with its turn")))
		}
		delete(r.subs, e.Thread)
		return rows
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
