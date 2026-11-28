package codex

import "github.com/yousan/hataori/internal/agent"

var _ agent.WorkerLoginStatus = (*Adapter)(nil)

func (a *Adapter) LoginStatusLaunch(spec agent.Spec) (agent.Launch, error) {
	return agent.Launch{Argv: []string{a.command, "login", "status"}, Env: a.sessionEnv(spec)}, nil
}
