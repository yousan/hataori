// Package control declares everything a client can ask of the core.
//
// Chat front ends, the hataori command, the supervisor's tools and the
// dashboard are all clients of one API. Each API value is bound to one
// Caller, and the core checks every call against it.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Errors a client may act on; the core wraps them with details.
var (
	// ErrForbidden: the caller may not make the call, such as a guest's
	// input (design, section 5.1).
	ErrForbidden = errors.New("not allowed")
	// ErrUnknownEffort: a Start asked for an effort not in Efforts.
	ErrUnknownEffort = errors.New("unknown effort")
	// ErrUnknownAgent: a Start named an agent the instance has no adapter for. Since 3.8.
	ErrUnknownAgent = errors.New("unknown agent")
	// ErrClosed: a Send to a thread that is closed or removed. Nothing was
	// given to the agent, and a message does not wake the thread.
	ErrClosed = errors.New("the thread is closed")
	// ErrRemovalRefused: a RemoveThread whose proof did not hold (design, section
	// 4.6). The thread stays closed and its workspace is unchanged; the
	// error says why.
	ErrRemovalRefused = errors.New("removal refused")
	// ErrLinkedElsewhere: a Link of a conversation that is linked to
	// another thread. Since 3.4.
	ErrLinkedElsewhere = errors.New("the conversation shows another thread")
)

// Efforts are the effort values hataori knows. An agent adapter maps each
// to what its agent takes.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// API is the core's single entry point. Implementations are bound to one
// Caller and refuse any call the Caller may not make.
type API interface {
	// Threads lists the threads the caller may see that match f.
	Threads(ctx context.Context, f Filter) ([]ThreadInfo, error)
	// Thread returns state, open requests and the last output, capped at
	// recent bytes, and the thread's last events, at most events of them
	// (and at most MaxEvents), newest last.
	Thread(ctx context.Context, id ThreadID, recent, events int) (ThreadDetail, error)
	// ThreadEvents returns the thread's events before position before, or
	// its last ones when before is 0: at most n of them (and at most
	// MaxEvents), newest last, as Thread returns them. Asking again before
	// the oldest one returned reads the thread's log back to its start; an
	// empty result is the start. Since protocol 3.3.
	ThreadEvents(ctx context.Context, id ThreadID, before uint64, n int) ([]Event, error)
	// SetName records the name the chat service shows people for a
	// conversation: a scope's own channel when l.Thread is empty (l.Client
	// owns the scope and l.Channel is its key), else a linked thread. An
	// empty name forgets it. A change writes one named event; the same
	// name again changes nothing. It needs OpName in the conversation's
	// scope: a chat front end's caller has it. Since protocol 3.5.
	SetName(ctx context.Context, l LinkRef, name string) error
	// Scopes lists the scopes the caller may see, by key, each with the
	// name a person knows it by: the config's, else its channel's as the
	// chat service shows it (SetName), which is text and left out for a
	// caller that may not read text. It needs OpRead. Since protocol 3.5.
	Scopes(ctx context.Context) ([]ScopeInfo, error)

	// Start starts a thread in a scope. A repeated r.Key starts nothing and
	// returns the thread the first call started.
	Start(ctx context.Context, r StartRequest) (ThreadID, error)
	// Send gives input to a thread. key makes the call safe to repeat: a key
	// that was accepted before is ignored. busy says what to do when a turn
	// is running.
	Send(ctx context.Context, id ThreadID, key string, in Input, busy Busy) error
	// Answer answers an open question or approval.
	Answer(ctx context.Context, req RequestID, a Answer) error
	// AskInterrupt asks a person to approve interrupting the running turn of
	// thread id, with reason. It stores an approval request in the caller's
	// AsksIn thread and returns its id at once; nothing is interrupted until
	// a person approves it with Answer. It needs OpAsk, the caller's person
	// allowed in id's scope, and AsksIn a thread of that scope. A thread
	// without a running turn is refused (send instead). A repeated key
	// returns the original request. Since protocol 3.10.
	AskInterrupt(ctx context.Context, id ThreadID, key string, reason string) (RequestID, error)
	// Stop interrupts the thread's running turn.
	Stop(ctx context.Context, id ThreadID) error
	// CloseThread ends the agent process of an idle, asleep or failed thread and
	// closes it: the workspace stays, and a message does not wake it (Send
	// is ErrClosed). A thread with a turn running, a question open or a
	// session starting is refused, and so is an idle one whose last turn
	// left background tasks running, or that has a message or an answer the
	// core accepted and no turn has reported yet, such as a message held
	// until the turn that was running ended. A closed thread is not closed
	// again, but its agent is stopped if it still runs.
	// It is checked like Stop and needs OpClose. On the wire it is the
	// operation Close.
	CloseThread(ctx context.Context, id ThreadID) error
	// RemoveThread removes the workspace of a closed thread: only when every step
	// of the proof of design section 4.6 holds at that moment, by a rename
	// into trash, and the thread is then removed. A thread that is not
	// closed is refused; a proof that does not hold is ErrRemovalRefused,
	// with the reason, and the thread stays closed. Each removal and each
	// refusal writes one event. It is checked like Stop and needs OpRemove.
	// On the wire it is the operation Remove.
	RemoveThread(ctx context.Context, id ThreadID) error
	// Link shows the thread in one more conversation and returns its link
	// (design, section 5.2). It is checked like Send: the caller's person
	// must be allowed in the thread's scope. It is refused, with one log
	// line naming the thread, the client and the channel, when l.Channel
	// is neither the scope's own channel nor in its show_in for l.Client;
	// a refused link makes nothing. With l.Thread empty, the core asks the
	// client (its ScopeOwner) to open a conversation in l.Channel, titled
	// like the thread, only once the link is allowed; when the thread has
	// a link in that channel of that client already, it returns that one
	// and opens nothing. Linking a conversation twice to the same thread
	// changes nothing; a conversation linked to another thread is
	// ErrLinkedElsewhere. Since 3.4.
	Link(ctx context.Context, id ThreadID, l LinkRef) (LinkRef, error)

	// Events delivers events after a position, then live ones, until ctx ends.
	// Once the events stored when it was called are delivered, it delivers
	// one Event of kind CaughtUp, whose Pos is the last position delivered
	// (or after). The mark is not stored and belongs to no thread.
	Events(ctx context.Context, f Filter, after uint64) (<-chan Event, error)

	// Workspace reads the state of a thread's workspace and changes nothing.
	// It answers from the remote-tracking refs as last fetched; it does not
	// ask the remote.
	Workspace(ctx context.Context, id ThreadID) (WorkspaceState, error)

	// SignInLink makes a single-use, short-lived dashboard sign-in link for
	// the caller's person, and the session it opens belongs to that person.
	// It needs OpSignIn, which the owner's connection on the control socket
	// has, and which a chat front end gives the caller it makes for a person
	// who asked in chat; the dashboard's caller never has it. The person
	// must be one who may sign in, today the owner only. A guest is refused.
	// way names the dashboard's listener the link is for (design, section
	// 7.2): WayLocal, WayWeb, or empty for the loopback listener when the
	// instance has one and the web listener otherwise. Since 3.2.
	SignInLink(ctx context.Context, way Way) (string, error)

	// Terminal streams the terminal output of the thread's current session,
	// until ctx ends. The first chunk continues after position after of run,
	// or, when after is 0, belongs to another run of the session host or is
	// no longer kept, is the end of the kept log with Reset set. Each later
	// chunk continues the one before it, or starts again with Reset. A
	// session whose host has ended gets the end of the last run's log with
	// Reset and an empty Run, then nothing until it runs again. When the
	// thread gets a new session (one replacing a session that could not
	// resume), the stream goes on with it, from a chunk marked Reset. A
	// session without a terminal (on pipes) is ErrNotFound. It takes no input.
	Terminal(ctx context.Context, id ThreadID, run string, after uint64) (<-chan TerminalChunk, error)

	// HostTerminals lists the terminals on the host that no session host
	// owns: tmux panes, then other terminals. HostScreen returns the screen
	// of one tmux pane, by TmuxPane.Pane. Both are for display only and need
	// OpHost.
	HostTerminals(ctx context.Context) ([]HostTerminal, error)
	HostScreen(ctx context.Context, pane string) (string, error)

	// AttachTerminal readies the thread's agent for a person's terminal on
	// the host (`hataori attach`): an asleep thread is woken first, with no
	// input, and the result names the socket of the session host that the
	// terminal then connects to itself, so that a daemon restart does not
	// end it. It needs OpAttach, which only the owner's connection on the
	// control socket has. A closed or removed thread, and an agent with no
	// terminal, are refused and change nothing.
	AttachTerminal(ctx context.Context, id ThreadID) (Attachment, error)

	// Limits returns the remaining usage of each agent whose sessions
	// report it, as the last report said, one entry per agent by name. An
	// agent that reports none has no entry. It needs OpRead; the usage is
	// the agent's account's, not a scope's.
	Limits(ctx context.Context) ([]AgentLimits, error)
}

// Caller is who is asking. An API value is bound to one Caller,
// and the core checks every call against it.
//
// Start, Send, Answer, Stop, Close and Remove are refused unless the
// Principal is the owner or one of the people of the thread's scope.
type Caller struct {
	// AsksIn is where approval requests are shown. The daemon sets it when
	// binding the caller; zero forbids asking.
	AsksIn ThreadID
	// From is the "<client>:<id>" a relayed input came from, such as a
	// chat message's author. The core resolves it to a person and uses that
	// person as the Principal, whatever Principal says; an id no person
	// lists is a guest, with no Principal. Empty for a caller the daemon
	// authenticated itself.
	From      string
	Principal Principal // a person's name
	Scopes    []string  // the scopes it may see and act in
	May       Ops       // any of OpRead, OpStart, OpSend, OpAnswer, OpStop, OpSignIn, OpHost, OpAttach, OpClose, OpRemove, OpName, OpAsk
	// NoText leaves text written by a person or an agent out of what the
	// caller reads: ThreadInfo.Title and ThreadInfo.Last,
	// ThreadDetail.Recent, the text of its requests, and the text fields of
	// events (Field.Text).
	NoText bool
	// RefuseBusy makes every Send of the caller one with Refuse, whatever
	// Busy it gives: input never goes into a running turn and never
	// interrupts it. A Send with Deliver or Interrupt to a running thread is
	// refused with ErrBusy and logged.
	RefuseBusy bool
	// Mark, when set, is a line the core puts before the text of every
	// input of the caller's Start and Send, so that the agent sees who it
	// came from. The client cannot leave it out: the daemon sets it when it
	// binds the caller. A thread's title is made from the text without it,
	// and a Send that answers an open question is not marked: the agent's
	// question tool takes the answer as it is given.
	Mark string
	// ReadAsPerson checks the caller's reads like its input: the person From
	// resolves to must be allowed in a scope to read its threads, events and name.
	// A read in a scope it is not allowed in is refused and logged, as
	// Start is; a list of every scope leaves those scopes out. A chat front
	// end's caller for a person's command has it, so that a guest's status
	// command shows nothing.
	ReadAsPerson bool
}

// Principal is the name of a person of the instance's config. The core
// compares principals; it never looks inside one.
type Principal string

// Ops is a set of operations a Caller may use.
type Ops uint16

// The operations of the API, as members of Ops.
const (
	OpRead   Ops = 1 << iota // Threads, Thread, ThreadEvents, Events, Workspace, Terminal, Limits, Scopes
	OpStart                  // Start
	OpSend                   // Send
	OpAnswer                 // Answer
	OpStop                   // Stop
	OpSignIn                 // SignInLink: the control socket's connections, and a chat front end's caller for a person who asked; never the dashboard
	// OpHost may list and read the terminals hataori does not own
	// (HostTerminals, HostScreen). Only the owner is given it, and only when
	// the config turns them on.
	OpHost
	// OpAttach may attach a terminal on the host to a thread's agent
	// (AttachTerminal). Only the owner's connection on the control socket
	// is given it.
	OpAttach
	OpClose  // CloseThread: the owner's connection on the control socket only
	OpRemove // RemoveThread: the owner's connection on the control socket only
	// OpName may record the names the chat service shows (SetName): a chat
	// front end's caller, and the owner's connection on the control socket.
	OpName
	OpAsk // AskInterrupt
)

// Way is one of the dashboard's listeners, each a way of reaching it with
// its own proof of who is at the other end (design, section 7.2). Since 3.2.
type Way string

const (
	// WayLocal is the loopback listener: a browser on the host, or one on
	// another machine through a port forwarded over SSH.
	WayLocal Way = "local"
	// WayWeb is the web listener, over HTTPS with a stable name.
	WayWeb Way = "web"
)

// Has reports whether every operation in o is in ops.
func (ops Ops) Has(o Ops) bool { return ops&o == o }

// ThreadID is a thread's number, unique in the instance (threads.id). It
// names the thread in every operation and event; a chat conversation is a
// link to it (LinkRef). Since 3.0; 0 names no thread.
type ThreadID int64

// LinkRef is where a thread is shown: one conversation of one client. For
// a chat surface, the channel and the thread on that service.
type LinkRef struct {
	Client  string `json:"client"`  // the client's name, such as a chat front end's
	Channel string `json:"channel"` // opaque; for chat, a channel
	Thread  string `json:"thread"`  // opaque; for chat, the service's thread
}

// StartRequest is the argument of API.Start.
type StartRequest struct {
	Key   string `json:"key"`   // safe to repeat: the same key starts nothing twice
	Scope string `json:"scope"` // opaque; for chat, a channel
	// Thread makes the first link itself: the conversation Thread in the
	// scope's own channel, of the scope's owner. Empty to let the owner
	// open one (ScopeOwner.NewThread). Since 3.0 it names a link, not the
	// thread.
	Thread string `json:"thread"`
	Agent  string `json:"agent,omitempty"` // registered agent name; empty for the scope's agent; since 3.8
	Repo   string `json:"repo"`            // the repository the thread's workspace is cloned from
	Input  Input  `json:"input"`           // the first input
	Model  string `json:"model"`           // empty for the scope's default
	Effort string `json:"effort"`          // empty for the scope's default
}

// Input is what a client gives an agent.
type Input struct {
	Text string `json:"text"`
	// Link is the conversation the input came through, a link of the
	// thread; absent for input from outside chat. The core keeps it only
	// when it is one of the thread's links, and the input event names it,
	// so that every other link can show the input. Since 3.6.
	Link *LinkRef `json:"link,omitempty"`
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
	// Link is the conversation the answer came through, as Input.Link.
	// Since 3.6.
	Link *LinkRef `json:"link,omitempty"`
}

// Filter narrows Threads and Events. A zero field matches everything the
// caller may see.
type Filter struct {
	Scope  string   `json:"scope"`
	Thread ThreadID `json:"thread"` // the thread's number; 0 for any
	// Link matches the one thread linked to the conversation, or none.
	// Since 3.0.
	Link *LinkRef `json:"link,omitempty"`
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

// ScopeInfo is one line of Scopes. Name is empty when no name is known.
type ScopeInfo struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
}

// ThreadInfo is one line of Threads.
type ThreadInfo struct {
	ID    ThreadID  `json:"id"`              // the thread's number; since 3.0, it replaces number
	Scope string    `json:"scope"`           // the scope the thread belongs to; since 3.0
	Links []LinkRef `json:"links"`           // where it is shown, the first link first; since 3.0
	Title string    `json:"title,omitempty"` // text: absent for a caller that may not read text, since 2.0
	// Name is the chat thread's own name as the chat service shows it, of
	// the thread's first link (SetName), which follows a rename; text,
	// absent for a caller that may not read text, and when none is known.
	// Since 3.7.
	Name      string    `json:"name,omitempty"`
	Repo      string    `json:"repo"`
	State     State     `json:"state"`
	StartedBy Principal `json:"started_by"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`

	// Of the thread's current session, since 1.5.
	Agent  string `json:"agent,omitempty"`  // the agent's registered name
	Model  string `json:"model,omitempty"`  // as the agent reports it; as asked for until then
	Effort string `json:"effort,omitempty"` // the same
	// Of the whole thread, since 1.5.
	Turns      int    `json:"turns,omitempty"`      // turns started in this thread
	Last       string `json:"last,omitempty"`       // the last line of output, at most LastMax bytes; text
	Background int    `json:"background,omitempty"` // background of the thread's last turn_ended event
	// From the usage events, since 1.8.
	Context *Context    `json:"context,omitempty"` // the last context use reported; absent until one is
	Tokens  *TokenCount `json:"tokens,omitempty"`  // absent until the agent reports usage
	// Of the thread's current session, since 1.12: the terminal its agent
	// runs in, such as /dev/pts/3; absent on pipes and while no process
	// runs.
	Terminal string `json:"terminal,omitempty"`
}

// Attachment is the result of AttachTerminal.
type Attachment struct {
	// Socket is the path of the Unix socket of the thread's session host,
	// which a terminal on the host connects to. It is not text.
	Socket string `json:"socket"`
	Number int64  `json:"number"` // the thread's number
}

// Context is a session's context use, in tokens.
type Context struct {
	Used int64 `json:"used"`
	Size int64 `json:"size"` // the context window
}

// TokenCount counts tokens: input, output, cache reads and cache writes
// together.
type TokenCount struct {
	Turn  int64 `json:"turn"`  // of the last turn that reported usage
	Today int64 `json:"today"` // of every turn today, in the daemon's local time zone
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
	Events   []Event   `json:"events,omitempty"` // the last events asked for, newest last; since 1.5
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

// TerminalChunk is one piece of a session's terminal output. Positions
// count the bytes the session host wrote since its run started.
type TerminalChunk struct {
	Run   string `json:"run"`   // the session host's run; a new run starts a new count; empty when no host runs
	Pos   uint64 `json:"pos"`   // the position after Data; 0 when Run is empty
	Reset bool   `json:"reset"` // Data is the end of the log, not a continuation: redraw
	Cols  int    `json:"cols"`  // the terminal's size
	Rows  int    `json:"rows"`
	Data  []byte `json:"data,omitempty"` // the raw bytes; text, so optional
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
	Thread ThreadID        `json:"thread,omitempty"` // the thread's number; absent for an event of no thread
	Scope  string          `json:"scope,omitempty"`  // the thread's scope; since 3.0
	Time   time.Time       `json:"time"`
	Kind   EventKind       `json:"kind"`
	Data   json.RawMessage `json:"data,omitempty"` // the fields of Kind; see EventKinds
}

// ScopeOwner is the client that gives a scope its threads. The core asks
// the owner for a new thread's first link, after the Start is allowed; key
// is the Start's key. A chat front end opens a chat thread, from the
// trigger message when key is one, and returns where. The core stores the
// link with the thread, in the same transaction.
type ScopeOwner interface {
	NewThread(ctx context.Context, scope, key, title string) (LinkRef, error)
}

// HostTerminal is a terminal on the host that hataori does not own.
// hataori shows it and never judges its state or sends it anything.
type HostTerminal struct {
	Device   string     `json:"device"`             // the terminal device, such as /dev/pts/3
	Process  string     `json:"process,omitempty"`  // the command of its foreground process
	Activity *time.Time `json:"activity,omitempty"` // its last output, when known
	Tmux     *TmuxPane  `json:"tmux,omitempty"`     // absent for a terminal outside tmux
}

// TmuxPane is where a HostTerminal is in tmux.
type TmuxPane struct {
	Pane       string `json:"pane"`                  // the pane's id, such as %3; what HostScreen takes
	Session    string `json:"session"`               // the session's name
	Window     string `json:"window"`                // the window's index
	WindowName string `json:"window_name,omitempty"` // the window's name
	Index      string `json:"index"`                 // the pane's index in its window
	Dir        string `json:"dir,omitempty"`         // the pane's current directory
	Attached   bool   `json:"attached"`              // a client shows the session now
}

// AgentLimits is an agent's remaining usage, as one of its sessions last
// reported it. At is when that report was made. Every is how often the
// agent's sessions report it while one runs: a report older than Every is
// not current, so a reader shows it with its time.
type AgentLimits struct {
	Agent   string        `json:"agent"`   // the agent's registered name
	At      time.Time     `json:"at"`      // when it was reported
	Every   int64         `json:"every"`   // seconds
	Windows []LimitWindow `json:"windows"` // as the agent reports them, in its order
}

// LimitWindow is one usage window of an agent.
type LimitWindow struct {
	Name    string     `json:"name"`              // the agent's name for it, such as seven_day or primary
	Minutes int64      `json:"minutes,omitempty"` // its length; absent when the agent does not say
	Used    int        `json:"used"`              // the share used, in percent: 0 to 100
	Resets  *time.Time `json:"resets,omitempty"`  // when it starts again; absent when the agent does not say
}
