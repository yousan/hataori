package chat

import (
	"context"
	"sync"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// convCache remembers the conversation of each thread this front end has
// looked up: a thread's links do not change while it reads its events.
type convCache struct {
	mu sync.Mutex
	m  map[control.ThreadID][]surface.Conversation
}

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
// end, one per link, the first link first; none when it has no link here.
func (f *FrontEnd) convsOf(ctx context.Context, id control.ThreadID) ([]surface.Conversation, error) {
	f.convs.mu.Lock()
	convs, ok := f.convs.m[id]
	f.convs.mu.Unlock()
	if ok {
		return convs, nil
	}
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpRead})
	ts, err := api.Threads(ctx, control.Filter{Thread: id})
	if err != nil || len(ts) == 0 {
		return nil, err
	}
	convs = f.linkConvs(ts[0])
	if len(convs) > 0 {
		f.convs.mu.Lock()
		if f.convs.m == nil {
			f.convs.m = map[control.ThreadID][]surface.Conversation{}
		}
		f.convs.m[id] = convs
		f.convs.mu.Unlock()
	}
	return convs, nil
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
