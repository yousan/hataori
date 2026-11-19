package chat

import (
	"context"
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

// everyLink returns rows, written for the first conversation of thread id
// (convs[0]), and a copy of each for every other conversation in convs.
// Which messages each link gets (design, section 5.2): every link gets
// the agent's output, its questions, files, failures, progress and
// subagent lines. A reply to the message that started the turn, and to
// the prompt the trigger posted, stays in the first link, where those
// messages are; the copies are posted without the reply.
func everyLink(rows []Row, convs []surface.Conversation) []Row {
	if len(convs) < 2 {
		return rows
	}
	out := append([]Row(nil), rows...)
	for _, conv := range convs[1:] {
		for _, r := range rows {
			if r.Conv != convs[0] {
				continue
			}
			r.Conv = conv
			r.ReplyToRow = nil
			if r.Message.ReplyTo != nil && r.Message.ReplyTo.Conv == convs[0] {
				r.Message.ReplyTo = nil
			}
			out = append(out, r)
		}
	}
	return out
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
