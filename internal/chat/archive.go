package chat

import (
	"context"
	"encoding/json"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// threadPart is the part of the row of a state event that archives or
// un-archives a chat thread: after what else the event shows.
const threadPart = 1 << 28

// archiveRows are the rows that archive the chat threads of a thread of a
// scope with ArchiveWhenAsleep when it goes asleep or is closed, and
// un-archive them when it is woken: whatever woke it, a message in the chat
// thread (which the service has un-archived by itself), the dashboard or
// `hataori attach`. The rows of the wake come before what the woken thread
// posts, since they are the state event's. A driver that cannot set a
// thread's flags (Limits.SetThread) gets none, and its threads stay open.
func (f *FrontEnd) archiveRows(e control.Event, convs []surface.Conversation) []Row {
	if e.Kind != "state" || !f.Scopes[e.Scope].ArchiveWhenAsleep || !f.Driver.Limits().SetThread {
		return nil
	}
	var d struct{ From, To control.State }
	if json.Unmarshal(e.Data, &d) != nil {
		return nil
	}
	var op Op
	switch {
	case d.To == control.Asleep || d.To == control.Closed:
		op = Archive
	case d.From == control.Asleep:
		op = Unarchive
	default:
		return nil
	}
	var rows []Row
	for _, conv := range convs {
		if conv.Thread != "" { // a channel is never archived
			rows = append(rows, Row{Event: e.Pos, Part: threadPart, Conv: conv, Op: op})
		}
	}
	return rows
}

// archive archives or un-archives the chat thread conv, once: a failure is
// logged, the row is dropped, and nothing else changes; the rows behind it
// go on.
func (o *Outbox) archive(ctx context.Context, conv surface.Conversation, r pending, op Op) outcome {
	err := o.Driver.SetThread(ctx, conv, surface.ThreadMeta{Archived: op == Archive})
	if err == nil {
		return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'sent', sent_at = ? WHERE id = ?`, o.now().UnixMilli(), r.id)
	}
	if ctx.Err() != nil {
		return stuck
	}
	o.Log.Warn("chat outbox: "+string(op)+" of a chat thread failed", "key", key(r.event, r.part), "err", err)
	return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'dropped', failures = failures + 1 WHERE id = ?`, r.id)
}
