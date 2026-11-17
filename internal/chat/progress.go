package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// The progress line is one message per quiet stretch of a turn: posted
// after QuietAfter without visible output, edited at most every EditEvery,
// deleted when output appears or the turn ends. Its rows are keyed by the
// event that began the stretch, so a front end that reads the stretch
// again (after a restart) writes the same rows and uses the same line.
const (
	linePart   = 1 << 30 // (anchor, linePart) posts the line, (anchor, linePart+n) is its n-th edit; far above other parts
	deletePart = -1      // (event, -1) deletes it, before what the event shows
)

// quiet is a thread's open turn, for its progress line.
type quiet struct {
	anchor uint64    // the event that began the stretch
	since  time.Time // its time
	tools  []tool    // running, in the order they started
	line   bool      // the line's post is written
	edit   int64     // the last edit written (0: the post)
}

type tool struct{ id, name string }

func (f *FrontEnd) quietAfter() time.Duration {
	if f.QuietAfter > 0 {
		return f.QuietAfter
	}
	return 90 * time.Second
}

func (f *FrontEnd) editEvery() time.Duration {
	if f.EditEvery > 0 {
		return f.EditEvery
	}
	return 15 * time.Second
}

func (f *FrontEnd) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// visible reports whether the front end shows e as output in the thread.
func visible(e control.Event) bool {
	var d eventData
	json.Unmarshal(e.Data, &d)
	switch e.Kind {
	case "text":
		return strings.TrimSpace(d.Text) != ""
	case "request":
		return !d.Approval
	case "subagent_started", "subagent_ended", "file_out":
		return true // a line or a file in the thread
	}
	return false
}

// track follows e for the progress line and returns the rows that delete
// the line when e ends the stretch.
func (r *reader) track(e control.Event) []Row {
	var d struct{ ID, Tool string }
	json.Unmarshal(e.Data, &d)
	q := r.quiet[e.Thread]
	switch {
	case e.Kind == "turn_started":
		r.quiet[e.Thread] = &quiet{anchor: e.Pos, since: e.Time}
		return nil
	case q == nil:
		return nil
	case e.Kind == "tool_started":
		if !slices.ContainsFunc(q.tools, func(t tool) bool { return t.id == d.ID }) {
			q.tools = append(q.tools, tool{d.ID, d.Tool})
		}
		return nil
	case e.Kind == "tool_ended":
		q.tools = slices.DeleteFunc(q.tools, func(t tool) bool { return t.id == d.ID })
		return nil
	case e.Kind != "turn_ended" && !visible(e):
		return nil
	}
	var rows []Row
	// The line was due, so it may have been posted, also by a reader
	// before a restart; a delete of a line never posted is dropped.
	if q.line || e.Time.Sub(q.since) >= r.quietAfter {
		rows = append(rows, Row{Event: e.Pos, Part: deletePart, Conv: convOf(e.Thread), Decoration: true,
			Op: Delete, Target: &RowKey{Event: q.anchor, Part: linePart}})
		// A line is posted only once the delete before it is done, so
		// while one is waiting, this stretch had no line to delete.
		if g := r.gone[e.Thread]; g == nil || r.done(*g) {
			r.gone[e.Thread] = &RowKey{Event: e.Pos, Part: deletePart}
		}
	}
	if e.Kind == "turn_ended" {
		delete(r.quiet, e.Thread)
	} else {
		r.quiet[e.Thread] = &quiet{anchor: e.Pos, since: e.Time, tools: q.tools}
	}
	return rows
}

// done reports whether row k is sent or given up.
func (r *reader) done(k RowKey) bool {
	state, _, err := r.outbox.state(r.ctx, k)
	return err == nil && state != "pending"
}

// tick returns the rows that post or edit the progress lines due at now.
func (f *FrontEnd) tick(ctx context.Context, r *reader, now time.Time) []Row {
	var rows []Row
	for id, q := range r.quiet {
		el := now.Sub(q.since)
		if el < f.quietAfter() {
			continue
		}
		n := int64((el - f.quietAfter()) / f.editEvery())
		m := surface.Message{Text: progressText(el, q.tools), Quiet: true}
		if !q.line {
			if g := r.gone[id]; g != nil && !r.done(*g) {
				continue // the line before is still shown
			}
			q.line = true
			if last, _, err := f.Outbox.lineChanges(ctx, q.anchor); err != nil || last.IsZero() {
				// Not posted yet (also not before a restart): post it.
				rows = append(rows, Row{Event: q.anchor, Part: linePart, Conv: convOf(id), Message: m, Decoration: true})
				continue
			}
		}
		if n <= q.edit {
			continue
		}
		// One change at a time, at most every EditEvery after the last one
		// sent (also by a reader before a restart). A dropped change
		// holds nothing back.
		last, pending, err := f.Outbox.lineChanges(ctx, q.anchor)
		if err != nil || pending || last.IsZero() || now.Sub(last) < f.editEvery() {
			continue
		}
		q.edit = n
		rows = append(rows, Row{Event: q.anchor, Part: linePart + int(n), Conv: convOf(id), Message: m, Decoration: true,
			Op: Edit, Target: &RowKey{Event: q.anchor, Part: linePart}})
	}
	return rows
}

func progressText(el time.Duration, tools []tool) string {
	el = el.Truncate(time.Second)
	s := "Still working: " + duration(el) + " without output."
	if len(tools) > 0 {
		s += " Running " + tools[len(tools)-1].name + "."
	}
	return s
}

func convOf(id control.ThreadID) surface.Conversation {
	return surface.Conversation{Channel: id.Scope, Thread: id.Key}
}
