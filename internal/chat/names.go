package chat

import (
	"context"

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
