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
	// Thread returns state, open requests and the last output, capped at
	// recent bytes, and the thread's last events, at most events of them
	// (and at most MaxEvents), newest last.
	Thread(ctx context.Context, id ThreadID, recent, events int) (ThreadDetail, error)

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

	// Workspace reads the state of a thread's workspace and changes nothing.
	// It answers from the remote-tracking refs as last fetched; it does not
	// ask the remote.
	Workspace(ctx context.Context, id ThreadID) (WorkspaceState, error)
}

// Caller is who is asking. An API value is bound to one Caller,
// and the core checks every call against it.
type Caller struct {
	Principal Principal // a person, a webhook, a supervisor, a dashboard user
	Scopes    []string  // the scopes it may see and act in
	May       Ops       // any of OpRead, OpStart, OpSend, OpAnswer, OpStop
	// NoText leaves text written by a person or an agent out of what the
	// caller reads: ThreadInfo.Last, ThreadDetail.Recent, the text of its
	// requests, and the text fields of events (Field.Text). The thread's
	// title, taken from its first prompt, is not left out.
	NoText bool
}

// Principal is an opaque identity, given by the client that authenticated
// it. The core compares principals; it never looks inside one.
type Principal string

// Ops is a set of operations a Caller may use.
type Ops uint8

// The operations of the API, as members of Ops.
const (
	OpRead   Ops = 1 << iota // Threads, Thread, Events, Workspace
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

	// Of the thread's current session, since 1.4.
	Agent  string `json:"agent,omitempty"`  // the agent's registered name
	Model  string `json:"model,omitempty"`  // as the agent reports it; as asked for until then
	Effort string `json:"effort,omitempty"` // the same
	// Of the whole thread, since 1.4.
	Turns      int    `json:"turns,omitempty"`      // turns started in this thread
	Last       string `json:"last,omitempty"`       // the last line of output, at most LastMax bytes; text
	Background int    `json:"background,omitempty"` // background of the thread's last turn_ended event
}

// LastMax is the most bytes ThreadInfo.Last holds.
const LastMax = 200

// MaxEvents is the most events Thread returns.
const MaxEvents = 100

// ThreadDetail is the result of Thread.
type ThreadDetail struct {
	ThreadInfo
	Requests []Request `json:"requests"`         // open questions and approvals
	Recent   string    `json:"recent"`           // the last output, cut to the size asked for
	Events   []Event   `json:"events,omitempty"` // the last events asked for, newest last; since 1.4
}

// WorkspaceState is what a thread's workspace holds that may exist nowhere
// else, as numbers. It is read from the workspace's own repository.
type WorkspaceState struct {
	Branch string `json:"branch"` // the checked-out branch; empty when HEAD is detached
	Base   string `json:"base"`   // the commit the branch started from
	// Added and Removed count lines against Base: commits, staged and
	// unstaged changes of tracked files. Untracked files are counted in
	// Uncommitted only.
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Uncommitted counts files that are modified, staged or untracked, each
	// once. Ignored files are not counted.
	Uncommitted int `json:"uncommitted"`
	// Unpushed counts commits reachable from HEAD or a local branch and from
	// no remote-tracking ref.
	Unpushed int `json:"unpushed"`
	// Fetched is when the remote-tracking refs were last updated, by a
	// fetch, a push or the clone; absent when the repository does not say.
	Fetched *time.Time `json:"fetched,omitempty"`
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

// ScopeOwner is the client that gives a scope its threads. When another client
// starts a thread there, the core asks the owner for the key; a chat front end
// opens a chat thread and returns its id.
type ScopeOwner interface {
	NewThread(ctx context.Context, scope, title string) (key string, err error)
}
