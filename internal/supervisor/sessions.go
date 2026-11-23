package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/workspace"
)

// TokenEnv is the environment variable that holds the token of `hataori
// mcp`'s connection. Sessions puts it in the supervisor's MCP config.
const TokenEnv = "HATAORI_TOKEN"

// MCPName is the name the supervisor's agent lists hataori's tools under.
const MCPName = "hataori"

// StartKey is the key of the Start that makes a scope's first supervisor
// thread; one that replaces a closed one is StartKey-after-<its number>. The
// core keeps a Start's key per scope, so a daemon that died during the
// Start gets the same thread from it instead of a second one.
const StartKey = "hataori-supervisor"

// FirstInput is the first message of a supervisor's thread. Its first line
// is the thread's title.
const FirstInput = "Supervisor of this channel\n" +
	"You are this channel's supervisor. Your hataori tools list, read, start and send to the threads of this channel; " +
	"you are told when something happens in them. For now, reply with one line saying you are ready."

// TokenFile holds the token of a supervisor session, in its state
// directory, outside its own directory. A session keeps its token for its
// whole life (decision 1 of #267: a token per supervisor session): each
// launch of it gives its MCP server the same one, and the daemon binds it
// again after a restart, while the agent and its MCP server still run.
const TokenFile = "supervisor-token"

// Tokens binds connections of the control socket to a Caller: the daemon's
// wire.Server.
type Tokens interface {
	Issue(api control.API) (string, error)
	Restore(token string, api control.API) error
	Revoke(token string)
}

// Scope is a scope's supervisor, as the config names it.
type Scope struct {
	Model        string // the model; the adapter's cheapest when empty
	Effort       string // the effort; the scope's when empty
	Instructions string // appended to the agent's own instructions
	Repos        []Repo // the only repositories its Start may clone; none: it starts nothing
}

// Sessions is the supervisors' part of the daemon (design, section 7.1): it
// starts each scope's supervisor thread once, gives each launch of its
// sessions hataori's MCP server with the session's token, and binds the
// tokens of the sessions that kept running across a restart. It is a client
// inside the daemon: to the core the threads are ordinary threads started
// confined, and the token's connection an ordinary limited Caller.
type Sessions struct {
	Core    *core.Core
	Tokens  Tokens
	Root    string            // the instance root, for `hataori mcp --root`
	Hataori string            // the hataori binary that runs `hataori mcp`
	Scopes  map[string]Scope  // the scopes that name a supervisor, by key
	Owner   control.Principal // whose Start makes the threads: the instance's owner
	Cheap   string            // the adapter's cheapest model; "" when it names none
	// Budgets are the scopes' budgets, by key: a scope's sends and starts
	// are counted, and refused over budget, on its token's connection.
	Budgets map[string]*Budget
	Log     *slog.Logger

	mu     sync.Mutex
	issued map[control.ThreadID]issued // by thread: the token of its newest session
}

type issued struct {
	session int64
	token   string
}

// tokenLen is the length of a token wire.Server.Issue makes: 32 random
// bytes in unpadded base64.
const tokenLen = 43

// Confine gives one launch of a confined session hataori's MCP server for
// the session's scope, with the session's token, and the supervisor's
// instructions. It is the runner's Confine. A scope that names no
// supervisor, as after the operator removed it, gives none, and the launch
// fails.
func (s *Sessions) Confine(ctx context.Context, l runner.ConfinedLaunch) (runner.Confined, error) {
	sv, ok := s.Scopes[l.Scope]
	if !ok {
		return runner.Confined{}, fmt.Errorf("scope %q names no supervisor", l.Scope)
	}
	token, err := s.bind(l.Session, l.Thread, l.Scope, l.StateDir, true)
	if err != nil {
		return runner.Confined{}, err
	}
	return runner.Confined{
		MCP: []agent.MCPServer{{Name: MCPName, Argv: []string{s.Hataori, "mcp", "--scope", l.Scope, "--root", s.Root},
			Env: []string{TokenEnv + "=" + token}}},
		Instructions: sv.Instructions,
	}, nil
}

// bind binds the token of session, kept in its state directory, to the
// scope's supervisor Caller. With issue, a session without a token gets a
// new one; without, it is left without. The token of an older session of
// the same thread, as one a session replaced, is revoked.
func (s *Sessions) bind(session int64, thread control.ThreadID, scope, stateDir string, issue bool) (string, error) {
	api := s.API(scope, thread)
	file := filepath.Join(stateDir, TokenFile)
	b, err := os.ReadFile(file)
	token := strings.TrimSpace(string(b))
	switch {
	case err == nil && len(token) == tokenLen:
		if err := s.Tokens.Restore(token, api); err != nil {
			return "", err
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("supervisor: %w", err)
	case !issue:
		return "", nil
	default:
		// None yet, or one cut short by a daemon that died while it wrote
		// it, before any agent was started with it.
		if token, err = s.Tokens.Issue(api); err != nil {
			return "", err
		}
		if err := writeToken(file, token); err != nil {
			s.Tokens.Revoke(token)
			return "", err
		}
	}
	s.mu.Lock()
	if s.issued == nil {
		s.issued = map[control.ThreadID]issued{}
	}
	old, had := s.issued[thread]
	if !had || old.session <= session {
		s.issued[thread] = issued{session, token}
	}
	s.mu.Unlock()
	if had && old.session < session && old.token != token {
		s.Tokens.Revoke(old.token)
	}
	return token, nil
}

// API is what the connection of scope's supervisor is bound to: its
// Caller, with its sends and starts counted against the scope's budget,
// and its starts limited to the scope's repos (none for a scope that names
// no supervisor).
func (s *Sessions) API(scope string, thread control.ThreadID) control.API {
	api := s.Core.As(Caller(scope, thread))
	if b := s.Budgets[scope]; b != nil {
		api = b.API(api)
	}
	return Repos(api, scope, s.Scopes[scope].Repos, s.Log)
}

// writeToken writes token to file, readable by this user only, and syncs it.
func writeToken(file, token string) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("supervisor: %w", err)
	}
	_, err = f.WriteString(token + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("supervisor: %w", err)
	}
	return nil
}

// Restore binds the tokens of the confined sessions whose agent may still
// run, as after a daemon restart: their MCP servers connect again with the
// token they were started with. Call it before the control socket serves.
// A session of a scope that names no supervisor any more is not bound.
func (s *Sessions) Restore(ctx context.Context) error {
	live, err := s.Core.LiveConfined(ctx)
	if err != nil {
		return err
	}
	for _, l := range live {
		if _, ok := s.Scopes[l.Scope]; !ok {
			s.Log.Warn("a confined session of a scope that names no supervisor keeps no tools", "session", l.Session, "scope", l.Scope)
			continue
		}
		if _, err := s.bind(l.Session, l.Thread, l.Scope, runner.StateDir(s.Root, l.Session), false); err != nil {
			s.Log.Error("binding a supervisor's token again", "session", l.Session, "err", err)
		}
	}
	return nil
}

// Start starts the thread of each scope's supervisor, once: a scope whose
// thread exists already gets it back from the core by StartKey. A start
// that fails before the thread exists, as when the chat service did not
// open its chat thread, is tried again later until ctx ends; one the core
// refuses is logged and not tried again.
func (s *Sessions) Start(ctx context.Context) {
	var keys []string
	for k := range s.Scopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var wg sync.WaitGroup
	for _, scope := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wait := time.Second; ; wait = min(2*wait, 5*time.Minute) {
				id, err := s.StartOne(ctx, scope)
				if err == nil || id != 0 || final(err) {
					if err != nil {
						s.Log.Error("supervisor not started", "scope", scope, "thread", int64(id), "err", ErrText(err))
					} else {
						s.Log.Info("supervisor's thread", "scope", scope, "thread", int64(id))
					}
					return
				}
				s.Log.Warn("supervisor not started; trying again", "scope", scope, "in", wait.String(), "err", ErrText(err))
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}()
	}
	wg.Wait()
}

// ErrNoChat: the scope's client runs no chat front end in this daemon, so
// there is no channel to open the supervisor's thread in.
var ErrNoChat = errors.New("the scope's client has no chat front end in this daemon")

// final reports whether a Start refused with err would be refused again.
func final(err error) bool {
	return errors.Is(err, control.ErrForbidden) || errors.Is(err, core.ErrDisabled) || errors.Is(err, core.ErrConfinedKey) ||
		errors.Is(err, ErrNoChat) || stoppedByPlugin(err) ||
		errors.Is(err, control.ErrUnknownEffort) || errors.Is(err, context.Canceled)
}

// stoppedByPlugin reports whether a plugin's handler stopped the input of
// err; one that failed, such as a plugin not connected yet, did not, and
// the input is tried again.
func stoppedByPlugin(err error) bool {
	var s *control.Stopped
	return errors.As(err, &s) && !s.Failed
}

// ErrText is err as a log line shows it: a plugin's stop without its
// reason, which may quote the input.
func ErrText(err error) string {
	var s *control.Stopped
	if errors.As(err, &s) && !s.Failed {
		return fmt.Sprintf("the plugin %s stopped the input at %s", s.Plugin, s.Point)
	}
	return err.Error()
}

// StartOne starts the supervisor thread of scope, or returns the one it
// has: the scope's newest confined thread, unless a person closed or
// removed it, which a new one then replaces. One that failed is kept, and
// logged: closing it is how a person asks for a new one. A thread whose
// session failed to start is returned with the error.
func (s *Sessions) StartOne(ctx context.Context, scope string) (control.ThreadID, error) {
	sv, ok := s.Scopes[scope]
	if !ok {
		return 0, fmt.Errorf("supervisor: scope %q names no supervisor", scope)
	}
	// The scope's directory is decided before anything else, so that a
	// thread that exists now, from before scopes had one, gives its own.
	dir, err := s.Dir(ctx, scope)
	if err != nil {
		return 0, err
	}
	last, state, err := s.Core.LastConfined(ctx, scope)
	if err != nil {
		return 0, err
	}
	key := StartKey
	switch {
	case last != 0 && state != control.Closed && state != control.Removed:
		if state == control.Failed {
			s.Log.Warn("the supervisor's thread failed; close it, and the next start of the daemon opens a new one",
				"scope", scope, "thread", int64(last))
		}
		return last, nil
	case last != 0:
		key = fmt.Sprintf("%s-after-%d", StartKey, last)
	}
	if _, ok := s.Core.Owners[s.Core.Scopes[scope].Owner]; !ok {
		return 0, fmt.Errorf("supervisor: scope %q: %w", scope, ErrNoChat)
	}
	model := sv.Model
	if model == "" {
		model = s.Cheap
	}
	caller := control.Caller{Principal: s.Owner, Scopes: []string{scope}, May: control.OpRead | control.OpStart}
	return s.Core.StartConfined(ctx, caller, control.StartRequest{Key: key, Scope: scope,
		Input: control.Input{Text: FirstInput}, Model: model, Effort: sv.Effort}, dir)
}

// Dir returns the directory of scope's supervisor threads (design, section
// 7.1, "Its directory and its notes"), recorded in the store the first time
// it is asked for: the directory of the scope's newest supervisor thread
// that has one, so that what it holds carries on, else a new one in
// <root>/confined/ named from the scope's key (workspace.ScopePath). Every
// supervisor thread of the scope started from then on starts in it; nothing
// here makes, empties or removes it.
func (s *Sessions) Dir(ctx context.Context, scope string) (string, error) {
	db := s.Core.Store.DB
	var dir string
	err := db.QueryRowContext(ctx, `SELECT dir FROM supervisor_dirs WHERE scope = ?`, scope).Scan(&dir)
	if err == nil {
		return dir, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("supervisor: the directory of %q: %w", scope, err)
	}
	err = db.QueryRowContext(ctx, `SELECT workspace FROM threads WHERE scope = ? AND confined = 1 AND workspace != ''
		ORDER BY id DESC LIMIT 1`, scope).Scan(&dir)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		dir = workspace.ScopePath(s.Root, scope)
	case err != nil:
		return "", fmt.Errorf("supervisor: the directory of %q: %w", scope, err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO supervisor_dirs (scope, dir) VALUES (?, ?) ON CONFLICT (scope) DO NOTHING`, scope, dir)
	if err != nil {
		return "", fmt.Errorf("supervisor: the directory of %q: %w", scope, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.Log.Info("the scope's supervisor directory", "scope", scope, "dir", dir)
		return dir, nil
	}
	if err := db.QueryRowContext(ctx, `SELECT dir FROM supervisor_dirs WHERE scope = ?`, scope).Scan(&dir); err != nil {
		return "", fmt.Errorf("supervisor: the directory of %q: %w", scope, err)
	}
	return dir, nil
}
