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
	m  map[control.ThreadID]surface.Conversation
}

// linkConv returns the conversation of t's first link of this front end.
func (f *FrontEnd) linkConv(t control.ThreadInfo) (surface.Conversation, bool) {
	for _, l := range t.Links {
		if l.Client == f.Name {
			return surface.Conversation{Channel: l.Channel, Thread: l.Thread}, true
		}
	}
	return surface.Conversation{}, false
}

// convOf returns the conversation thread id is shown in by this front end,
// through its link; ok is false when it has none here.
func (f *FrontEnd) convOf(ctx context.Context, id control.ThreadID) (surface.Conversation, bool, error) {
	f.convs.mu.Lock()
	conv, ok := f.convs.m[id]
	f.convs.mu.Unlock()
	if ok {
		return conv, true, nil
	}
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpRead})
	ts, err := api.Threads(ctx, control.Filter{Thread: id})
	if err != nil || len(ts) == 0 {
		return surface.Conversation{}, false, err
	}
	conv, ok = f.linkConv(ts[0])
	if ok {
		f.convs.mu.Lock()
		if f.convs.m == nil {
			f.convs.m = map[control.ThreadID]surface.Conversation{}
		}
		f.convs.m[id] = conv
		f.convs.mu.Unlock()
	}
	return conv, ok, nil
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
