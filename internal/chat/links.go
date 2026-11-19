package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// linkConvs returns the conversations of t's links of this front end, the
// first link first.
func (f *FrontEnd) linkConvs(t control.ThreadInfo) []surface.Conversation {
	var out []surface.Conversation
	for _, l := range t.Links {
		if l.Client == f.Name {
			out = append(out, surface.Conversation{Channel: l.Channel, Thread: l.Thread})
		}
	}
	return out
}

// convOf returns the first conversation thread id is shown in by this
// front end, through its links; ok is false when it has none here.
func (f *FrontEnd) convOf(ctx context.Context, id control.ThreadID) (surface.Conversation, bool, error) {
	convs, err := f.convsOf(ctx, id)
	if err != nil || len(convs) == 0 {
		return surface.Conversation{}, false, err
	}
	return convs[0], true, nil
}

// convsOf returns every conversation thread id is shown in by this front
// end, one per link, the first link first; none when it has no link here,
// or its scope is not one this front end shows. It reads the links as they
// are now, so a link made since, by any client, is seen at once.
func (f *FrontEnd) convsOf(ctx context.Context, id control.ThreadID) ([]surface.Conversation, error) {
	scopes := f.scopeList()
	if len(scopes) == 0 {
		return nil, nil
	}
	q := `SELECT l.channel, l.thread FROM links l JOIN threads t ON t.id = l.thread_id
		WHERE l.thread_id = ? AND l.client = ? AND t.scope IN (?` + strings.Repeat(", ?", len(scopes)-1) + `) ORDER BY l.id`
	args := []any{int64(id), f.Name}
	for _, sc := range scopes {
		args = append(args, sc)
	}
	rows, err := f.Outbox.Store.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []surface.Conversation
	for rows.Next() {
		var c surface.Conversation
		if err := rows.Scan(&c.Channel, &c.Thread); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// everyLink returns rows, each written for one of convs, and a copy of
// each for every other conversation in convs. Which messages each link gets
// (design, section 5.2): every link gets the agent's output, its questions,
// files, failures, progress and subagent lines. A reply stays in the
// conversation of the message it replies to: the reply of a final answer
// to the message that started the turn, or to the prompt a trigger posted,
// is in the link the turn's input came through; the copies are posted
// without it.
func everyLink(rows []Row, convs []surface.Conversation) []Row {
	if len(convs) < 2 {
		return rows
	}
	out := append([]Row(nil), rows...)
	for _, r := range rows {
		if !slices.Contains(convs, r.Conv) {
			continue
		}
		from := r.Conv
		for _, conv := range convs {
			if conv == from {
				continue
			}
			c := r
			c.Conv = conv
			c.ReplyToRow = nil
			if c.Message.ReplyTo != nil && c.Message.ReplyTo.Conv != conv {
				c.Message.ReplyTo = nil
			}
			out = append(out, c)
		}
	}
	return out
}

// mirrorRows returns the lines that show an input or answer event of a
// thread shown in convs in every one of them but the link it came through,
// as "<person>: <text>" (design, section 5.2). Only what a person wrote in
// a link is shown: an input that came through none, such as the
// supervisor's, is not.
func (f *FrontEnd) mirrorRows(e control.Event, convs []surface.Conversation) []Row {
	if e.Kind != "input" && e.Kind != "answer" {
		return nil
	}
	var d struct {
		Text      string           `json:"text"`
		Principal string           `json:"principal"`
		Link      *control.LinkRef `json:"link"`
	}
	if json.Unmarshal(e.Data, &d) != nil || strings.TrimSpace(d.Text) == "" || d.Link == nil {
		return nil
	}
	who := d.Principal
	if who == "" {
		who = "someone"
	}
	var rows []Row
	for _, conv := range convs {
		if l := d.Link; l != nil && l.Client == f.Name && l.Channel == conv.Channel && l.Thread == conv.Thread {
			continue // where it was written
		}
		rows = append(rows, Row{Event: e.Pos, Part: 0, Conv: conv, Message: surface.Message{Text: who + ": " + d.Text, Quiet: true}})
	}
	return rows
}

// threadIn returns the thread linked to conv on this front end, as api sees
// it; ok is false when conv links to none.
func (f *FrontEnd) threadIn(ctx context.Context, api control.API, conv surface.Conversation) (control.ThreadInfo, bool, error) {
	ts, err := api.Threads(ctx, control.Filter{Link: &control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread}})
	if err != nil || len(ts) == 0 {
		return control.ThreadInfo{}, false, err
	}
	return ts[0], true, nil
}
