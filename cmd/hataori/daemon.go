package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// daemon runs the instance at root until ctx ends. Every error before it
// serves is one line.
func daemon(ctx context.Context, root string, logOut io.Writer) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	in, err := instance.Open(abs)
	if err != nil {
		return err
	}
	defer in.Close()
	cfg := in.Config

	adapters := map[string]agent.Adapter{}
	for name, settings := range cfg.Adapters {
		f, ok := agent.Lookup(name)
		if !ok {
			return fmt.Errorf("config: key %q: no agent adapter %q in this binary", "adapters."+name, name)
		}
		ad, err := f(settings)
		if err != nil {
			return err
		}
		adapters[name] = ad
	}
	if len(adapters) != 1 {
		return fmt.Errorf("config: key %q: name exactly one agent adapter (%d given)", "adapters", len(adapters))
	}
	var agentName string
	for name := range adapters {
		agentName = name
	}

	st, err := store.Open(ctx, abs)
	if err != nil {
		return err
	}
	defer st.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lg := slog.New(slog.NewTextHandler(logOut, nil))
	m := &session.Machine{Store: st}
	r := &runner.Runner{Store: st, Machine: m, Adapters: adapters,
		Host: host.Command{Path: exe, Args: []string{"host"}}, Root: abs, Log: lg, IdlePeriod: cfg.Idle,
		ContextBytes: cfg.ContextBytes}
	defer r.Close()

	scopes := map[string]core.Scope{}
	clients := map[string]string{}
	var keys []string
	for _, s := range cfg.Scopes {
		scopes[s.Key] = core.Scope{Enabled: s.Enabled, Owner: s.Client, Model: s.Model, Effort: s.Effort}
		clients[s.Key] = s.Client
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	c := &core.Core{Store: st, Machine: m, Scopes: scopes, Owners: map[string]control.ScopeOwner{},
		Workspaces: workspace.Maker{Root: abs, Client: func(scope string) string { return clients[scope] }},
		Runner:     r, Agent: agentName, Model: cfg.Model, Effort: cfg.Effort, Log: lg}

	owner := control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop
	srv, err := wire.Listen(abs, c.As(control.Caller{Principal: control.Principal(cfg.Owner), Scopes: keys, May: owner}), lg)
	if err != nil {
		return err
	}
	defer srv.Close()
	if err := r.Reconnect(ctx); err != nil {
		return err
	}
	if err := c.Recover(ctx); err != nil {
		return err
	}
	// The login relay (section 4.7). It starts flows only once a client of
	// the owner takes its updates: the chat front end sets Notify.
	lr := &relay.Relay{Store: st, Adapters: adapters, Owner: control.Principal(cfg.Owner), Off: !cfg.LoginRelay, Log: lg}
	var fronts []*chat.FrontEnd
	if len(cfg.Drivers) > 0 {
		confDir, err := in.Root.OpenRoot("config")
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		defer confDir.Close()
		if fronts, err = frontEnds(abs, confDir, cfg, st, c, lr, lg); err != nil {
			return err
		}
	}
	for _, fe := range fronts {
		go func() {
			if err := fe.Run(ctx); err != nil && ctx.Err() == nil {
				lg.Error("chat front end", "client", fe.Name, "err", err)
			}
		}()
	}
	go func() {
		if err := lr.Run(ctx); err != nil {
			lg.Error("login relay", "err", err)
		}
	}()
	lg.Info("hataori runs", "root", abs, "agent", agentName)
	err = srv.Serve(ctx)
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return nil
	}
	return err
}

// frontEnds makes one chat front end for each surface driver the config
// names, in the binary's registry, and makes it the owner of the scopes
// whose client has the driver's name. Scopes of other clients keep no
// owner here.
func frontEnds(root string, confDir *os.Root, cfg *instance.Config, st *store.Store, c *core.Core, lr *relay.Relay, lg *slog.Logger) ([]*chat.FrontEnd, error) {
	var names []string
	for name := range cfg.Drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	var fronts []*chat.FrontEnd
	for _, name := range names {
		f, ok := surface.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("config: key %q: no surface driver %q in this binary", "drivers."+name, name)
		}
		d, err := f(confDir, cfg.Drivers[name])
		if err != nil {
			return nil, err
		}
		scopes := map[string]chat.Scope{}
		for _, s := range cfg.Scopes {
			if s.Client != name {
				continue
			}
			var posters []control.Principal
			for _, p := range s.Posters {
				posters = append(posters, control.Principal(p))
			}
			scopes[s.Key] = chat.Scope{Enabled: s.Enabled, Posters: posters}
		}
		client := name
		fe := &chat.FrontEnd{Name: name, Driver: d, API: c.As, Trigger: cfg.Trigger, Owner: control.Principal(cfg.Owner),
			Scopes: scopes, Log: lg.With("client", name), Instance: filepath.Base(root),
			Outbox:  &chat.Outbox{Store: st, Client: name, Driver: d, Log: lg.With("client", name)},
			FileDir: filepath.Join(root, "state", "chat", name, "files"),
			FileRoots: func(_ context.Context, id control.ThreadID, session int64) ([]string, error) {
				return []string{workspace.Path(root, client, id.Scope, id.Key), runner.StateDir(root, session)}, nil
			}}
		if cfg.LoginRelay {
			fe.Relay = lr
		}
		c.Owners[name] = fe
		fronts = append(fronts, fe)
	}
	if len(fronts) > 0 {
		// The owner's direct messages: every front end hears the relay.
		lr.Notify = func(u relay.Update) {
			for _, fe := range fronts {
				fe.LoginUpdate(u)
			}
		}
	}
	return fronts, nil
}
