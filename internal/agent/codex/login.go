package codex

import (
	"regexp"
	"strings"

	"github.com/yousan/hataori/internal/agent"
)

var _ agent.LoginFlow = (*Adapter)(nil)

// LoginLaunch starts the device login on pipes with the sessions' CODEX_HOME
// and environment.
func (a *Adapter) LoginLaunch(agent.Spec) (agent.Launch, error) {
	return agent.Launch{Argv: []string{a.command, "login", "--device-auth"}, Env: a.env()}, nil
}

var loginANSI = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\))")
var loginURL = regexp.MustCompile(`^https://[^\s]+$`)

// The captured code has four and five letters and digits.
var loginDeviceCode = regexp.MustCompile(`^[A-Z0-9]{4,}-[A-Z0-9]{4,}$`)

// LoginDecode recognizes a link, a device code, or a login error. The end
// lines come from Codex source, not a capture.
func (a *Adapter) LoginDecode(line []byte) agent.LoginStep {
	s := strings.TrimSpace(loginANSI.ReplaceAllString(string(line), ""))
	switch {
	case loginURL.MatchString(s):
		return agent.LoginStep{Link: s}
	case loginDeviceCode.MatchString(s):
		return agent.LoginStep{ShowCode: s}
	case strings.HasPrefix(s, "Error logging in:"):
		return agent.LoginStep{Refused: s}
	}
	return agent.LoginStep{}
}

// LoginCode returns ErrNoCodeBack because Codex takes no code back.
func (a *Adapter) LoginCode(string) ([]byte, error) { return nil, agent.ErrNoCodeBack }
