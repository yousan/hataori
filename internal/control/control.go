// Package control declares everything a client can ask of the core.
//
// Chat front ends, the hataori command, the supervisor's tools and the
// dashboard are all clients of one API. Each API value is bound to one
// Caller, and the core checks every call against it.
package control

import (
	"context"
	"encoding/json"
	"time"
)

// API is the core's single entry point. Implementations are bound to one
// Caller and refuse any call the Caller may not make.
type API interface {
	// Threads lists the threads the caller may see that match f.
	Threads(ctx context.Context, f Filter) ([]ThreadInfo, error)
	// Thread returns state, open requests and the last output, capped at recent bytes.
	Thread(ctx context.Context, id ThreadID, recent int) (ThreadDetail, error)

	// Start starts a thread in a scope. A repeated r.Key starts nothing and
	// returns the thread the first call started.
	Start(ctx context.Context, r StartRequest) (ThreadID, error)
	// Send gives input to a thread. key makes the call safe to repeat: a key
	// that was accepted before is ignored. busy says what to do when a turn
	// is running.
	Send(ctx context.Context, id ThreadID, key string, in Input, busy Busy) error
	// Answer answers an open question or approval.
	Answer(ctx context.Context, req RequestID, a Answer) error
	// Stop interrupts the thread's running turn.
	Stop(ctx context.Context, id ThreadID) error

	// Events delivers events after a position, then live ones, until ctx ends.
	// Once the events stored when it was called are delivered, it delivers
	// one Event of kind CaughtUp, whose Pos is the last position delivered
	// (or after). The mark is not stored and belongs to no thread.
	Events(ctx context.Context, f Filter, after uint64) (<-chan Event, error)
}

// Caller is who is asking. An API value is bound to one Caller,
// and the core checks every call against it.
type Caller struct {
	Principal Principal // a person, a webhook, a supervisor, a dashboard user
	Scopes    []string  // the scopes it may see and act in
	May       Ops       // any of OpRead, OpStart, OpSend, OpAnswer, OpStop
}

// Principal is an opaque identity, given by the client that authenticated
// it. The core compares principals; it never looks inside one.
type Principal string

// Ops is a set of operations a Caller may use.
type Ops uint8

// The operations of the API, as members of Ops.
const (
	OpRead   Ops = 1 << iota // Threads, Thread, Events
	OpStart                  // Start
	OpSend                   // Send
	OpAnswer                 // Answer
	OpStop                   // Stop
)

// Has reports whether every operation in o is in ops.
func (ops Ops) Has(o Ops) bool { return ops&o == o }

// ThreadID identifies a thread by its scope and key. Both are opaque strings
// given by a client; for a chat surface the scope is a channel and the key
// is the surface's thread.
type ThreadID struct {
	Scope string
	Key   string
}

// StartRequest is the argument of API.Start.
type StartRequest struct {
	Key    string // safe to repeat: the same key starts nothing twice
	Scope  string // opaque; for chat, a channel
	Thread string // opaque key, or empty to let the scope's owner make one
	Repo   string // the repository the thread's workspace is cloned from
	Input  Input  // the first input
	Model  string // empty for the scope's default
	Effort string // empty for the scope's default
}

// Input is what a client gives an agent.
type Input struct {
	Text string
}

// Busy says what Send does when a turn is running.
type Busy int

// The values of Busy.
const (
	Refuse    Busy = iota // fail and change nothing
	Deliver               // give the input to the running turn
	Interrupt             // stop the running turn, then send
)

// RequestID identifies an open question or approval.
type RequestID string

// Answer answers a Request.
type Answer struct {
	Text    string   // a free-text answer to a question
	Choices []string // tokens of the chosen options, for a question with choices
	Allow   bool     // the decision, for an approval
}

// Filter narrows Threads and Events. A zero field matches everything the
// caller may see.
type Filter struct {
	Scope  string
	Thread ThreadID
}

// State is the state of a thread's current session (design, section 4.2).
type State string

// The session states.
const (
	Creating State = "creating"
	Idle     State = "idle"
	Running  State = "running"
	Waiting  State = "waiting" // a question or approval is open
	Asleep   State = "asleep"  // no process; the next message resumes it
	Closed   State = "closed"  // stopped by a person or by idle age; workspace kept
	Removed  State = "removed"
	Failed   State = "failed"
)

// ThreadInfo is one line of Threads.
type ThreadInfo struct {
	ID        ThreadID
	Number    int64 // short number, unique in the instance
	Title     string
	Repo      string
	State     State
	StartedBy Principal
	Created   time.Time
	Updated   time.Time
}

// ThreadDetail is the result of Thread.
type ThreadDetail struct {
	ThreadInfo
	Requests []Request // open questions and approvals
	Recent   string    // the last output, cut to the size asked for
}

// RequestKind tells a question from an approval.
type RequestKind int

// The kinds of Request.
const (
	Question RequestKind = iota
	Approval
)

// Request is an open question or approval from the agent.
type Request struct {
	ID      RequestID
	Kind    RequestKind
	Text    string
	Choices []Choice
}

// Choice is one option of a question.
type Choice struct {
	Label string
	Token string // what Answer.Choices carries back
}

// EventKind names the kind of an Event. The kinds are defined with the
// session state machine.
type EventKind string

// CaughtUp marks the end of the stored events in an Events stream.
const CaughtUp EventKind = "caught_up"

// Event is one entry of the core's event log. Pos is unique and increasing
// in the instance, so a reader can resume after the last Pos it saw.
type Event struct {
	Pos    uint64
	Thread ThreadID
	Time   time.Time
	Kind   EventKind
	Data   json.RawMessage // the fields of Kind
}

// ScopeOwner is the client that gives a scope its threads. When another client
// starts a thread there, the core asks the owner for the key; a chat front end
// opens a chat thread and returns its id.
type ScopeOwner interface {
	NewThread(ctx context.Context, scope, title string) (key string, err error)
}
