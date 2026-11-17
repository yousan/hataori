// Package agent declares a coding agent, as seen by the core.
//
// An adapter has two parts: a launch description, and a Codec that turns
// what the agent reports into events and does no I/O. Adapters register a
// Factory under a name; the name is used only as a key and as a path segment.
package agent

import (
	"context"
	"time"

	"github.com/yousan/hataori/internal/registry"
)

// Adapter is one coding agent.
type Adapter interface {
	// Probe reports whether the agent is installed and logged in.
	Probe(ctx context.Context) (Health, error)

	// Launch returns how to start the agent for spec: a new session,
	// or a resumed one when spec.ResumeID is set.
	Launch(spec Spec) (Launch, error)

	// NewCodec returns the translator for one running session.
	NewCodec(spec Spec) Codec
}

// LoginFlow is implemented by an adapter whose agent can be logged in again
// from chat. hataori runs the agent's own login command and passes on what it
// prints and the owner's code; it keeps neither. An adapter that does not
// implement it is still an Adapter; its login outages are only reported.
type LoginFlow interface {
	// LoginLaunch returns the login command, with the same config directory
	// as sessions started from spec.
	LoginLaunch(spec Spec) (Launch, error)
	// LoginDecode reads one line the login command printed.
	LoginDecode(line []byte) LoginStep
	// LoginCode returns the bytes that hand a code to the login command.
	LoginCode(code string) ([]byte, error)
}

// LoginStep is what a line of the login command means: a link to show
// (with a code to show beside it, if the agent gives one), a request for a
// code, a refusal with its text, or nothing (the zero value).
type LoginStep struct {
	Link      string // a sign-in link for the owner
	ShowCode  string // a code the owner enters on the link's page
	WantsCode bool   // the command waits for the code the page showed
	Refused   string // the command rejected a code, with its text
}

// Health is the result of Probe.
type Health struct {
	Installed bool
	LoggedIn  bool
	Version   string
	Detail    string // why not, when either is false
}

// Launch is everything the session host needs to start an agent.
type Launch struct {
	Argv     []string
	Env      []string
	Terminal bool              // run on a pseudo-terminal instead of pipes
	Files    map[string][]byte // written into the session's state directory first
}

// Source says where a line came from: Output (the agent's output on pipes),
// Hook (a call from the agent's hook command) or File (a file the codec asked
// to follow).
type Source int

// The values of Source.
const (
	Output Source = iota
	Hook
	File
)

// Codec turns what an agent reports into events, and requests into what the
// agent accepts. It does no I/O, so it is tested against captured output.
type Codec interface {
	// Open returns what to do first, if anything.
	Open() Actions
	// Decode translates one reported line.
	Decode(src Source, line []byte) Actions
	// Input translates a user message.
	Input(in Input) (Actions, error)
	// Answer translates the answer to an approval or a question.
	Answer(a Answer) (Actions, error)
	// Interrupt returns how to stop the running turn.
	Interrupt() Actions
	// Tick tells the codec the time, so that it can act on its deadlines
	// without a clock of its own. The caller calls it every second or so.
	Tick(now time.Time) Actions
}

// Actions is what the caller of a Codec does next.
type Actions struct {
	Events []Event     // for the core
	Write  [][]byte    // to the agent's input (terminal or stdin), in order
	Reply  []HookReply // to hook calls that are waiting for an answer
	Follow []string    // files to start following, such as a transcript
}

// HookReply answers one hook call that is waiting.
type HookReply struct {
	Call string // the id the session host gave the waiting call
	Body []byte // what the hook command prints
}

// Spec describes one session of an agent.
type Spec struct {
	Dir          string // workspace
	StateDir     string // settings, attachments, extra instructions; outside the workspace
	ResumeID     string
	Model        string
	Effort       string
	Instructions string // appended to the agent's own
	Approvals    Policy // allow everything, or ask in chat
}

// Policy says what happens to the agent's permission requests.
type Policy int

// The values of Policy.
const (
	AllowAll  Policy = iota // allow every request without asking
	AskInChat               // turn each request into a Request event
)

// Input is a user message for the agent.
type Input struct {
	// ID names the input for its report: the codec returns it in the
	// TurnStarted event of the turn that reports the input. It is opaque to
	// the codec, and may be empty.
	ID   string
	Text string
}

// Answer answers a Request event.
type Answer struct {
	Request string   // Event.Request.ID
	Text    string   // a free-text answer to a question
	Choices []string // chosen options, for a question with choices
	Allow   bool     // the decision, for an approval
}

// Kind is the kind of an Event.
type Kind int

// The kinds of Event.
const (
	Unknown Kind = iota
	SessionReady
	TurnStarted
	Text
	ToolStarted
	ToolEnded
	SubagentStarted
	SubagentEnded
	Request
	RequestWithdrawn
	FileOut
	Usage
	TurnEnded
	SessionEnded
)

// Starter says who started a turn.
type Starter int

// The values of Starter.
const (
	ByCaller   Starter = iota // input sent through the core
	ByAgent                   // the agent itself, after a background task
	ByTerminal                // a person at the agent's terminal
)

// EndStatus says how a turn ended.
type EndStatus int

// The values of EndStatus.
const (
	Completed EndStatus = iota
	Interrupted
	Failed
)

// Reason says why a turn failed.
type Reason string

// The reasons a turn fails.
const (
	Limit          Reason = "limit"
	Login          Reason = "login"
	APIError       Reason = "api_error"
	Crashed        Reason = "crashed"
	BlockedAtStart Reason = "blocked at start"
	NotDelivered   Reason = "not delivered"           // input the agent never reported receiving
	CannotResume   Reason = "cannot resume"           // SessionEnded: the agent no longer has the conversation
	WrongSession   Reason = "resumed another session" // SessionEnded: a resume reported another session id
)

// Event is one thing the agent did. Seq orders the events of one session.
// Each Kind uses only the fields it needs.
type Event struct {
	Seq  uint64
	Kind Kind

	SessionID string // SessionReady, SessionEnded: the agent's own session id
	Model     string // SessionReady: the model the agent reports
	Effort    string // SessionReady: the effort the agent reports

	Text  string // Text: the text; Unknown: the line
	Final bool   // Text: the turn's final answer

	StartedBy Starter // TurnStarted
	Input     string  // TurnStarted: the Input.ID of the input this turn reports, "" when none or not known

	Tool string // ToolStarted, ToolEnded: the tool's name or label; SubagentStarted, SubagentEnded: the subagent's name or type
	ID   string // ToolStarted, ToolEnded, SubagentStarted, SubagentEnded: pairs start and end
	// Parent is, for ToolStarted and ToolEnded, the ID of the subagent the
	// tool runs in; empty for the agent's own tools.
	Parent string

	Request *RequestInfo // Request; RequestWithdrawn uses only its ID

	File string // FileOut: the path of a file the agent hands over

	Status     EndStatus // TurnEnded
	Reason     Reason    // TurnEnded when failed; SessionEnded: why, as the agent says or CannotResume, WrongSession
	Background int       // TurnEnded: background tasks still running
}

// RequestInfo is a question or approval the agent asks.
type RequestInfo struct {
	ID       string
	Approval bool // an approval rather than a question
	Text     string
	Choices  []string
}

// Factory makes an Adapter from its section of the instance's config, which
// is opaque to everything but the adapter.
type Factory func(settings []byte) (Adapter, error)

var adapters registry.Registry[Factory]

// Register registers an adapter's Factory under name. Registering a name twice
// is an error.
func Register(name string, f Factory) error { return adapters.Register(name, f) }

// Lookup returns the Factory registered under name.
func Lookup(name string) (Factory, bool) { return adapters.Lookup(name) }

// Names returns the names of the registered adapters.
func Names() []string { return adapters.Names() }
