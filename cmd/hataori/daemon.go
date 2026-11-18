package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/dashboard"
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
	if cfg.OldForm {
		lg.Warn("config: owner and posters are ids, the old form; they are read as people (see docs/configuration.md)")
	}
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
	c := &core.Core{Store: st, Machine: m, Scopes: scopes, Owners: map[string]control.ScopeOwner{}, People: cfg.People,
		Workspaces: workspace.Maker{Root: abs, Client: func(scope string) string { return clients[scope] }},
		Runner:     r, Agent: agentName, Model: cfg.Model, Effort: cfg.Effort, Log: lg}

	// The dashboard is a client with a read-only Caller (section 7.2). It
	// listens before anything else serves, so a config it refuses stops the
	// daemon with the reason.
	var dash *dashboard.Server
	if d := cfg.Dashboard; d != nil {
		dc := dashboard.Config{Listen: d.Listen, URL: d.URL, Proxy: d.Proxy}
		if d.TLSCert != "" {
			cert, err := loadCert(in.Root, d.TLSCert, d.TLSKey)
			if err != nil {
				return err
			}
			dc.TLS = &cert
		}
		if dc.SessionKey, err = sessionKey(in.Root); err != nil {
			return err
		}
		caller := control.Caller{Principal: control.Principal(cfg.Owner), Scopes: keys, May: control.OpRead}
		if dashboardHook != nil {
			dashboardHook(&caller)
		}
		dash, err = dashboard.Listen(dc, c.As(caller), lg)
		if err != nil {
			return err
		}
		defer dash.Close()
		c.SignIn = dash
	}

	// The socket's connections are the owner's, and only they may make a
	// dashboard sign-in link.
	owner := control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop | control.OpSignIn
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
	lr := &relay.Relay{Store: st, Adapters: adapters, People: cfg.People, Off: !cfg.LoginRelay, Log: lg}
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
	// The front ends end, and are waited for, before what they use is closed.
	feCtx, feCancel := context.WithCancel(ctx)
	var feDone sync.WaitGroup
	defer func() { feCancel(); feDone.Wait() }()
	for _, fe := range fronts {
		feDone.Add(1)
		go func() {
			defer feDone.Done()
			if err := fe.Run(feCtx); err != nil && feCtx.Err() == nil {
				lg.Error("chat front end", "client", fe.Name, "err", err)
			}
		}()
	}
	go func() {
		if err := lr.Run(ctx); err != nil {
			lg.Error("login relay", "err", err)
		}
	}()
	if dash != nil {
		go func() {
			if err := dash.Serve(ctx); err != nil {
				lg.Error("dashboard", "err", err)
			}
		}()
		lg.Info("dashboard listens", "url", dash.URL())
	}
	lg.Info("hataori runs", "root", abs, "agent", agentName)
	err = srv.Serve(ctx)
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return nil
	}
	return err
}

// dashboardHook, when a test sets it, changes the dashboard's Caller.
var dashboardHook func(*control.Caller)

// sessionKeyFile holds the key that signs dashboard sessions, so that a
// session outlives a restart. Removing it ends every session.
const sessionKeyFile = "state/dashboard-session.key"

// sessionKey reads the dashboard's session key, and makes it the first
// time. Whoever reads the key can make sessions, so a key file that others
// may read is refused, as the config is. The instance lock is held, so no
// other daemon makes it at the same time.
func sessionKey(root *os.Root) ([]byte, error) {
	f, err := root.OpenFile(sessionKeyFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("dashboard: %s: %w", sessionKeyFile, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("dashboard: %s is not a regular file", sessionKeyFile)
		}
		if st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("dashboard: %s is accessible to other users (mode %04o); run chmod 600 on it, or remove it to end every session", sessionKeyFile, st.Mode().Perm())
		}
		b, err := io.ReadAll(io.LimitReader(f, 1024))
		if err != nil {
			return nil, fmt.Errorf("dashboard: %s: %w", sessionKeyFile, err)
		}
		if len(b) < 32 {
			return nil, fmt.Errorf("dashboard: %s is shorter than 32 bytes: remove it to make a new one", sessionKeyFile)
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	// Written whole under another name, then renamed, so that a crash
	// leaves no short key behind.
	tmp := sessionKeyFile + ".new"
	if err := root.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	f, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, sessionKeyFile)
	}
	if err != nil {
		root.Remove(tmp)
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	return key, nil
}

// frontEndHook, when a test sets it, sees each front end before it runs,
// for a clock of the test's own.
var frontEndHook func(*chat.FrontEnd)

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
			scopes[s.Key] = chat.Scope{Enabled: s.Enabled}
		}
		client := name
		fe := &chat.FrontEnd{Name: name, Driver: d, API: c.As, Trigger: cfg.Trigger, People: cfg.People,
			Scopes: scopes, Log: lg.With("client", name), Instance: filepath.Base(root),
			Outbox:  &chat.Outbox{Store: st, Client: name, Driver: d, Log: lg.With("client", name)},
			FileDir: filepath.Join(root, "state", "chat", name, "files"),
			FileRoots: func(_ context.Context, id control.ThreadID, session int64) ([]string, error) {
				return []string{workspace.Path(root, client, id.Scope, id.Key), runner.StateDir(root, session)}, nil
			}}
		if cfg.LoginRelay {
			fe.Relay = lr
		}
		if frontEndHook != nil {
			frontEndHook(fe)
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

// loadCert reads the dashboard's certificate and key from the config
// directory.
func loadCert(root *os.Root, certFile, keyFile string) (tls.Certificate, error) {
	read := func(key, name string) ([]byte, error) {
		f, err := root.Open(filepath.Join("config", name))
		if err != nil {
			return nil, fmt.Errorf("config: key %q: %w", key, err)
		}
		defer f.Close()
		return io.ReadAll(f)
	}
	certPEM, err := read("dashboard.tls_cert", certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := read("dashboard.tls_key", keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("config: keys %q and %q: %w", "dashboard.tls_cert", "dashboard.tls_key", err)
	}
	return cert, nil
}
