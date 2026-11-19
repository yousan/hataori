package chat

import (
	"context"
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
// just opened is linked only once the core has stored its start.
const renameRetry = 2 * time.Second

// Renamed records the name the chat service shows for a conversation, as
// the driver tells it (surface.Renamer): a thread's when it is opened and
// each time a person renames it. A conversation of no scope or link of
// this front end changes nothing.
func (f *FrontEnd) Renamed(conv surface.Conversation, name string) {
	ctx := f.context()
	f.bg.Add(1)
	go func() {
		defer f.bg.Done()
		api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpName})
		l := control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread}
		err := api.SetName(ctx, l, name)
		if err == nil || conv.Thread == "" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(renameRetry):
		}
		if err := api.SetName(ctx, l, name); err != nil {
			f.Log.Debug("chat: a thread's name was not recorded", "channel", conv.Channel, "thread", conv.Thread, "err", err)
		}
	}()
}
