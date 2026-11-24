package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// serves says whether threads of scope may be shown in channel, as the
// config the daemon loaded says: the channel is the scope's own, or in the
// scope's show_in. A link made while the channel was listed is not served
// once it no longer is; its row stays, so that putting the entry back shows
// the thread there again (#572).
func (f *FrontEnd) serves(channel, scope string) bool {
	return slices.Contains(f.scopesIn(channel), scope)
}

// neverSince is the since of a link that is not served: nothing written in
// its chat thread is input.
const neverSince int64 = math.MaxInt64

// settleLinks brings the stored state of the front end's links in line with
// the config the daemon loaded. A link that is no longer served (serves)
// loses its pending outbox rows: they were written while the channel was
// listed, and must not be posted there now. What is written in its chat
// thread meanwhile is no input, also when it is caught up on later: a link
// served again is so since this start. One log line names the links that
// are not served, the first maxListed of them. Run calls it before the
// outbox is sent and the driver connects.
func (f *FrontEnd) settleLinks(ctx context.Context) error {
	type link struct {
		id     int64
		since  int64
		served bool
	}
	var names []string
	dropped := int64(0)
	err := f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT l.id, l.channel, l.thread, l.since, t.id, t.scope FROM links l
			JOIN threads t ON t.id = l.thread_id WHERE l.client = ? ORDER BY l.id`, f.Name)
		if err != nil {
			return err
		}
		defer rows.Close()
		var links []link
		for rows.Next() {
			var l link
			var id int64
			var channel, thread, scope string
			if err := rows.Scan(&l.id, &channel, &thread, &l.since, &id, &scope); err != nil {
				return err
			}
			l.served = f.serves(channel, scope)
			if !l.served {
				names = append(names, fmt.Sprintf("thread %d of %s in %s/%s", id, scope, channel, thread))
			}
			links = append(links, l)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, l := range links {
			since := l.since
			switch {
			case !l.served:
				since = neverSince
			case l.since == neverSince:
				since = f.inboundState().started()
			}
			if since != l.since {
				if _, err := tx.ExecContext(ctx, `UPDATE links SET since = ? WHERE id = ?`, since, l.id); err != nil {
					return err
				}
			}
			if l.served {
				continue
			}
			r, err := tx.ExecContext(ctx, `UPDATE chat_outbox SET state = 'dropped' WHERE client = ? AND link = ? AND state = 'pending'`, f.Name, l.id)
			if err != nil {
				return err
			}
			n, err := r.RowsAffected()
			if err != nil {
				return err
			}
			dropped += n
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("chat: settling the links: %w", err)
	}
	if len(names) > 0 {
		n := len(names)
		names = names[:min(n, maxListed)]
		f.Log.Warn("chat: links are no longer served, as their channel is neither the scope's own nor in its show_in",
			"count", n, "links", names, "pending_messages_dropped", dropped)
	}
	return nil
}

// linkConvs returns the conversations of t's links of this front end that
// it serves, the first link first.
func (f *FrontEnd) linkConvs(t control.ThreadInfo) []surface.Conversation {
	var out []surface.Conversation
	for _, l := range t.Links {
		if l.Client == f.Name && f.serves(l.Channel, t.Scope) {
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
// end, one per link that it serves, the first link first; none when it has
// no such link here. It reads the links as they
// are now, so a link made since, by any client, is seen at once.
func (f *FrontEnd) convsOf(ctx context.Context, id control.ThreadID) ([]surface.Conversation, error) {
	scopes := f.scopeList()
	if len(scopes) == 0 {
		return nil, nil
	}
	q := `SELECT l.channel, l.thread, t.scope FROM links l JOIN threads t ON t.id = l.thread_id
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
		var scope string
		if err := rows.Scan(&c.Channel, &c.Thread, &scope); err != nil {
			return nil, err
		}
		if f.serves(c.Channel, scope) {
			out = append(out, c)
		}
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
// without it. So is a mention (a supervisor's "@<name>", #651): a person is
// mentioned once, in the conversation the row was written for.
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
			c.Message.Notify = nil
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
// as "<person>: <text>" (design, section 5.2). What a person wrote in a
// link is shown in the others; what came through no link but names the way
// it came (via), such as input given in the dashboard, is shown in every
// link. An input of neither, such as the supervisor's, is not shown, nor
// is the supervisor's input that a person approved as its proposal (via
// proposal), which its own thread showed.
func (f *FrontEnd) mirrorRows(e control.Event, convs []surface.Conversation) []Row {
	if e.Kind != "input" && e.Kind != "answer" {
		return nil
	}
	var d struct {
		Text      string           `json:"text"`
		Principal string           `json:"principal"`
		Link      *control.LinkRef `json:"link"`
		Via       string           `json:"via"` // since protocol 3.23
	}
	if json.Unmarshal(e.Data, &d) != nil || strings.TrimSpace(d.Text) == "" || (d.Link == nil && (d.Via == "" || d.Via == control.ViaProposal)) {
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
