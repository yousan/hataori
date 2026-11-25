// Package supervisor is the supervisor's side of hataori (design, section
// 7.1): the caller its tools are bound to, and `hataori mcp`, the MCP
// server that offers the control API to its agent as tools.
//
// The core has no supervisor code. To the core a supervisor is an ordinary
// limited Caller, and its thread an ordinary thread.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/instance"
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

// The supervisor's autonomy in a scope (design, section 7.1, "Autonomy"),
// as the config names it: scopes[].supervisor.autonomy.
const (
	Watch   = instance.AutonomyWatch
	Propose = instance.AutonomyPropose
	Act     = instance.AutonomyAct
)

// Caller is the caller a supervisor's connection is bound to: the scope's
// threads only; reading, and as autonomy allows, starting, sending and
// asking a person to approve an interrupt; a send never goes into or
// interrupts a running turn; neither it nor an interrupt is for a confined
// thread, thread itself included; and every input marked. Under Watch it
// reads only; under Propose its starts and sends are proposals shown in
// thread (control.Caller.Propose); under Act they run. Any other autonomy,
// as none, is Propose. The daemon chooses it, never the client.
func Caller(scope string, thread control.ThreadID, autonomy string) control.Caller {
	c := control.Caller{
		From:       ID(scope),
		AsksIn:     thread,
		Scopes:     []string{scope},
		May:        control.OpRead | control.OpStart | control.OpSend | control.OpAsk,
		RefuseBusy: true,
		// Whatever the autonomy: no conversation orders another, or the
		// supervisor's own thread, and neither answers for a person (#871).
		OrdinaryOnly: true,
		Mark:         Mark,
		Research:     true, // of its own starts only (#681)
	}
	switch autonomy {
	case Watch:
		c.May, c.Research = control.OpRead, false
	case Act:
	default:
		c.Propose = true
	}
	return c
}

// watching is the API of a supervisor whose autonomy is Watch: its Start,
// Send and AskInterrupt are refused at the call, with a reason that names
// the mode. Its Caller has none of them either (Caller), so the core
// refuses them too.
type watching struct {
	control.API
	scope string
	log   *slog.Logger
}

func (w *watching) refuse(op string) error {
	w.log.Warn("control call refused", "op", op, "scope", w.scope, "id", ID(w.scope), "reason", "the supervisor's autonomy is watch")
	return fmt.Errorf("the supervisor's autonomy in this channel is %q: it reads threads and tells people in its own thread, "+
		"and does not start, send or ask to interrupt: %w", Watch, control.ErrForbidden)
}

func (w *watching) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	return 0, w.refuse("Start")
}

func (w *watching) Send(ctx context.Context, id control.ThreadID, key string, in control.Input, busy control.Busy) error {
	return w.refuse("Send")
}

func (w *watching) AskInterrupt(ctx context.Context, id control.ThreadID, key, reason string) (control.RequestID, error) {
	return "", w.refuse("AskInterrupt")
}
