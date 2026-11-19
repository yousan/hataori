// Package supervisor is the supervisor's side of hataori (design, section
// 7.1): the caller its tools are bound to, and `hataori mcp`, the MCP
// server that offers the control API to its agent as tools.
//
// The core has no supervisor code. To the core a supervisor is an ordinary
// limited Caller, and its thread an ordinary thread.
package supervisor

import (
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// Client is the client name of a supervisor's ids: the supervisor of scope
// S calls as the id "supervisor:S". The operator makes it a person by
// listing that id under a person of the config, and lets it act in S by
// listing that person in S's people. Without both, the core refuses its
// Start and Send like a guest's.
const Client = "supervisor"

// Mark is the first line of every input a supervisor gives a thread. The
// daemon adds it, so a worker's agent can tell the supervisor's messages
// from a person's.
const Mark = "From the supervisor of this channel:"

// ID is the id the supervisor of scope calls as.
func ID(scope string) string { return people.Join(Client, scope) }

// Caller is the caller a supervisor's connection is bound to: the scope's
// threads only; reading, starting and sending only; a send never goes into
// or interrupts a running turn; and every input marked. The daemon chooses
// it, never the client.
func Caller(scope string) control.Caller {
	return control.Caller{
		From:       ID(scope),
		Scopes:     []string{scope},
		May:        control.OpRead | control.OpStart | control.OpSend,
		RefuseBusy: true,
		Mark:       Mark,
	}
}
