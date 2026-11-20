// Package agent declares a coding agent, as seen by the core.
//
// An adapter has two parts: a launch description, and a Codec that turns
// what the agent reports into events and does no I/O. Adapters register a
// Factory under a name; the name is used only as a key and as a path segment.
package agent

import (
	"context"
	"errors"
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

// ErrNoCodeBack is returned by LoginCode, for every code, when the login
// takes no code back: the owner only enters ShowCode on the link's page.
var ErrNoCodeBack = errors.New("agent: this login takes no code back")

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
	// A flow that takes no code back returns ErrNoCodeBack for every code, including empty.
	LoginCode(code string) ([]byte, error)
}

// LoginStep is what a line of the login command means: a link to show
// (with a code to show beside it, if the agent gives one), a request for a
// code, a refusal with its text, or nothing (the zero value). The link and
// ShowCode may arrive separately, with ShowCode after the link.
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

// CheapModel is implemented by an adapter that can name its cheapest model:
// the model of a supervisor whose config names none (design, section 7.1).
type CheapModel interface {
	CheapModel() string
}

// Launch is everything the session host needs to start an agent.
type Launch struct {
	Argv     []string
	Env      []string
	Terminal bool              // run on a pseudo-terminal instead of pipes
	Files    map[string][]byte // written into the session's state directory first
	// Confined says the launch confines the session as Spec.Confine asks.
	// A confined spec whose launch does not say so is never started, so an
	// adapter that knows nothing of confinement refuses it.
	Confined bool
}

// Source says where a line came from: Output (the agent's output on pipes),
// Hook (a call from the agent's hook command), File (a file the codec asked
// to follow) or Screen (what the agent wrote to its terminal).
type Source int

// The values of Source.
const (
	Output Source = iota
	Hook
	File
	// Screen is the agent's terminal output, the bytes as the terminal got
	// them and not cut into lines, each call carrying what came after the
	// last. It is given only to a codec whose Open asked for it
	// (Actions.WatchScreen), and only until the codec ends the watch
	// (Actions.StopScreen); after that the terminal is never read again.
	Screen
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
	Events []Event // for the core
	// Keys is the input of an agent in a terminal, in order: pastes and
	// named keys, which the terminal's holder encodes (design, section 4.3).
	Keys []Part
	// Write is the input of an agent on pipes, in order: bytes for its
	// standard input, unchanged.
	Write  [][]byte
	Reply  []HookReply // to hook calls that are waiting for an answer
	Follow []string    // files to start following, such as a transcript
	// Inputs are the Input.IDs of the inputs that Keys or Write carries,
	// whichever call produced it: the caller records them as written before
	// it hands them over.
	Inputs []string
	// WatchScreen, from Open only, asks for the terminal's output (Source
	// Screen) until the codec says StopScreen: for what no hook reports,
	// such as a start-up dialog, or whether the agent reads keys yet.
	// Elsewhere it is ignored. A codec rebuilt after a restart is not
	// opened, so it never watches.
	WatchScreen bool
	// StopScreen ends the watch, from any call: the codec needs the screen
	// no more. A codec that watches must end the watch itself; the session
	// reporting SessionReady does not.
	StopScreen bool
}

// Part is one piece of input for an agent in a terminal: a paste when Key
// is empty, else one named key.
type Part struct {
	Paste string // delivered as one bracketed paste, its bytes unchanged
	Key   Key
}

// Key is a name from a fixed table. A terminal's holder maps every name or
// refuses the whole input; it never guesses.
type Key string

// The named keys.
const (
	Enter  Key = "Enter"
	Escape Key = "Escape"
	Up     Key = "Up"
	Down   Key = "Down"
)

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
	// Confine, when set, starts the session confined (design, section
	// 7.1): no shell, no subagent and no other tool that runs a command,
	// reaches the network or wakes the agent later; file tools only in Dir;
	// and no MCP servers but Confine.MCP, none of the user's, the project's
	// or a plugin's. An adapter that cannot do all of it refuses the launch,
	// and an ordinary spec leaves it nil.
	Confine *Confinement `json:",omitempty"`
	// TrustFiles are the files a start-up dialog may say the workspace
	// loads, such as a project settings file, that an ordinary session may
	// be let past the dialog for (design, section 3.5). A dialog that names
	// any other file is not answered, and the start fails as blocked at
	// start, naming them. A confined session ignores those files, so the
	// list does not matter to it.
	TrustFiles []string `json:",omitempty"`
	// Scope is the key of the scope the session's thread belongs to, as
	// the config writes it: for what an adapter tells a person, such as
	// where TrustFiles come from. Set at each launch.
	Scope string `json:",omitempty"`
	// TakenIn is a session that a person started in their own window and
	// hataori took in (design, section 4.8): it is past its start, and runs
	// as ResumeID with Model. Its codec starts ready, as one rebuilt after a
	// restart does; Open reports SessionReady at once and watches no
	// screen. Nothing launches it.
	TakenIn bool `json:",omitempty"`
	// ConfigDir is the agent's configuration directory, when the session
	// must run with one other than the daemon's: the one the window of a
	// session taken in used, for its resume under hataori's own host.
	ConfigDir string `json:",omitempty"`
}

// Confinement is what a confined session may use besides its file tools.
type Confinement struct {
	// MCP are the session's only MCP servers, each started on stdio. They
	// may carry credentials for one launch, so the runner never stores them.
	MCP []MCPServer
}

// MCPServer is an MCP server the agent starts on stdio.
type MCPServer struct {
	Name string   // the name its tools are listed under
	Argv []string // the command and its arguments
	Env  []string // NAME=value, added to the agent's environment for it
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
	// Written says the input was written to the agent before the caller
	// restarted, and its report has not been read. The codec records it as
	// written and waiting for its report, and writes nothing.
	Written bool
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
	// Limits is the remaining usage of the agent's account. It belongs to
	// no turn and changes no state; the core keeps the newest per agent.
	Limits
	// InputTaken reports an input taken into an already running turn.
	InputTaken
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

	Text  string // Text: the text; Unknown: the line; SessionEnded before the session was ready: what a person may read about why
	Final bool   // Text: the turn's final answer

	StartedBy Starter // TurnStarted
	Input     string  // TurnStarted, InputTaken: the Input.ID of the input this turn reports; TurnEnded with NotDelivered: of the input never reported; "" when none or not known

	Tool string // ToolStarted, ToolEnded: the tool's name or label; SubagentStarted, SubagentEnded: the subagent's name or type
	ID   string // ToolStarted, ToolEnded, SubagentStarted, SubagentEnded: pairs start and end
	// Parent is, for ToolStarted and ToolEnded, the ID of the subagent the
	// tool runs in; empty for the agent's own tools.
	Parent string

	Request *RequestInfo // Request; RequestWithdrawn uses only its ID

	File string // FileOut: the path of a file the agent hands over
	// More are, for FileOut, further files handed over with File, in
	// order; Caption is the text that goes with them.
	More    []string
	Caption string

	Status     EndStatus // TurnEnded
	Reason     Reason    // TurnEnded when failed; SessionEnded: why, as the agent says or CannotResume, WrongSession, BlockedAtStart
	Background int       // TurnEnded: background tasks still running
	// Trust is, for SessionEnded with BlockedAtStart, every file a start-up
	// dialog asked to trust, as it named them, when the start was blocked
	// because the session may not trust some of them. A person may let
	// exactly these through (Spec.TrustFiles).
	Trust []string

	Usage *UsageCount // Usage: the counts the agent reports

	Limits *LimitReport // Limits: the windows the agent reports
}

// LimitReport is what a Limits event reports. At is when the agent gave
// the value, which a codec takes from the line it read, not from a clock;
// Every is how often the adapter's sessions report it while they run.
type LimitReport struct {
	At      time.Time
	Every   time.Duration
	Windows []LimitWindow
}

// LimitWindow is one usage window as the agent names it. Minutes and
// Resets are zero when the agent does not say.
type LimitWindow struct {
	Name    string
	Minutes int64
	Used    int // percent, 0 to 100
	Resets  time.Time
}

// UsageCount is what a Usage event reports: tokens of the running turn so
// far, each count the total since the turn started, and the context use.
type UsageCount struct {
	Model       string // the model the tokens were used with; "" when not said
	Input       int64
	Output      int64
	CacheRead   int64
	CacheWrite  int64
	ContextUsed int64 // tokens in the context now; with ContextSize
	ContextSize int64 // the context window; 0 when the agent does not say
}

// RequestInfo is a question or approval the agent asks.
type RequestInfo struct {
	ID       string
	Approval bool // an approval rather than a question
	Text     string
	Choices  []string
	Multi    bool // several of Choices may be chosen at once
}

// Factory makes an Adapter from its section of the instance's config,
// which is opaque to everything but the adapter, except for the keys
// "model" and "effort": hataori reads those as the defaults of the
// agent's threads and takes them out of settings.
type Factory func(settings []byte) (Adapter, error)

var adapters registry.Registry[Factory]

// Register registers an adapter's Factory under name. Registering a name twice
// is an error.
func Register(name string, f Factory) error { return adapters.Register(name, f) }

// Lookup returns the Factory registered under name.
func Lookup(name string) (Factory, bool) { return adapters.Lookup(name) }

// Names returns the names of the registered adapters.
func Names() []string { return adapters.Names() }
