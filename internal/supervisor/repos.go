package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yousan/hataori/internal/control"
)

// Repo is one of the repositories a scope's supervisor works on
// (scopes[].supervisor.repos; design, section 7.1, "Orders").
type Repo struct {
	Address string // as an order's repo: names it; what a start clones
	Forge   string // its forge name, "<owner>/<name>"; empty when it has none
}

// Repos limits the starts of api, a supervisor's caller in scope, to the
// repositories repos lists: a Start in scope names one by its address, as
// listed, or by its forge name, which is replaced by the address. Any
// other Start in scope is refused with control.ErrForbidden before it
// reaches api, so nothing is cloned; with no repos, every one is. A Start
// in another scope goes on to api, the core, which refuses it as it
// refuses any call of the caller outside its scope. The daemon binds it to
// the supervisor's token, so nothing `hataori mcp` sends gets past it.
func Repos(api control.API, scope string, repos []Repo, log *slog.Logger) control.API {
	return &repoLimited{API: api, scope: scope, repos: repos, log: log}
}

type repoLimited struct {
	control.API
	scope string
	repos []Repo
	log   *slog.Logger
}

func (l *repoLimited) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	if r.Scope != l.scope {
		return l.API.Start(ctx, r)
	}
	address, err := l.check(r.Repo)
	if err != nil {
		return 0, err
	}
	r.Repo = address
	return l.API.Start(ctx, r)
}

// CheckStart refuses a Start in scope of repo, as the supervisor's
// connection does (Repos), unless repo is the address of one of the
// scope's repos. It is the core's check of the Start that a person
// approved as the supervisor's proposal (#650): the proposal named the
// listed address, and the scope's repos may have changed since.
func (s *Sessions) CheckStart(scope, repo string) error {
	l := &repoLimited{scope: scope, repos: s.Scopes[scope].Repos, log: s.Log}
	address, err := l.check(repo)
	if err == nil && address != repo {
		err = fmt.Errorf("the supervisor of scope %q may start a thread only in a repository its scope lists under supervisor.repos: %w", scope, control.ErrForbidden)
	}
	return err
}

// check returns the listed address that repo names, or the refusal of a
// Start of it, logged.
func (l *repoLimited) check(repo string) (string, error) {
	address, ok := l.address(repo)
	if !ok {
		reason := "the repository is not one of the scope's supervisor.repos"
		err := fmt.Errorf("the supervisor of scope %q may start a thread only in a repository its scope lists under supervisor.repos, "+
			"by the address listed there or by its forge name, and this is not one: %w", l.scope, control.ErrForbidden)
		if len(l.repos) == 0 {
			reason = "the scope lists no supervisor.repos"
			err = fmt.Errorf("the supervisor of scope %q starts no thread: its scope lists no repositories under supervisor.repos: %w",
				l.scope, control.ErrForbidden)
		}
		l.log.Warn("control call refused", "op", "Start", "from", ID(l.scope), "scope", l.scope, "reason", reason)
		return "", err
	}
	return address, nil
}

// address returns the listed address that repo names, as listed or by its
// forge name, whose case does not matter.
func (l *repoLimited) address(repo string) (string, bool) {
	if repo == "" {
		return "", false
	}
	for _, e := range l.repos {
		if e.Address == repo {
			return e.Address, true
		}
	}
	for _, e := range l.repos {
		if e.Forge != "" && strings.EqualFold(e.Forge, repo) {
			return e.Address, true
		}
	}
	return "", false
}
