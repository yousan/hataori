package chat

import (
	"context"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// nameScopes tells the core the name the chat service shows for the
// channel of each of this front end's scopes, when its driver can tell
// (surface.Namer). Connected calls it after each connect, so a channel
// renamed while hataori runs shows its new name after the next one.
func (f *FrontEnd) nameScopes(ctx context.Context) {
	namer, ok := f.Driver.(surface.Namer)
	if !ok {
		return
	}
	for _, ch := range f.scopeList() {
		if ctx.Err() != nil {
			return
		}
		name, err := namer.Name(ctx, surface.Conversation{Channel: ch})
		if err != nil {
			f.Log.Warn("chat: a channel's name could not be read", "channel", ch, "err", err)
			continue
		}
		api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: []string{ch}, May: control.OpName})
		if err := api.SetName(ctx, control.LinkRef{Client: f.Name, Channel: ch}, name); err != nil {
			f.Log.Warn("chat: a channel's name could not be recorded", "channel", ch, "err", err)
		}
	}
}

// renameRetry is how long Renamed waits before it records a thread's name
// once more, when the core did not take it: a thread the front end has
// just opened is linked only once the core has stored its start. A
// variable for tests.
var renameRetry = 2 * time.Second

// renames is the names Renamed has still to record, by conversation, and
// the conversations whose names are being recorded.
type renames struct {
	mu      sync.Mutex
	pending map[surface.Conversation]string
	running map[surface.Conversation]bool
}

// Renamed records the name the chat service shows for a conversation, as
// the driver tells it (surface.Renamer): a thread's when it is opened and
// each time a person renames it. The names of one conversation are
// recorded one after another, the newest last: a name that comes while
// another is recorded replaces any older one still to be recorded. A
// conversation of no scope or link of this front end changes nothing.
func (f *FrontEnd) Renamed(conv surface.Conversation, name string) {
	r := &f.renames
	r.mu.Lock()
	if r.pending == nil {
		r.pending, r.running = map[surface.Conversation]string{}, map[surface.Conversation]bool{}
	}
	r.pending[conv] = name
	if r.running[conv] {
		r.mu.Unlock()
		return
	}
	r.running[conv] = true
	r.mu.Unlock()
	f.bg.Add(1)
	go f.recordNames(f.context(), conv)
}

// recordNames records the names Renamed has for conv until none is left.
// A thread's name the core did not take is tried once more after
// renameRetry, unless a newer one came meanwhile.
func (f *FrontEnd) recordNames(ctx context.Context, conv surface.Conversation) {
	defer f.bg.Done()
	r := &f.renames
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpName})
	l := control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread}
	retried := false
	for {
		r.mu.Lock()
		name, ok := r.pending[conv]
		if !ok || ctx.Err() != nil {
			delete(r.pending, conv)
			delete(r.running, conv)
			r.mu.Unlock()
			return
		}
		delete(r.pending, conv)
		r.mu.Unlock()
		err := api.SetName(ctx, l, name)
		if err == nil || conv.Thread == "" || retried {
			if err != nil {
				f.Log.Debug("chat: a conversation's name was not recorded", "channel", conv.Channel, "thread", conv.Thread, "err", err)
			}
			retried = false
			continue
		}
		retried = true
		select {
		case <-ctx.Done():
		case <-time.After(renameRetry):
		}
		r.mu.Lock()
		if _, newer := r.pending[conv]; !newer {
			r.pending[conv] = name
		}
		r.mu.Unlock()
	}
}
