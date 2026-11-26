package main

import (
	"context"
	"slices"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/store"
)

func revokeUnservedInputs(ctx context.Context, st *store.Store, cfg *instance.Config) error {
	scopes := make(map[string]instance.Scope, len(cfg.Scopes))
	for _, s := range cfg.Scopes {
		scopes[s.Key] = s
	}
	return chat.RevokeUnservedInputs(ctx, st, func(client, channel, scope string) bool {
		s, ok := scopes[scope]
		return ok && ((s.Client == client && s.Key == channel) || slices.Contains(s.ShowIn, client+":"+channel))
	})
}
