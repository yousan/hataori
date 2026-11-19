package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/window"
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
	// Each plugin's manifest and grant are checked before anything starts;
	// nothing of a plugin runs yet (design, section 7.3).
	var scopeKeys []string
	for _, s := range cfg.Scopes {
		scopeKeys = append(scopeKeys, s.Key)
	}
	grants, err := plugin.Load(cfg.Plugins, scopeKeys)
	if err != nil {
		return err
	}

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
	if len(adapters) == 0 {
		return fmt.Errorf("config: key %q: name at least one agent adapter", "adapters")
	}
	agentName := cfg.Agent
	// The scopes that name a supervisor (design, section 7.1). It runs on the
	// default adapter, with that adapter's cheapest model by default.
	supervisors := map[string]supervisor.Scope{}
	for i, s := range cfg.Scopes {
		sv := s.Supervisor
		if sv == nil {
			continue
		}
		if sv.Agent != "" && sv.Agent != agentName {
			return fmt.Errorf("config: key %q: %q is not the default agent adapter %q", fmt.Sprintf("scopes[%d].supervisor.agent", i), sv.Agent, agentName)
		}
		supervisors[s.Key] = supervisor.Scope{Model: sv.Model, Effort: sv.Effort, Instructions: sv.Instructions}
	}
	var cheap string
	if cm, ok := adapters[agentName].(agent.CheapModel); ok {
		cheap = cm.CheapModel()
	}

	st, err := store.Open(ctx, abs)
	if err != nil {
		return err
	}
	defer st.Close()
	lg := slog.New(slog.NewTextHandler(logOut, nil))
	// Before anything reads a thread: existing threads get their first
	// link, once (design, section 6.2).
	scopeClients := map[string]string{}
	for _, s := range cfg.Scopes {
		scopeClients[s.Key] = s.Client
	}
	if err := st.LinkThreads(ctx, scopeClients, lg); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if cfg.OldForm {
		lg.Warn("config: owner and posters are ids, the old form; they are read as people (see docs/configuration.md)")
	}
	m := &session.Machine{Store: st}
	r := &runner.Runner{Store: st, Machine: m, Adapters: adapters,
		Host: host.Command{Path: exe, Args: []string{"host"}}, Root: abs, Log: lg, IdlePeriod: cfg.Idle,
		ContextBytes: cfg.ContextBytes, Cols: uint16(cfg.TerminalCols), Rows: uint16(cfg.TerminalRows)}
	defer r.Close()
	trust := map[string][]string{}
	for _, s := range cfg.Scopes {
		trust[s.Key] = s.TrustFiles
	}
	r.TrustFiles = func(scope string) []string { return trust[scope] }

	scopes := map[string]core.Scope{}
	var keys []string
	for _, s := range cfg.Scopes {
		scopes[s.Key] = core.Scope{Name: s.Name, Enabled: s.Enabled, Owner: s.Client, Agent: s.Agent, Model: s.Model, Effort: s.Effort, ShowIn: s.ShowIn}
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	maker := workspace.Maker{Root: abs}
	// The only way the daemon removes a workspace (design, section 4.6).
	remover := workspace.Remover{Maker: maker, Disposable: cfg.Cleanup.Disposable, OtherRoots: cfg.Cleanup.OtherRoots,
		Retention: cfg.Cleanup.TrashRetention}
	agents := make(map[string]core.AgentDefaults, len(adapters))
	for name := range adapters {
		d := cfg.AgentDefaults[name]
		agents[name] = core.AgentDefaults{Model: d.Model, Effort: d.Effort}
	}
	c := &core.Core{Agents: agents, Store: st, Machine: m, Scopes: scopes, Owners: map[string]control.ScopeOwner{}, People: cfg.People,
		Workspaces: maker, Remover: remover,
		Runner: r, Agent: agentName, Model: cfg.Model, Effort: cfg.Effort, Log: lg}
	if coreHook != nil {
		coreHook(c)
	}

	// The dashboard is a client with a read-only Caller (section 7.2). It
	// listens before anything else serves, so a config it refuses stops the
	// daemon with the reason.
	var dash *dashboard.Server
	if d := cfg.Dashboard; d != nil {
		dc := dashboard.Config{HostInput: cfg.HostInput, MayOperate: func(person string) bool {
			return c.MaySignIn(person) && dashboardCaller(cfg.People, keys, cfg.HostTerminals, cfg.HostInput, person).May.Has(control.OpHostInput)
		}}
		if l := d.Local; l != nil {
			dc.Local = &dashboard.Local{Listen: l.Listen, URL: l.URL, Key: "dashboard."}
		}
		if w := d.Web; w != nil {
			dc.Web = &dashboard.Web{Listen: w.Listen, URL: w.URL, Proxy: w.Proxy, Key: w.Key}
			if w.TLSCert != "" {
				cert, err := loadCert(in.Root, w.Key, w.TLSCert, w.TLSKey)
				if err != nil {
					return err
				}
				dc.Web.TLS = &cert
			}
		}
		if d.OldForm {
			lg.Info("the config's dashboard section uses the old form, one listener: put the web listener's keys in \"dashboard.web\" and the loopback listener's address in \"dashboard.listen\"")
		}
		if dc.SessionKey, err = sessionKey(in.Root); err != nil {
			return err
		}
		dc.Root = in.Root // the passkeys, next to the session key
		dc.Now = dashboardNow
		// A session's requests are made as its person, while the core still
		// lets that person sign in.
		forPerson := func(person string) (control.API, bool) {
			if !c.MaySignIn(person) {
				return nil, false
			}
			return c.As(dashboardCaller(cfg.People, keys, cfg.HostTerminals, cfg.HostInput, person)), true
		}
		dash, err = dashboard.Listen(dc, forPerson, lg)
		if err != nil {
			return err
		}
		defer dash.Close()
		c.SignIn = dash
	}

	// The socket's connections are the owner's, and only they may make a
	// dashboard sign-in link, attach a terminal on the host, and close and
	// remove threads.
	owner := control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop | control.OpSignIn | control.OpAttach |
		control.OpClose | control.OpRemove | control.OpName
	if cfg.HostTerminals { // off unless the config turns it on; only the owner gets it
		lister := &hostterm.Lister{Own: filepath.Base(exe)}
		if hostTermsHook != nil {
			hostTermsHook(lister)
		}
		var terms window.HostTerms = lister
		if hostTermsSource != nil {
			terms = hostTermsSource
		}
		// The hooks of the plugin a person installed (hataori plugin)
		// report the sessions they started; the list of terminals shows
		// them, from memory only.
		// Without them the terminals are listed as before: a socket that
		// cannot be made, such as a directory under /tmp that another
		// user made first, does not stop the daemon.
		c.HostTerms = terms
		if reports, err := window.Listen(abs); err != nil {
			lg.Warn("window reports: not taken; panes show no session a person started", "err", err)
		} else {
			defer reports.Close()
			c.HostTerms = reports.Terms(terms)
		}
		owner |= control.OpHost
		if cfg.HostInput { // off unless the config turns it on; only the owner gets it
			// Each instance's buffers have a name of their own, so that
			// one daemon's start never deletes another's.
			lister.Buffers = "hataori-input-" + instanceTag(abs) + "-"
			n, err := lister.DropLeftovers(ctx)
			if err != nil {
				return fmt.Errorf("host input: deleting the tmux buffers an earlier run left: %w", err)
			}
			if n > 0 {
				lg.Info("host input: deleted tmux buffers an earlier run left", "buffers", n)
			}
			c.HostInput, c.HostInputIdle, c.Stopping = lister, cfg.HostInputIdle, ctx.Done()
			if hostInputSource != nil {
				c.HostInput = hostInputSource
			}
			owner |= control.OpHostInput
			defer c.StopHostInput(15 * time.Second) // each end logged and its buffer deleted before the daemon exits
		}
	}
	srv, err := wire.Listen(abs, c.As(control.Caller{Principal: control.Principal(cfg.Owner), Scopes: keys, May: owner}), lg)
	if err != nil {
		return err
	}
	defer srv.Close()
	// A supervisor's connection is bound to its scope by a token (design,
	// section 7.1): each of its sessions gets one, and every launch of the
	// session is confined, with hataori's MCP server holding that token.
	// Each scope's budget (design, section 7.1): the waker's gate, and the
	// count of its sends and starts at the call. It tells a person in the
	// supervisor's chat thread, through the scope's chat front end, which
	// is set once the front ends are made.
	// And its loop guard (section 7.1), the waker's first gate.
	budgets := map[string]*supervisor.Budget{}
	guards := map[string]*supervisor.LoopGuard{}
	tells := map[string]func(ctx context.Context, id control.ThreadID, key, text string) error{}
	for _, s := range cfg.Scopes {
		if sv := s.Supervisor; sv != nil {
			client := s.Client
			tell := func(ctx context.Context, id control.ThreadID, key, text string) error {
				if t := tells[client]; t != nil {
					return t(ctx, id, key, text)
				}
				lg.Warn("supervisor: no chat front end to tell a person", "client", client, "text", text)
				return nil
			}
			budgets[s.Key] = &supervisor.Budget{Core: c, Scope: s.Key, Zone: m.Zone, Log: lg,
				Limits: supervisor.Limits{Turns: sv.TurnsPerHour, Sends: sv.SendsPerHour, Tokens: sv.TokensPerDay}, Tell: tell}
			guards[s.Key] = &supervisor.LoopGuard{Core: c, Scope: s.Key, Cycles: sv.LoopCycles, Tell: tell, Log: lg}
		}
	}
	sv := &supervisor.Sessions{Core: c, Tokens: srv, Root: abs, Hataori: exe, Scopes: supervisors,
		Owner: control.Principal(cfg.Owner), Cheap: cheap, Budgets: budgets, Log: lg}
	issueSupervisor := func(scope string) (string, error) { return srv.Issue(sv.API(scope, 0)) }
	if supervisorHook != nil {
		supervisorHook(issueSupervisor, srv.Revoke)
	}
	// A plugin's connection is bound by a token to the caller its grant
	// makes (design, section 7.3), and to nothing it says itself. The
	// dispatcher of its hooks knows its connections by that binding, and
	// tells it plugin_started when it connects.
	hooks := plugin.NewDispatcher(grants, lg)
	// The core runs the input hook points through it (#351), before any
	// input can come: nothing has served or recovered yet. Without plugins
	// it has none, and input goes as it did before hooks.
	if len(grants) > 0 {
		c.Hooks = hooks
	}
	asPlugin := plugin.BindAs(c.As)
	srv.Peered = func(api control.API, p *wire.Peer) { hooks.Peered(api, p) }
	issuePlugin := func(id string) (string, error) {
		for _, g := range grants {
			if g.ID == id {
				return srv.Issue(asPlugin(g.Caller()))
			}
		}
		return "", fmt.Errorf("no plugin %q in the config", id)
	}
	if pluginHook != nil {
		pluginHook(issuePlugin, srv.Revoke)
	}
	r.Confine = sv.Confine
	for scope := range supervisors {
		if name, ok := cfg.People.Resolve(supervisor.ID(scope)); !ok || !cfg.People.May(name, scope) {
			lg.Warn("the supervisor of a scope is no person of that scope: it may read its threads, but not send or start",
				"scope", scope, "id", supervisor.ID(scope))
		}
	}
	// Before anything reconnects or recovers: where each scope's waker
	// starts reading, the first time it has a supervisor, so that a turn end
	// written from here on wakes it.
	for scope := range supervisors {
		if err := supervisor.Begin(ctx, c, scope); err != nil {
			return err
		}
	}
	// Before the socket serves: the MCP servers of supervisor sessions that
	// kept running connect again with their tokens.
	if err := sv.Restore(ctx); err != nil {
		return err
	}
	if err := r.Reconnect(ctx); err != nil {
		return err
	}
	if err := c.Recover(ctx); err != nil {
		return err
	}
	// The login relay (section 4.7). It starts flows only once a client of
	// the owner takes its updates: the chat front end sets Notify.
	lr := &relay.Relay{Store: st, Adapters: adapters, People: cfg.People, Off: !cfg.LoginRelay, Log: lg, Guard: []string{exe, "login-guard"}}
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
		for _, fe := range fronts {
			tells[fe.Name] = fe.Notice
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
	if len(supervisors) > 0 {
		// Each scope's supervisor thread, once; found again after a restart;
		// and its waker, which tells it what happened in its scope. They
		// end, and are waited for, before the store is closed.
		svCtx, svCancel := context.WithCancel(feCtx)
		var svDone sync.WaitGroup
		defer func() { svCancel(); svDone.Wait() }()
		svDone.Add(1)
		go func() {
			defer svDone.Done()
			sv.Start(svCtx)
		}()
		for _, s := range cfg.Scopes {
			if s.Supervisor == nil {
				continue
			}
			// The loop guard goes before the budget, so that the threads it
			// drops spend nothing.
			b := budgets[s.Key]
			w := &supervisor.Waker{Core: c, Scope: s.Key, Owner: control.Principal(cfg.Owner), Quiet: s.Supervisor.Quiet,
				OutputMax: s.Supervisor.OutputMax, ThreadsMax: s.Supervisor.ThreadsMax, Gates: []supervisor.Gate{guards[s.Key], b}, Log: lg}
			if wakerHook != nil {
				wakerHook(w)
			}
			b.Clock = w.Clock // a test's clock, when it set one
			svDone.Add(1)
			go func() {
				defer svDone.Done()
				w.Run(svCtx)
			}()
		}
	}
	if age := cfg.Cleanup.RemoveClosedAfter; age > 0 {
		// It ends, and is waited for, before the store is closed.
		passCtx, passCancel := context.WithCancel(ctx)
		passDone := make(chan struct{})
		defer func() { passCancel(); <-passDone }()
		go func() {
			defer close(passDone)
			removalPass(passCtx, c, age, lg)
		}()
	}
	{
		// It ends, and is waited for, before the daemon returns.
		purgeCtx, purgeCancel := context.WithCancel(ctx)
		purgeDone := make(chan struct{})
		defer func() { purgeCancel(); <-purgeDone }()
		go func() {
			defer close(purgeDone)
			purgePass(purgeCtx, remover, lg)
		}()
	}
	if dash != nil {
		go func() {
			if err := dash.Serve(ctx); err != nil {
				lg.Error("dashboard", "err", err)
			}
		}()
		for _, way := range []control.Way{control.WayLocal, control.WayWeb} {
			if u := dash.URL(way); u != "" {
				lg.Info("dashboard listens", "listener", string(way), "url", u)
			}
		}
	}
	lg.Info("hataori runs", "root", abs, "agent", agentName)
	err = srv.Serve(ctx)
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return nil
	}
	return err
}

// removalPass runs the automatic removal pass until ctx ends: at the start
// and then every quarter of age, at least every second and at most every
// ten minutes.
func removalPass(ctx context.Context, c *core.Core, age time.Duration, lg *slog.Logger) {
	every := min(max(age/4, time.Second), 10*time.Minute)
	for {
		n, err := c.RemoveClosed(ctx, age)
		if err != nil && ctx.Err() == nil {
			lg.Error("removal pass", "err", err)
		}
		if n > 0 {
			lg.Info("removal pass", "removed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// purgePass deletes the entries of trash/ older than the retention, as
// the removal named them, until ctx ends: at the start and then every
// quarter of the retention, at least every second and at most every ten
// minutes. Each deleted entry is logged; one that cannot be deleted is
// logged and tried again on the next pass.
func purgePass(ctx context.Context, rm workspace.Remover, lg *slog.Logger) {
	every := min(max(rm.Retention/4, time.Second), 10*time.Minute)
	if purgeEvery > 0 {
		every = purgeEvery
	}
	for {
		purged, err := rm.Purge()
		for _, p := range purged {
			lg.Info("trash purged", "entry", p)
		}
		if err != nil {
			lg.Error("trash purge", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// purgeEvery, when a test sets it, is the interval of the trash purge.
var purgeEvery time.Duration

// dashboardCaller is the read-only Caller of a dashboard session of person:
// the scopes that person may act in, every scope for the owner (section
// 7.2). Only the owner reads the host's terminals, and only when the config
// turns them on; only the owner types into a host tmux pane, and only when
// the config turns host input on (the dashboard asks for a passkey check
// first on the web listener).
func dashboardCaller(dir *people.Directory, scopes []string, hostTerminals, hostInput bool, person string) control.Caller {
	caller := control.Caller{Principal: control.Principal(person), May: control.OpRead}
	for _, s := range scopes {
		if dir.May(person, s) {
			caller.Scopes = append(caller.Scopes, s)
		}
	}
	if hostTerminals && person == dir.Owner() {
		caller.May |= control.OpHost
		if hostInput {
			caller.May |= control.OpHostInput
		}
	}
	if dashboardHook != nil {
		dashboardHook(&caller)
	}
	return caller
}

// dashboardHook, when a test sets it, changes the dashboard's Caller.
var dashboardHook func(*control.Caller)

// dashboardNow, when a test sets it, is the dashboard server's clock.
var dashboardNow func() time.Time

// hostTermsHook, when a test sets it, changes how the daemon lists the
// host's terminals, such as to read a tmux server of the test's own.
var hostTermsHook func(*hostterm.Lister)

// instanceTag is a short name of the instance at root, the same in each
// run: the start of a hash of its absolute path.
func instanceTag(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:6])
}

// hostInputSource, when a test sets it, is what the daemon types into
// host panes with instead of tmux.
var hostInputSource core.HostInput

// hostTermsSource, when a test sets it, is what the daemon lists the
// host's terminals from instead of tmux and the process table, such as a
// list the test changes as it goes.
var hostTermsSource core.HostTerms

// supervisorHook, when a test sets it, is given what issues a supervisor's
// token for a scope, and what revokes one.
var supervisorHook func(issue func(scope string) (string, error), revoke func(token string))

// pluginHook, when a test sets it, is given what issues a token for a
// plugin of the config, by its id, and what revokes one. Starting plugins
// with it comes later (#349).
var pluginHook func(issue func(id string) (string, error), revoke func(token string))

// wakerHook, when a test sets it, sees each supervisor's waker before it
// runs, for a clock of the test's own.
var wakerHook func(*supervisor.Waker)

// coreHook, when a test sets it, is given the daemon's core, so that the
// test can call it as an in-process client such as a chat front end.
var coreHook func(*core.Core)

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
		// The channels of this client that may show other scopes' threads.
		showIn := map[string][]string{}
		for _, s := range cfg.Scopes {
			for _, e := range s.ShowIn {
				if client, channel, _ := strings.Cut(e, ":"); client == name {
					showIn[channel] = append(showIn[channel], s.Key)
				}
			}
		}
		fe := &chat.FrontEnd{Name: name, Driver: d, API: c.As, Trigger: cfg.Trigger, People: cfg.People,
			Scopes: scopes, ShowIn: showIn, Log: lg.With("client", name), Instance: filepath.Base(root),
			Outbox:  &chat.Outbox{Store: st, Client: name, Driver: d, Log: lg.With("client", name)},
			FileDir: filepath.Join(root, "state", "chat", name, "files"),
			FileRoots: func(ctx context.Context, id control.ThreadID, session int64) ([]string, error) {
				dir, err := c.WorkspaceDir(ctx, id) // as stored with the thread
				if err != nil {
					return nil, err
				}
				return []string{dir, runner.StateDir(root, session)}, nil
			}}
		if cfg.LoginRelay {
			fe.Relay = lr
		}
		if cfg.Dashboard != nil && cfg.Dashboard.Web != nil {
			fe.SignInWay = control.WayWeb // a link asked for in chat (#259)
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

// loadCert reads the web listener's certificate and key from the config
// directory; key is the prefix of their config keys.
func loadCert(root *os.Root, key, certFile, keyFile string) (tls.Certificate, error) {
	read := func(key, name string) ([]byte, error) {
		f, err := root.Open(filepath.Join("config", name))
		if err != nil {
			return nil, fmt.Errorf("config: key %q: %w", key, err)
		}
		defer f.Close()
		return io.ReadAll(f)
	}
	certPEM, err := read(key+"tls_cert", certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := read(key+"tls_key", keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("config: keys %q and %q: %w", key+"tls_cert", key+"tls_key", err)
	}
	return cert, nil
}
