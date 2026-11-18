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
//
// Start, Send, Answer and Stop are refused unless the Principal is the
// owner or one of the people of the thread's scope.
type Caller struct {
	// From is the "<client>:<id>" a relayed input came from, such as a
	// chat message's author. The core resolves it to a person and uses that
	// person as the Principal, whatever Principal says; an id no person
	// lists is a guest, with no Principal. Empty for a caller the daemon
	// authenticated itself.
	From      string
	Principal Principal // a person's name
	Scopes    []string  // the scopes it may see and act in
	May       Ops       // any of OpRead, OpStart, OpSend, OpAnswer, OpStop
}

// Principal is the name of a person of the instance's config. The core
// compares principals; it never looks inside one.
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
	Scope string `json:"scope"`
	Key   string `json:"key"`
}

// StartRequest is the argument of API.Start.
type StartRequest struct {
	Key    string `json:"key"`    // safe to repeat: the same key starts nothing twice
	Scope  string `json:"scope"`  // opaque; for chat, a channel
	Thread string `json:"thread"` // opaque key, or empty to let the scope's owner make one
	Repo   string `json:"repo"`   // the repository the thread's workspace is cloned from
	Input  Input  `json:"input"`  // the first input
	Model  string `json:"model"`  // empty for the scope's default
	Effort string `json:"effort"` // empty for the scope's default
}

// Input is what a client gives an agent.
type Input struct {
	Text string `json:"text"`
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
	Text    string   `json:"text"`    // a free-text answer to a question
	Choices []string `json:"choices"` // tokens of the chosen options, for a question with choices
	Allow   bool     `json:"allow"`   // the decision, for an approval
}

// Filter narrows Threads and Events. A zero field matches everything the
// caller may see.
type Filter struct {
	Scope  string   `json:"scope"`
	Thread ThreadID `json:"thread"`
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
	ID        ThreadID  `json:"id"`
	Number    int64     `json:"number"` // short number, unique in the instance
	Title     string    `json:"title"`
	Repo      string    `json:"repo"`
	State     State     `json:"state"`
	StartedBy Principal `json:"started_by"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// ThreadDetail is the result of Thread.
type ThreadDetail struct {
	ThreadInfo
	Requests []Request `json:"requests"` // open questions and approvals
	Recent   string    `json:"recent"`   // the last output, cut to the size asked for
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
	ID      RequestID   `json:"id"`
	Kind    RequestKind `json:"kind"`
	Text    string      `json:"text"`
	Choices []Choice    `json:"choices"`
}

// Choice is one option of a question.
type Choice struct {
	Label string `json:"label"`
	Token string `json:"token"` // what Answer.Choices carries back
}

// EventKind names the kind of an Event. The kinds are defined with the
// session state machine.
type EventKind string

// CaughtUp marks the end of the stored events in an Events stream.
const CaughtUp EventKind = "caught_up"

// Event is one entry of the core's event log. Pos is unique and increasing
// in the instance, so a reader can resume after the last Pos it saw.
type Event struct {
	Pos    uint64          `json:"pos"`
	Thread ThreadID        `json:"thread"`
	Time   time.Time       `json:"time"`
	Kind   EventKind       `json:"kind"`
	Data   json.RawMessage `json:"data,omitempty"` // the fields of Kind; see EventKinds
}

// ScopeOwner is the client that gives a scope its threads. When a Start has
// no Thread, the core asks the owner for the key, after the call is
// allowed; key is the Start's key. A chat front end opens a chat thread,
// from the trigger message when key is one, and returns its id.
type ScopeOwner interface {
	NewThread(ctx context.Context, scope, key, title string) (thread string, err error)
}
