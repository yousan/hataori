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
	"fmt"
	"strings"
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
	// ErrNoDashboard: a SignInLink on an instance that has no dashboard.
	// On the socket it has no code of its own, as before.
	ErrNoDashboard = errors.New("the instance has no dashboard: add a \"dashboard\" section to its config")
	// ErrEnded: a HostOperation's Send after the operation ended. Nothing
	// was sent. Since 3.11.
	ErrEnded = errors.New("the operation has ended")
	// ErrProposed: a Start or a Send of a caller with Propose was not made
	// but stored as a proposal that a person decides. The error is a
	// *Proposed. Since 3.26.
	ErrProposed = errors.New("proposed")
	// ErrReopenRefused: a ReopenThread that changed nothing, for the reason
	// its *ReopenRefused gives. Since 3.31.
	ErrReopenRefused = errors.New("reopen refused")
	// ErrNotInterrupted: the agent's window did not take the interrupt of a
	// Stop or a Send, such as while a person has scrolled back in it.
	ErrNotInterrupted = errors.New("the agent's window did not take the interrupt; the turn goes on")
)

// ReopenRefused is why a ReopenThread was refused: a sentence for a
// person, such as the open supervisor thread that keeps a closed one from
// being reopened. Nothing changed. It matches ErrReopenRefused.
type ReopenRefused struct {
	Reason string
}

func (e *ReopenRefused) Error() string { return "not reopened: " + e.Reason }

// Is matches ErrReopenRefused.
func (e *ReopenRefused) Is(target error) bool { return target == ErrReopenRefused }

// Reopened is what a ReopenThread did. Since 3.31.
type Reopened struct {
	// Workspace says what became of the thread's directory: ReopenKept,
	// ReopenRestored or ReopenRemade.
	Workspace string `json:"workspace"`
	// Conversation says what the next message does: ReopenResume, or
	// ReopenNew when the conversation cannot be resumed.
	Conversation string `json:"conversation"`
}

// The values of Reopened, and of the reopened event's fields.
const (
	ReopenKept     = "kept"     // the directory is where it was
	ReopenRestored = "restored" // the directory was taken back from trash to its place
	ReopenRemade   = "remade"   // the directory was gone, and was made again at its place
	ReopenResume   = "resume"   // the next message resumes the recorded conversation
	ReopenNew      = "new"      // the next message starts a new conversation in the directory
)

// Proposed is the error of a Start or a Send that a caller with Propose
// made: nothing ran, and the call waits as proposal Number in the caller's
// AsksIn thread for a person's decision. Since 3.26.
type Proposed struct {
	Number int64
}

func (p *Proposed) Error() string {
	return fmt.Sprintf("proposed as proposal %d: nothing runs until a person of the scope approves it", p.Number)
}

func (p *Proposed) Is(target error) bool { return target == ErrProposed }

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
	// SetLayout records how the chat service of client lays out the
	// conversations it can see: its groups, their channels and each
	// channel's open threads, in the service's order (design, section
	// 5.2). It replaces what client recorded before; nothing of it is kept
	// across a daemon restart. A change writes one layout event; the same
	// layout again changes nothing. The core sets Scope and Thread itself
	// and ignores what the call gives for them. It needs OpName in a scope
	// of the caller's whose owner is client. Since protocol 3.21.
	SetLayout(ctx context.Context, client string, groups []LayoutGroup) error
	// Layout returns the layout each chat client recorded (SetLayout), one
	// entry per client, as the caller may see it: the owner sees every
	// channel the client recorded; any other caller sees only the channels
	// that are its scopes' own channels, and only the groups that hold one
	// of them. A channel that is the own channel of a scope the caller may
	// see names it (LayoutChannel.Scope), and a thread linked to a thread
	// of a scope the caller may see names it (LayoutThread.Thread). Names
	// and URLs are text, left out for a caller that may not read text. A client that
	// recorded none has no entry. It needs OpRead. Since protocol 3.21.
	Layout(ctx context.Context) ([]ChatLayout, error)

	// Start starts a thread in a scope. A repeated r.Key starts nothing and
	// returns the thread the first call started. For a caller with Propose
	// it starts nothing and returns a *Proposed (Caller.Propose).
	Start(ctx context.Context, r StartRequest) (ThreadID, error)
	// Send gives input to a thread. key makes the call safe to repeat: a key
	// that was accepted before is ignored. busy says what to do when a turn
	// is running. For a caller with Propose it gives nothing and returns a
	// *Proposed (Caller.Propose).
	Send(ctx context.Context, id ThreadID, key string, in Input, busy Busy) error
	// Answer answers an open question, approval or proposal.
	Answer(ctx context.Context, req RequestID, a Answer) error
	// AskInterrupt asks a person to approve interrupting the running turn of
	// thread id, with reason. It stores an approval request in the caller's
	// AsksIn thread and returns its id at once; nothing is interrupted until
	// a person approves it with Answer. It needs OpAsk, the caller's person
	// allowed in id's scope, and AsksIn a thread of that scope. A thread
	// without a running turn is refused (send instead). A repeated key
	// returns the original request; an empty key makes no repeat check. An
	// open request for the same running turn, asked in the same thread, is
	// returned instead of a second one. The caller's AsksIn thread cannot be
	// the target, nor for a caller with OrdinaryOnly any confined thread
	// (ErrForbidden). Since protocol 3.10.
	AskInterrupt(ctx context.Context, id ThreadID, key string, reason string) (RequestID, error)
	// Stop interrupts the thread's running turn. A turn other than 0 names
	// the turn to stop, by the number its turn_started event gives it, and
	// is checked in the same step as the interrupt: when it is not the
	// thread's running turn, nothing is interrupted and the call is refused
	// (the wire's refused). Since protocol 3.14.
	Stop(ctx context.Context, id ThreadID, turn int64) error
	// CloseThread ends the agent process of an idle, asleep or failed thread and
	// closes it: the workspace stays, and a message does not wake it (Send
	// is ErrClosed). A thread with a turn running, a question open or a
	// session starting is refused, and so is an idle one whose last turn
	// left background tasks running and was not stopped, or that has a message or an answer the
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
	// ReopenThread takes a closed or removed thread back to asleep, so that
	// its next message wakes it (design, section 5.1): in its own
	// directory, under the same number, links and key. The recorded
	// conversation is resumed when it can be. A removed thread's directory
	// is taken back from trash when it is still there, and made again at
	// its place when it is not (a clone of the thread's repository, or an
	// empty directory for a thread started confined); a made-again
	// directory, or a session that never recorded its conversation, gets a
	// new conversation. A path that something else took is never
	// overwritten: the call is refused. So is a thread that is neither
	// closed nor removed, one whose agent was never started, a removed
	// thread taken in from a person's window, a supervisor's own thread
	// while another own thread of its scope is open (the reason names it),
	// and a look-up's thread. Each refusal is a
	// *ReopenRefused and changes nothing. A reopen writes a state event
	// (reason "reopen") and a reopened event. It needs OpReopen and the
	// owner: the owner's connection on the control socket, and a chat
	// front end's caller for the owner's press of a Reopen button. On the
	// wire it is the operation Reopen. Since 3.31.
	ReopenThread(ctx context.Context, id ThreadID) (Reopened, error)
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
	// HostView returns one tmux pane's screen, as HostScreen does, with the
	// cursor's position: for the dashboard's screen of a pane being
	// operated. Allowed as HostScreen is. Since 3.13.
	HostView(ctx context.Context, pane string) (HostView, error)

	// OperateHost lets the caller type into one tmux pane on the host that
	// no session host owns, by TmuxPane.Pane (design, section 7.2). It
	// needs OpHostInput, which the core honours only for the owner and only
	// when the config's host_input is on. A pane in which one of hataori's
	// own processes runs is refused, and a pane being operated already is
	// ErrBusy, naming since when and by whom. The start and the end are
	// logged with the person, the pane and the reason; no key ever is.
	// Since 3.11.
	OperateHost(ctx context.Context, pane string) (HostOperation, error)

	// TakeIn makes a thread of a window that a person started (design,
	// section 4.8): the window's last report (HostTerminal.Reported) must
	// be of an interactive session, the window must not be taken in
	// already, and its holder's check must pass, or nothing changes. The
	// thread is made in req.Scope as Start makes one, with its first link,
	// and its session is the window's; its workspace is the session's
	// directory, recorded as not hataori's, so no removal touches it. From
	// the window's next hook on, it reports to the thread, and the scope's
	// rules answer its permission requests. It needs OpTakeIn, which only
	// the owner's connection on the control socket has. Since 3.17.
	TakeIn(ctx context.Context, req TakeInRequest) (ThreadID, error)

	// Release ends a take-in: the thread goes asleep, its window's hooks
	// only report again, and the window is left as it is. A thread that is
	// not idle is refused. It needs OpTakeIn. Since 3.17.
	Release(ctx context.Context, id ThreadID) error

	// AttachTerminal readies the thread's agent for a person's terminal on
	// the host (`hataori attach`): an asleep thread is woken first, with no
	// input, and the result names the socket of the session host that the
	// terminal then connects to itself, so that a daemon restart does not
	// end it. It needs OpAttach, which only the owner's connection on the
	// control socket has. A closed or removed thread, and an agent with no
	// terminal, are refused and change nothing.
	AttachTerminal(ctx context.Context, id ThreadID) (Attachment, error)

	// Trust lets through the files a start-up dialog named, which blocked
	// the start of thread id's session sid (its state event's trust): sid
	// must be the thread's current session, and files exactly those. It records them as trusted for the thread's
	// scope and repository, and starts the thread again, in a new session
	// in the same workspace with the same first input (a session_replaced
	// event, reason "trusted"); a session that was woken gets the thread's
	// earlier conversation before it, as one that cannot resume does. From then on every launch in that scope on
	// that repository may answer a dialog that names no other file. It
	// needs OpTrust, and the caller's person must be the owner; anyone else
	// is refused (ErrForbidden) and nothing is recorded. A thread whose
	// current session did not fail that way is refused (the wire's
	// refused). Since 3.28.
	Trust(ctx context.Context, id ThreadID, sid int64, files []string) error
	// StartWithoutSettings starts thread id again without its repository's
	// settings, when a start-up dialog that named files blocked the start of
	// its current session sid (its state event's trust): the thread is
	// marked so that every later launch of it leaves the project's and the
	// local settings files out, with their hooks, and a new session starts
	// in the same workspace with the same first input (a session_replaced
	// event, reason "without project settings"), as Trust starts one. It is
	// checked as Start is: the caller's person must be allowed in the
	// thread's scope, and needs OpStart. A thread whose current session did
	// not fail that way is refused. Since 3.28.
	StartWithoutSettings(ctx context.Context, id ThreadID, sid int64) error
	// Trusts lists the files trusted by Trust, in the scopes the caller may
	// see, oldest first. It needs OpRead. Since 3.28.
	Trusts(ctx context.Context) ([]TrustedFile, error)
	// Untrust withdraws one file trusted by Trust, by its id: the next start
	// whose dialog names it is blocked again. A running session is not
	// changed. It needs OpTrust and the owner, as Trust does; an id that
	// is not one is ErrNotFound. Since 3.28.
	Untrust(ctx context.Context, id int64) error

	// Limits returns the remaining usage of each agent whose sessions
	// report it, as the last report said, one entry per agent by name. An
	// agent that reports none has no entry. It needs OpRead; the usage is
	// the agent's account's, not a scope's.
	Limits(ctx context.Context) ([]AgentLimits, error)

	// Clients returns the connection to its chat service of each client
	// that owns a scope the caller may see, one entry per client by name,
	// sorted by it. A client that does not report one (Connection) has no
	// entry. It needs OpRead. Since 3.22.
	Clients(ctx context.Context) ([]ClientState, error)

	// Forge makes one read of a forge, read only (design, section 7.1):
	// the issues, pull requests or checks of a repository the caller's
	// scope lists under supervisor.repos, which must have a forge name.
	// Only the daemon's supervisor client answers it, on a supervisor's
	// token connection, with the forge's token, which the caller never
	// sees; for any other repository it is ErrForbidden. The core answers
	// ErrNotSupported. Text by an author without write access whom the
	// scope does not trust is withheld. Since 3.27.
	Forge(ctx context.Context, r ForgeRequest) (ForgeAnswer, error)

	// Remind sets a reminder of the caller's scope, a wake of its supervisor
	// at a time from 5 minutes to 7 days ahead with a note of at most 1024
	// bytes, or cancels an open one by its number (design, section 7.1).
	// Anything else is refused with the reason, and one reminder more than
	// the scope's supervisor.reminders_max too. A reminder that is set counts
	// as one send of the supervisor's budget: over it, ErrForbidden. One that
	// is being given by a wake cannot be cancelled any more. Only the daemon's supervisor
	// client answers it, on a supervisor's token connection; the core answers
	// ErrNotSupported. Since 3.32.
	Remind(ctx context.Context, r RemindRequest) (RemindAnswer, error)

	// Files lists, reads, writes or edits files of the directory of the
	// caller's scope's supervisor (design, section 7.1, "No write is lost").
	// A write makes a new file, or replaces the one whose version it gives;
	// an edit replaces a passage that stands exactly once. One that does
	// not fit what is there now changes nothing and is answered with the
	// present content and version, as FilesAnswer.Refused; anything else that
	// is refused is an error, and changes nothing either. Only the
	// daemon's supervisor client answers it, on a supervisor's token
	// connection, whose scope and thread count whatever the request says;
	// the core answers ErrNotSupported. Since 3.41.
	Files(ctx context.Context, r FilesRequest) (FilesAnswer, error)
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
	// Via names the way of the daemon's own that the caller's input comes
	// through when it comes through no link, such as "dashboard" for every
	// dashboard session's Caller. The daemon sets it when it binds the
	// caller; no client sets it, and it is not on the wire. The core writes
	// it into the input, answer and approval_answered events of the
	// caller's Send and Answer and into the stop event of its Stop, and
	// names it in the log line of a refusal. Since protocol 3.23.
	Via    string
	Scopes []string // the scopes it may see and act in; a thread of another is answered as none (ErrNotFound)
	May    Ops      // any of OpRead, OpStart, OpSend, OpAnswer, OpStop, OpSignIn, OpHost, OpAttach, OpClose, OpRemove, OpName, OpAsk, OpHostInput, OpTakeIn, OpTrust, OpReopen
	// NoText leaves text written by a person or an agent out of what the
	// caller reads: ThreadInfo.Title, ThreadInfo.Last and ThreadInfo.Name,
	// ThreadDetail.Recent, the text of its requests and a question's
	// choices, a scope's name read from chat (ScopeInfo.Name, when the
	// config gives none), the names of a chat's layout (Layout), and the text fields of events (Field.Text). It
	// also refuses the reads that show a terminal, whose screen holds such
	// text: Terminal, and HostScreen and HostView of every pane, before any
	// pane is read.
	NoText bool
	// RefuseBusy makes every Send of the caller one with Refuse, whatever
	// Busy it gives: input never goes into a running turn and never
	// interrupts it. A Send with Deliver or Interrupt to a running thread is
	// refused with ErrBusy and logged.
	RefuseBusy bool
	// NoAnswer makes the caller's Send refused with ErrBusy, like one with
	// Refuse to a running thread, when the thread waits on a question,
	// instead of being its answer. The state is read in the transaction
	// that would take the input. The supervisor's waker has it, so that a
	// wake never answers the supervisor's own question.
	NoAnswer bool
	// Propose makes the caller's Start and Send proposals (design, section
	// 7.1, "Autonomy"): neither runs. Each is checked as the call would be
	// up to the point where it would run, then stored as a proposal in the
	// caller's AsksIn thread, and the call returns a *Proposed with the
	// proposal's number; a repeated key returns the same proposal. A person
	// of the scope other than the caller's person decides it with Answer.
	// Approved, the core makes the call as the caller, marked as its input,
	// once, under a key made from the proposal: a Send with Refuse that is
	// not taken as the answer to a question unless it was proposed as one,
	// to a thread waiting on that same question; a Start with the model and
	// the effort the proposal showed. The daemon sets it when it binds the
	// caller; zero AsksIn refuses every Start and Send. Since protocol 3.26.
	Propose bool
	// OrdinaryOnly refuses, with ErrForbidden, the caller's input to a
	// confined thread, whichever it is, its own included (ThreadKind): its
	// Send, whether it runs or is a proposal, its AskInterrupt of one, and
	// its Answer of a request shown in one. So a supervisor's connection
	// gives no conversation input, interrupts none and answers none. Reads
	// are as without it. The daemon sets it for a supervisor's caller, and
	// the call an approved proposal or interrupt request makes has it.
	OrdinaryOnly bool
	// Research lets the caller's Start ask for a thread that only reads,
	// without its repository's settings (StartRequest.Research): for a
	// thread of its own making, and never for one that exists. The daemon
	// sets it for a supervisor's caller. Since protocol 3.34.
	Research bool
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
	// Limit, when set, limits the caller to the operations and the event
	// kinds it names, within what May and Scopes allow: a plugin's caller
	// has the operations and event kinds its grant names (design, section
	// 7.3). Nil limits nothing.
	Limit *Limit
}

// Limit is what a Caller is limited to.
type Limit struct {
	// Operations are the operations the caller may call, by their names in
	// docs/protocol.md, such as "Threads" and "Close". Any other is refused
	// with ErrForbidden and one log line naming the operation, the scope and
	// the caller. HostKeys and End act on the operation OperateHost
	// started, and are allowed with it.
	Operations []string
	// Events are the event kinds the caller reads, through Events,
	// ThreadEvents and Thread alike; others are left out, as an event in
	// another scope is. Positions stay the instance's own: a reader that
	// resumes after the last position it read skips no event of these
	// kinds. The CaughtUp mark is delivered whatever this says.
	Events []EventKind
}

// Principal is the name of a person of the instance's config. The core
// compares principals; it never looks inside one.
type Principal string

// Ops is a set of operations a Caller may use.
type Ops uint16

// The operations of the API, as members of Ops.
const (
	OpRead   Ops = 1 << iota // Threads, Thread, ThreadEvents, Events, Workspace, Terminal, Limits, Scopes, Layout, Clients
	OpStart                  // Start
	OpSend                   // Send
	OpAnswer                 // Answer
	OpStop                   // Stop
	OpSignIn                 // SignInLink: the control socket's connections, and a chat front end's caller for a person who asked; never the dashboard
	// OpHost may list and read the terminals hataori does not own
	// (HostTerminals, HostScreen, HostView). Only the owner is given it, and only when
	// the config turns them on.
	OpHost
	// OpAttach may attach a terminal on the host to a thread's agent
	// (AttachTerminal). Only the owner's connection on the control socket
	// is given it.
	OpAttach
	OpClose  // CloseThread: the owner's connection on the control socket only
	OpRemove // RemoveThread: the owner's connection on the control socket only
	// OpName may record the names the chat service shows and how it lays
	// out its conversations (SetName, SetLayout): a chat front end's
	// caller, and the owner's connection on the control socket.
	OpName
	OpAsk // AskInterrupt
	// OpHostInput may type into a tmux pane on the host that hataori does
	// not own (OperateHost). The core honours it only for the owner, and
	// only when the config's host_input is on. Since 3.11.
	OpHostInput
	// OpTakeIn may take in a window a person started, and let it go
	// (TakeIn, Release): the owner's connection on the control socket
	// only. Since 3.17.
	OpTakeIn
	// OpTrust may let the files a start-up dialog named through for a
	// scope's repository, and withdraw that (Trust, Untrust). The core
	// honours it only for the owner. Since 3.28.
	OpTrust
	// OpReopen may reopen a closed or removed thread (ReopenThread). The
	// core honours it only for the owner: the owner's connection on the
	// control socket has it, and a chat front end gives it to the caller
	// it makes for a press of a Reopen button. Since 3.31.
	OpReopen
)

// ViaProposal is the Via of the caller as which the core makes the call of
// a proposal a person approved (Caller.Propose): the input, answer and
// thread_created events of that call name it, so that a reader can tell
// them from the proposer's own. Since 3.26.
const ViaProposal = "proposal"

// TrustedFile is one file the owner let through for a scope's threads on
// one repository (API.Trust). Since 3.28.
type TrustedFile struct {
	ID    int64     `json:"id"`
	Scope string    `json:"scope"`
	Repo  string    `json:"repo"`
	File  string    `json:"file"`
	By    Principal `json:"by"`
	At    time.Time `json:"at"`
}

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

// ThreadLink is a link of a thread: where it is shown, and who opened the
// conversation there. Since 3.40; before it, a link was its LinkRef.
type ThreadLink struct {
	LinkRef
	// Opened: the client opened the conversation, for this thread. Without
	// it the conversation was there before the link, or counts as that (a
	// link made before 3.40 that is not its thread's first one), and is not
	// hataori's to archive, rename or close. A daemon before 3.40 says it of
	// no link.
	Opened bool `json:"opened,omitempty"`
}

// TakeInRequest is the argument of API.TakeIn. Since 3.17.
type TakeInRequest struct {
	Window string `json:"window"` // the window, as HostTerminal.Reported.Window names it
	Scope  string `json:"scope"`  // the scope the thread is made in
	Key    string `json:"key"`    // makes the call safe to repeat: a second call with it returns the same thread
}

// StartRequest is the argument of API.Start.
type StartRequest struct {
	Machine string `json:"machine,omitempty"` // explicit eligible machine; no fallback

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
	// Research marks a thread that only reads: it is started, and started
	// again, without its repository's settings, as StartWithoutSettings
	// makes a thread, so that a start-up dialog that names only those files
	// never blocks it. Only a caller with Caller.Research may ask for it;
	// any other is ErrForbidden. Since 3.34.
	Research bool `json:"research,omitempty"`
}

// Input is what a client gives an agent.
type Input struct {
	Text string `json:"text"`
	// Link is the conversation the input came through, a link of the
	// thread; absent for input from outside chat. The core keeps it only
	// when it is one of the thread's links, and the input event names it,
	// so that every other link can show the input. Since 3.6.
	Link *LinkRef `json:"link,omitempty"`
	// Before and After are the text the plugins' before_prompt_build
	// handlers added before and after Text (design, section 7.3). The core
	// sets them; they are never read from a client, nor sent on the
	// socket as part of an input.
	Before string `json:"-"`
	After  string `json:"-"`
	// Via is the caller's Via (Caller.Via), which the core sets on the
	// input it hands the runner; never read from a client, nor sent on the
	// socket.
	Via string `json:"-"`
}

// ForAgent is what the agent gets of in: Before, Text and After, each
// that is not empty on lines of its own.
func (in Input) ForAgent() string {
	var parts []string
	for _, p := range []string{in.Before, in.Text, in.After} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n")
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

// ThreadKind is what a thread is to its scope (design, section 5.1): set at
// its start by the code that starts it, never by a call's content, and
// never changed. Every kind but KindOrdinary is a supervisor's, and
// confined (Confined).
type ThreadKind string

// The thread kinds, since 3.38.
const (
	KindOrdinary ThreadKind = "ordinary"
	// KindSupervisor is a scope's supervisor's own thread, which its wakes
	// go to: one of a scope is open at a time.
	KindSupervisor ThreadKind = "supervisor"
	// KindConversation is a conversation with a scope's supervisor; several
	// of a scope may be open at a time.
	KindConversation ThreadKind = "conversation"
	// KindLookup answers one question for a scope's supervisor and is never
	// reopened.
	KindLookup ThreadKind = "lookup"
)

// Confined reports whether k is a kind whose threads are started confined:
// any known kind but KindOrdinary.
func (k ThreadKind) Confined() bool {
	return k == KindSupervisor || k == KindConversation || k == KindLookup
}

// ScopeInfo is one line of Scopes. Name is empty when no name is known.
type ScopeInfo struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
}

// ChatLayout is one chat client's layout (Layout). Since 3.21.
type ChatLayout struct {
	Client string        `json:"client"` // the client's name, as in LinkRef
	Groups []LayoutGroup `json:"groups"` // in the chat service's order
}

// LayoutGroup is a group of channels in a chat service, such as a
// category; the channels in no group are a group with no id. Since 3.21.
type LayoutGroup struct {
	ID       string          `json:"id,omitempty"`   // opaque
	Name     string          `json:"name,omitempty"` // text
	Channels []LayoutChannel `json:"channels"`       // in the chat service's order
}

// LayoutChannel is one channel of a LayoutGroup. Since 3.21.
type LayoutChannel struct {
	ID   string `json:"id"`             // opaque; LinkRef.Channel
	Name string `json:"name,omitempty"` // text, such as "#general"
	URL  string `json:"url,omitempty"`  // opens the channel in the chat service's own client; text, since it may hold a name
	// Scope is the scope whose own channel this is, when the caller may
	// see that scope; set by the core.
	Scope   string         `json:"scope,omitempty"`
	Threads []LayoutThread `json:"threads"` // the open threads, in the chat service's order
}

// LayoutThread is one open thread of a LayoutChannel. Since 3.21.
type LayoutThread struct {
	ID   string `json:"id"`             // opaque; LinkRef.Thread
	Name string `json:"name,omitempty"` // text
	URL  string `json:"url,omitempty"`  // opens the thread in the chat service's own client; text, as LayoutChannel.URL
	// Thread is the number of the thread linked to this conversation, when
	// the caller may see that thread's scope; set by the core.
	Thread ThreadID `json:"thread,omitempty"`
}

// ThreadInfo is one line of Threads.
type ThreadInfo struct {
	PlacementPending bool                 `json:"placement_pending,omitempty"`
	PlacementReason  string               `json:"placement_reason,omitempty"`
	Availability     *MachineAvailability `json:"availability,omitempty"`
	Machine          string               `json:"machine,omitempty"`   // configured key, never an SSH address
	IOPaused         string               `json:"io_paused,omitempty"` // retained output cannot be read/rebuilt
	ID               ThreadID             `json:"id"`                  // the thread's number; since 3.0, it replaces number
	Scope            string               `json:"scope"`               // the scope the thread belongs to; since 3.0
	Links            []ThreadLink         `json:"links"`               // where it is shown, the first link first; since 3.0
	Title            string               `json:"title,omitempty"`     // text: absent for a caller that may not read text, since 2.0
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
	// Confined: the thread was started confined, as a scope's supervisor
	// thread is: its directory is the scope's, not a workspace with a
	// repository, so Workspace has nothing to read there. Since 3.36.
	Confined bool `json:"confined,omitempty"`
	// Kind is what the thread is to its scope; empty from a daemon of
	// before 3.38.
	Kind ThreadKind `json:"kind,omitempty"`
}

// MachineAvailability is an observation, never a session state or authority.
type MachineAvailability struct {
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
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
	Recent   string    `json:"recent"`           // the last output, cut to the size asked for at a character; its messages apart by a blank line since 3.12
	Events   []Event   `json:"events,omitempty"` // the last events asked for, newest last; since 1.5
	// Cancelled are the thread's last posts that a plugin's message_sending
	// handler cancelled, at most CancelledMax, newest last. Since 3.19.
	Cancelled []CancelledPost `json:"cancelled,omitempty"`
	// Refused are the thread's last posts that the chat service refused, at
	// most RefusedMax, newest last. Since 3.35.
	Refused []RefusedPost `json:"refused,omitempty"`
	// FailedStart is set while the thread's current session failed before
	// its first turn, or when it was woken, also once the thread was closed
	// after it. Since 3.33.
	FailedStart *FailedStart `json:"failed_start,omitempty"`
}

// FailedStart is a session that failed before its first turn, or when it
// was woken: what it was asked, who started it and why it failed. Since 3.33.
type FailedStart struct {
	Session int64    `json:"session"`
	Event   uint64   `json:"event"`            // the position of the state event of the failure
	Reason  string   `json:"reason,omitempty"` // that event's
	Detail  string   `json:"detail,omitempty"` // that event's; text
	Trust   []string `json:"trust,omitempty"`  // that event's: the files a start-up dialog named, when they blocked the start; text
	Text    string   `json:"text,omitempty"`   // what the thread was asked: its start's first input, while it had no turn; text
	// Supervisor is the supervisor thread that started the thread, when its
	// scope's supervisor did; 0 for a person's start.
	Supervisor ThreadID `json:"supervisor,omitempty"`
}

// StartChoice is what a person chose for a start that files of the
// repository blocked, as the start_chosen event says. Since 3.33.
type StartChoice string

// The choices: Trust, StartWithoutSettings, CloseThread.
const (
	StartTrusted         StartChoice = "trusted"
	StartWithoutSettings StartChoice = "without project settings"
	StartClosed          StartChoice = "closed"
)

// CancelledMax is how many cancelled posts ThreadDetail lists at most.
const CancelledMax = 20

// CancelledPost is a post, or an edit of one, that a plugin's
// message_sending handler cancelled, from its post_cancelled event: it
// stays in the outbox and was not sent. Since 3.19.
type CancelledPost struct {
	Time   time.Time `json:"time"`
	Link   LinkRef   `json:"link"`  // where it was to be posted
	Event  uint64    `json:"event"` // the position of the event it was to show
	Part   int       `json:"part"`
	Plugin string    `json:"plugin"`
	Failed bool      `json:"failed,omitempty"` // the handler failed where its policy blocks; Reason is hataori's
	Edit   bool      `json:"edit,omitempty"`   // an edit of a post, which keeps what it showed
	Reason string    `json:"reason,omitempty"` // the plugin's own, which is text, unless Failed
}

// RefusedMax is how many refused posts ThreadDetail lists at most.
const RefusedMax = 20

// RefusedPost is a post that the chat service, or the driver for it,
// refused for good, from its post_refused event: it was given up, and is
// not shown in that conversation, or not all of it. Since 3.35.
type RefusedPost struct {
	Time  time.Time `json:"time"`
	Link  LinkRef   `json:"link"`  // where it was to be posted
	Event uint64    `json:"event"` // the position of the event it was to show
	Part  int       `json:"part"`
	// Message is the chat service's id of the post's first message, when it
	// became several and a later one was refused: the ones before it stay.
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"` // the driver's error; text
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
	// Cols and Rows are the terminal's size, at which Data and what follows
	// are drawn. A chunk whose size differs from the one before it, with or
	// without Data, says the terminal was resized there.
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Data []byte `json:"data,omitempty"` // the raw bytes; text, so optional
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

// Connection is implemented by a ScopeOwner that connects to a chat
// service, so that a reader can tell when it is not connected (Clients).
type Connection interface {
	// State reports whether the client is connected now, and since when it
	// has been connected or not.
	State() (connected bool, since time.Time)
}

// ClientState is a client's connection to its chat service. Since 3.22.
type ClientState struct {
	Client    string    `json:"client"`    // the client's name, as scopes name their owner
	Connected bool      `json:"connected"` // connected now
	Since     time.Time `json:"since"`     // when it last connected, or since when it is not connected
}

// HostTerminal is a terminal on the host that hataori does not own.
// hataori shows it and never judges its state or sends it anything.
type HostTerminal struct {
	Device   string     `json:"device"`             // the terminal device, such as /dev/pts/3
	Process  string     `json:"process,omitempty"`  // the command of its foreground process
	Activity *time.Time `json:"activity,omitempty"` // its last output, when known
	Tmux     *TmuxPane  `json:"tmux,omitempty"`     // absent for a terminal outside tmux
	// Reported is what the hooks of an agent's session in this terminal
	// reported, through the plugin a person installed (design, section
	// 4.8); absent without them. Since 3.15.
	Reported *ReportedSession `json:"reported,omitempty"`
}

// ReportedSession is what the hooks of a session that a person started in
// their own window last reported. It holds no text of the session: no
// prompt, message or tool input. Since 3.15.
type ReportedSession struct {
	Window  string    `json:"window"`           // the holder and its reference to the window, such as "tmux:<socket>,<server pid>,%3"
	Agent   string    `json:"agent"`            // the adapter's name
	Session string    `json:"session"`          // the agent's session id
	Dir     string    `json:"dir"`              // the session's working directory
	Event   string    `json:"event"`            // the last hook event's name
	At      time.Time `json:"at"`               // when it was reported
	Thread  *ThreadID `json:"thread,omitempty"` // the thread whose session this is, if any
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

// HostView is a tmux pane's screen with its cursor (HostView). Since 3.13.
type HostView struct {
	Screen  string `json:"screen,omitempty"` // as HostScreen gives it; text, so optional
	CursorX int    `json:"cursor_x"`         // the cursor's column, from 0
	CursorY int    `json:"cursor_y"`         // the cursor's row, from 0 at the top of the screen
}

// HostOperation is one tmux pane being operated (OperateHost). It ends
// when the context OperateHost was given ends, when End is called, when no
// key came for Idle, when the pane goes or comes to run one of hataori's
// processes, or when the daemon stops. Done is then closed and Reason says
// which: one of the HostEnd values. Since 3.11.
type HostOperation interface {
	// Send gives the pane keys, in order. A batch that is refused, or not
	// sent because the pane's command cannot be read, changes nothing and
	// the operation goes on; once the operation has ended, Send is
	// ErrEnded.
	Send(ctx context.Context, keys []HostKey) error
	Idle() time.Duration // the config's host_input_idle
	Done() <-chan struct{}
	Reason() string // empty until Done is closed
	End()
}

// Why a HostOperation ended.
const (
	HostEnded    = "ended"         // End was called
	HostClosed   = "closed"        // the context ended: the connection or the tab closed
	HostIdle     = "idle"          // no key came for Idle
	HostGone     = "pane gone"     // tmux no longer lists the pane
	HostOwn      = "hataori's own" // one of hataori's processes runs in the pane
	HostStopping = "stopping"      // the daemon stops
)

// HostKey is one input to an operated pane: either Key, one of the tmux
// key names ValidHostKey accepts, which tmux encodes for the pane's modes;
// or Text, which goes through a tmux buffer of the operation's own and is
// never on a command line, as a paste when Paste is set. hataori logs and
// stores neither. Since 3.11.
type HostKey struct {
	Key   string `json:"key,omitempty"`
	Text  string `json:"text,omitempty"` // text, so optional
	Paste bool   `json:"paste,omitempty"`
}

// The tmux key names a HostKey's Key may be, by the modifiers tmux 3.4
// encodes for each. tmux types a name it cannot encode, such as S-Tab or
// S-a, into the pane as its letters, and drops some, such as C-Enter; those
// are refused.
var (
	// anyModKeys take any of C-, M- and S-.
	anyModKeys = map[string]bool{
		"Up": true, "Down": true, "Left": true, "Right": true, "Home": true, "End": true,
		"PPage": true, "NPage": true, "IC": true, "DC": true,
		"F1": true, "F2": true, "F3": true, "F4": true, "F5": true, "F6": true,
		"F7": true, "F8": true, "F9": true, "F10": true, "F11": true, "F12": true,
	}
	// metaKeys take M- only; Tab takes C- too.
	metaKeys = map[string]bool{"Enter": true, "BSpace": true, "Escape": true, "BTab": true, "Tab": true}
)

// ValidHostKey reports whether name is a key name hataori passes to tmux's
// send-keys: modifiers, each of C-, M- and S- at most once and in any
// order, then one of
//   - Up, Down, Left, Right, Home, End, PPage, NPage, IC, DC, F1 to F12,
//     with any modifiers;
//   - Tab, with C- or M-; Enter, BSpace, Escape, BTab, with M- only;
//   - one printable ASCII character other than a space, with M-, or with
//     C- when it is a letter or one of @ [ \ ] ^ _ ?; never with S-, and
//     never alone: a character alone is text, not a key.
func ValidHostKey(name string) bool {
	var c, m, s bool
	for len(name) > 2 && name[1] == '-' {
		var mod *bool
		switch name[0] {
		case 'C':
			mod = &c
		case 'M':
			mod = &m
		case 'S':
			mod = &s
		default:
			return false
		}
		if *mod {
			return false
		}
		*mod = true
		name = name[2:]
	}
	switch {
	case anyModKeys[name]:
		return true
	case name == "Tab":
		return !s
	case metaKeys[name]:
		return !c && !s
	case len(name) != 1 || name[0] <= ' ' || name[0] >= 0x7f || s || !c && !m:
		return false
	case c:
		ch := name[0]
		return 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || strings.IndexByte(`@[\]^_?`, ch) >= 0
	}
	return true
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
