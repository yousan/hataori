// Package holder is what holds an agent's terminal (design, section 4.3):
// it starts the agent, hands it input, shows its screen, stops it and says
// whether it runs. The relay of hook calls is not part of it: hataori's
// session host keeps that job whatever holds the terminal.
//
// hataori's own session host (Host) is the only holder so far, and the one
// every later holder is measured against.
package holder

import (
	"context"
	"errors"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
)

// Holder holds agents' terminals.
type Holder interface {
	// Caps says what this holder offers: only a holder with CanStart
	// starts agents, and only one with RawScreen can hold a session whose
	// screen is watched (agent.Actions.WatchScreen). With hataori's own
	// host the only holder, the runner does not ask yet.
	Caps() Caps

	// Start starts the agent with exactly spec's arguments, environment
	// and size, in a process group of its own, once the relay for
	// stateDir listens.
	Start(ctx context.Context, stateDir string, spec Spec) (Window, error)

	// Input hands the parts to the terminal in order and returns once all
	// of them were handed over. It refuses the whole input, handing over
	// nothing, when it cannot encode one of them. Text never goes on a
	// command line.
	Input(ctx context.Context, w Window, parts []agent.Part) error

	// Screen streams the terminal from a position: raw output when
	// Caps().RawScreen, else one snapshot, marked Reset, each time it
	// changes. The channel is closed when ctx ends or the window will show
	// nothing more.
	Screen(ctx context.Context, w Window, run string, after uint64) (<-chan control.TerminalChunk, error)

	// Stop ends the agent's process group, interrupt first. A holder that
	// did not start the agent returns ErrNotOwned and does nothing.
	Stop(ctx context.Context, w Window, interrupt, terminate time.Duration) (StopResult, error)

	// Alive says whether the agent runs. Unknown is never Gone.
	Alive(ctx context.Context, w Window) (Liveness, error)
}

// Caps is what a holder offers.
type Caps struct {
	CanStart  bool // it starts agents: hataori's own host
	RawScreen bool // raw output from the first byte, with positions
}

// Spec is what Start starts.
type Spec struct {
	Argv     []string
	Env      []string // exactly the agent's environment; nothing is inherited
	Dir      string
	Terminal bool   // a terminal; else pipes, which only hataori's own host offers
	Cols     uint16 // the terminal's size; the holder's default when zero
	Rows     uint16
	// Again is set when a daemon that died had begun this start and may
	// have started the agent before it died: the window recorded for the
	// session before that start began, with Process 0 when none was. A
	// window that start made is taken instead of making another.
	Again *Window
}

// Window names one window of one holder. Ref is opaque to the core. A
// window is never named by the Ref of another window of the same holder
// that runs at the same time.
type Window struct {
	Holder string // the holder's name
	Ref    string
	Owned  bool // hataori started it
	// Process is the process that holds the window, 0 when not known: for
	// hataori's own host, the host's. The core records it with the session,
	// and lists the terminals of such processes as its own.
	Process int
}

// StopResult says what Stop did.
type StopResult = host.StopResult

// State is what Alive says.
type State int

// The values of State.
const (
	Unknown State = iota // the holder cannot tell, as before the agent was started
	Running              // the agent runs, or the holder is finishing it
	Exited               // the agent ended, and the holder recorded how
	Gone                 // the holder went away without recording an end
)

// Liveness is what Alive returns.
type Liveness struct {
	State  State
	Status *host.Status // how it ended, when the holder recorded it
}

// Ended reports whether the agent is definitely not running: it exited, or
// its holder is gone.
func (l Liveness) Ended() bool { return l.State == Exited || l.State == Gone }

// ErrNotOwned is returned by Stop for a window hataori did not start.
var ErrNotOwned = errors.New("holder: hataori did not start this window")

// ErrHeld is returned by Start when another holder process holds the state
// directory, and the start was not begun before (Spec.Again).
var ErrHeld = errors.New("holder: another host still holds the state directory")
