package claudecode

import (
	"fmt"

	"github.com/yousan/hataori/internal/agent"
)

var _ agent.LookUpLauncher = (*Adapter)(nil)

// LaunchLookUp deliberately returns no command until the web-search-only
// profile is checked against the real CLI and its captures are committed.
// Existing supervisor captures retain file tools and do not prove this
// boundary. In particular, do not reuse Launch, env, settings or mcpConfig:
// those may carry hooks, instructions, user settings and a daemon credential.
func (a *Adapter) LaunchLookUp(spec agent.Spec) (agent.Launch, error) {
	if err := agent.CheckLookUpSpec(spec); err != nil {
		return agent.Launch{}, err
	}
	return agent.Launch{}, fmt.Errorf("%s: %w: web-search-only tool and context isolation has not been verified against the real agent", Name, agent.ErrLookUpUnavailable)
}
