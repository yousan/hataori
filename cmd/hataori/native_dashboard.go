package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/version"
)

// Native startup is an explicit CLI choice, independent of browser config.
// It neither launches another daemon nor changes an externally managed one.
type nativeDashboardKey struct{}

type nativeDashboard struct {
	server *dashboard.Server
	log    *slog.Logger
	done   chan struct{}
}

func listenNativeDashboard(root, identity string, cfg *instance.Config, c *core.Core, scopes []string, hostInput bool, log *slog.Logger) (*nativeDashboard, error) {
	// This is a dashboard Caller of the configured owner, never the control
	// socket's Caller. In particular, starting in an unlisted scope is not
	// granted. Pending control-socket policy additions keep their zero value.
	caller := dashboardCaller(cfg.People, scopes, cfg.HostTerminals, hostInput, cfg.Owner)
	dc := dashboard.Config{
		Native: &dashboard.Native{
			Transport: nativehttp.Config{Root: root, Instance: identity, Version: version.String()},
			Person:    cfg.Owner, API: c.As(caller),
		},
		HostInput: hostInput, AttachHost: attachHost, AttachIdle: cfg.HostInputIdle,
		MayAct:     func(person string) bool { return person == cfg.Owner && caller.May.Has(control.OpSend) },
		MayOperate: func(person string) bool { return person == cfg.Owner && caller.May.Has(control.OpHostInput) },
	}
	s, err := dashboard.Listen(dc, nil, log)
	if err != nil {
		return nil, fmt.Errorf("native dashboard: %w", err)
	}
	return &nativeDashboard{server: s, log: log}, nil
}

func (n *nativeDashboard) serve(ctx context.Context) {
	n.done = make(chan struct{})
	go func() {
		defer close(n.done)
		if err := n.server.Serve(ctx); err != nil {
			n.log.Error("native dashboard", "err", err)
		}
	}()
}

func (n *nativeDashboard) close() {
	// Also safe on a startup error before serve. http.Server.Close alone
	// does not wait for handlers or hijacked WebSockets.
	n.server.CloseAndWait()
	if n.done != nil {
		<-n.done
	}
}
