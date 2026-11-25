package claudecode

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yousan/hataori/internal/agent"
)

// NewCodec returns the translator for one running session.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec {
	// A session taken in is past its start: input goes to it at once.
	return &codec{spec: spec, seen: map[string]bool{}, turns: map[string]*turnText{}, lines: map[string]bool{},
		timeout: a.inputTimeout, hookTimeout: a.hookTimeout, requests: map[string]*request{}, ready: spec.TakenIn,
		started: spec.TakenIn, session: takenSession(spec), resolve: resolveLinks}
}

// takenSession is the session a codec of a session taken in runs: known
// from the take-in, so that its end is never read as a resume that failed.
func takenSession(spec agent.Spec) string {
	if spec.TakenIn {
		return spec.ResumeID
	}
	return ""
}

// Actions is agent.Actions.
type Actions = agent.Actions

// codec turns hook calls into events. It does no I/O: it reads the lines
// the session host recorded and keeps a little state between them. Only
// resolve, given to it, looks at the file system: the links in the path of
// a confined session's write.
type codec struct {
	resolve func(string) (string, bool)

	mu      sync.Mutex
	spec    agent.Spec
	seq     uint64
	seen    map[string]bool // hook call ids already decoded
	pasted  []queued        // inputs hataori pasted that no prompt has matched yet, with normalized text
	compact int             // compactions seen, to pair PreCompact with PostCompact

	// From the transcript.
	transcript string               // the transcript being followed
	session    string               // its session id; lines of another are skipped
	current    string               // prompt id of the transcript's latest user line
	turns      map[string]*turnText // by prompt id
	closed     bool                 // SessionEnd was reported
	oddUsage   bool                 // usage of a shape the codec does not know was reported once
	lastEnded  *turnText            // the turn whose end was reported last

	// Input.
	ready         bool          // the session reported its start
	running       bool          // a turn is running
	runningPrompt string        // the prompt id of that turn
	queue         []queued      // input held until the CLI can take it
	sent          *sentInput    // input written, not yet reported
	timeout       time.Duration // for sent; DefaultInputTimeout when zero
	mcp           mcpState      // the MCP servers input waits for (mcpwait.go)
	tasks         tasks         // the background tasks that run (background.go)

	// Questions.
	requests    map[string]*request // by hook call id, while the call waits
	hookTimeout time.Duration       // after it the CLI gives up on a waiting hook
	lines       map[string]bool     // uuids of transcript lines already read

	// The screen (screen.go), from Open until the first input is reported.
	started bool        // a hook was called: the CLI is past any start-up dialog
	watch   bool        // the screen is watched: Open asked for it and the watch has not ended
	wrote   bool        // input was pasted since Open
	scr     screenState // the trust dialog, and the input box for the first input
}

// turnText is what the codec knows about the text of one prompt's turn.
// Prompt ids tie the two sources together: every hook of a turn carries
// prompt_id, and the transcript's user lines carry the same promptId.
type turnText struct {
	known       bool       // a hook named this prompt: the turn is this session's
	ended       bool       // its end was reported
	interrupted bool       // it ended with the transcript's interrupt line
	final       string     // the final answer Stop reported
	held        string     // the latest assistant text, held back: it may be the final answer
	pending     []textItem // read from the transcript before any hook named the prompt

	usage    map[string]tokens // the usage of each model message of the turn, by message id
	model    string            // the model of the latest of them
	reported agent.UsageCount  // the last Usage event produced for the turn

	// A question splits the prompt's turn in two for the core: the question
	// ends its turn, and what the CLI does after the answer is the next one.
	// Each reports its own usage only.
	asking string           // the tool use id of the question the CLI waits in, or ""
	total  agent.UsageCount // the usage read of the prompt
	base   agent.UsageCount // what of it belongs to the core's turns before the current one
}

// ask notes that the CLI waits in a question. Until it goes on, usage still
// belongs to the turn that asked: the message that asks may be read later
// than the question.
func (t *turnText) ask(toolUseID string) {
	t.goOn() // a question before it whose end no hook reported
	t.asking = toolUseID
}

// goOn notes that the CLI went on after a question: what it uses from here
// on is the next turn's. The hooks and lines read say so, never the answer
// given or a deadline, so that reading them again after a restart gives the
// same counts.
func (t *turnText) goOn() {
	if t.asking == "" {
		return
	}
	t.asking = ""
	t.base, t.reported = t.total, agent.UsageCount{Model: t.model}
}

// tokens is the usage of one model message, as the transcript gives it.
type tokens struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
}

// known says whether t has the shape the codec reads: input and output
// counts, and no count below zero.
func (t tokens) known() bool {
	if t.Input == nil || t.Output == nil {
		return false
	}
	for _, n := range []*int64{t.Input, t.Output, t.CacheRead, t.CacheWrite} {
		if n != nil && *n < 0 {
			return false
		}
	}
	return true
}

type textItem struct {
	text      string
	interrupt bool
}

// call is a hook call as the session host records it.
type call struct {
	Hook    string          `json:"hook"`
	Wait    bool            `json:"wait"`
	At      int64           `json:"at"` // when the host took the call, Unix milliseconds
	Payload json.RawMessage `json:"payload"`
}

// payload holds the fields of every hook the codec reads; a field a hook
// does not have stays empty, and fields the codec does not know are ignored.
type payload struct {
	Event          string `json:"hook_event_name"`
	TranscriptPath string `json:"transcript_path"`
	PromptID       string `json:"prompt_id"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"`
	Model          string `json:"model"`
	Effort         struct {
		Level string `json:"level"`
	} `json:"effort"`
	Prompt       string          `json:"prompt"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolResponse json.RawMessage `json:"tool_response"`
	LastMessage  string          `json:"last_assistant_message"`
	// BackgroundTasks is nil for a hook that does not have the list, as
	// StopFailure, and empty for one that says no task runs.
	BackgroundTasks []backgroundTask `json:"background_tasks"`
	Error           string           `json:"error"`
	Reason          string           `json:"reason"` // SessionEnd
	// RateLimits is what the status line handed on (statusLineEvent).
	RateLimits json.RawMessage `json:"rate_limits"`
	AgentID    string          `json:"agent_id"`
	AgentType  string          `json:"agent_type"`
}

const askTool = "AskUserQuestion"

// Open asks for the screen: for the folder-trust dialog of a workspace
// never opened before, and for the first input, which waits until the CLI
// shows it can take a paste. The watch ends once the first input is
// reported or has failed (screen.go).
func (c *codec) Open() Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spec.TakenIn {
		// A person started it, so it is past any start-up dialog: no
		// screen, and the session's start is the take-in.
		return c.number(Actions{Events: []agent.Event{{Kind: agent.SessionReady, SessionID: c.spec.ResumeID, Model: c.spec.Model}}})
	}
	c.watch = true
	return Actions{WatchScreen: true}
}

// Input records what hataori pastes, so that the prompt it produces is
// known as the caller's. Writing it to the terminal is not done here yet.
func (c *codec) Input(in agent.Input) (Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if in.Written {
		// Pasted before a restart: its prompt is still to come.
		// Nothing is written for it, Enter neither: if the restart came
		// between the paste and its Enter, it ends "not delivered". Only
		// when another turn runs meanwhile, which Enter waits for (track),
		// it is pressed after that turn.
		typed := c.pasteText(in.Text)
		text := normalize(typed)
		c.pasted = append(c.pasted, queued{id: in.ID, text: text})
		if c.sent == nil {
			c.sent = &sentInput{text: text, typed: typed, id: in.ID, pressed: true}
		}
		return Actions{}, nil
	}
	c.queue = append(c.queue, queued{id: in.ID, text: c.pasteText(in.Text)})
	return c.number(c.flush()), nil
}

// queued is an input and its Input.ID.
type queued struct{ id, text string }

// Bracketed paste: what the terminal sends around pasted text. The holder
// adds them; pasted text must not hold them.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// The keys the codec presses: Enter after a paste, Escape to interrupt.
var (
	confirm = agent.Part{Key: agent.Enter}
	stopKey = agent.Part{Key: agent.Escape}
)

// commandPrefix goes before an input of a confined session that the CLI
// would read as one of its commands: one that starts with "!" (its shell
// mode runs the rest, also under --restricted) or "/" (its own commands,
// such as /add-dir, which widens the directories the file tools may use).
// The CLI takes the input as a prompt then. A space would not do: the CLI
// trims it when it puts an interrupted prompt back in the box.
const commandPrefix = "(message) "

// atSign is what is pasted for each "@" of an input of a confined session.
// The CLI takes an "@" for a mention and puts what it names into the
// conversation: a file, an MCP server's resource, a subagent
// (testdata/2.1.332/mention_*). --restricted keeps out files outside the
// session's directory only. Which "@" it takes depends on what is before
// it, but the CLI first takes out escape sequences and characters that do
// not show, so every "@" is written so, one in a word too. A text's own
// "(at)" is not told from it.
const atSign = "(at)"

// atSignNote tells the agent of a confined session about atSign, so that
// what it passes on, shows to a person or mentions a person with has the
// "@" again.
const atSignNote = "In every message you are given, each at sign (@) is written as " + atSign + ", so that your program takes none for a mention. " +
	"In everything you write, use @ itself: where you quote such a text, where you pass it on, and where you mention a person, also when the message says whom to mention."

// pasteText is what is pasted for text: without the paste markers, and in
// a confined session, with no "@" and with commandPrefix before a command.
func (c *codec) pasteText(text string) string {
	text = cleanPaste(text)
	if c.spec.Confine == nil {
		return text
	}
	text = strings.ReplaceAll(text, "@", atSign)
	// Spaces, and characters such as a zero-width space, which the CLI
	// dropped before "/add-dir", do not count as the start.
	first := strings.TrimLeftFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) })
	if strings.HasPrefix(first, "!") || strings.HasPrefix(first, "/") {
		return commandPrefix + text
	}
	return text
}

// cleanPaste removes the bracketed-paste markers from text, so that text
// cannot end the paste early and have the rest read as keys.
func cleanPaste(text string) string {
	for strings.Contains(text, pasteEnd) || strings.Contains(text, pasteStart) {
		text = strings.ReplaceAll(strings.ReplaceAll(text, pasteEnd, ""), pasteStart, "")
	}
	return text
}

// flush writes the next queued input when the CLI can take it: after the
// session reported its start, once the screen shows the CLI takes pastes
// (screen.go) and the CLI has its MCP servers' tools (mcpwait.go), with no
// turn running and no input waiting to be reported.
// Input is one bracketed paste; Tick presses Enter after it (tickInput).
// It counts as delivered only when UserPromptSubmit reports the same text.
func (c *codec) flush() Actions {
	if !c.ready || c.running || c.sent != nil || len(c.queue) == 0 || !c.pasteReady() || !c.mcpReady() {
		return Actions{}
	}
	q := c.queue[0]
	c.queue = c.queue[1:]
	c.sent = &sentInput{text: normalize(q.text), typed: q.text, id: q.id}
	return c.paste()
}

// paste pastes c.sent. With a paste that was not handed over the CLI has
// nothing to submit, and one that may have been handed over in part is not
// submitted either: Enter would submit what a person has typed.
func (c *codec) paste() Actions {
	s := c.sent
	c.pasted = append(c.pasted, queued{id: s.id, text: s.text})
	c.wrote = true
	a := c.press(agent.Part{Paste: s.typed}, func(nothing bool) {
		if c.sent != s {
			return
		}
		s.unsure, s.refused = !nothing, nothing
		if nothing {
			c.forget(s.text)
		}
	})
	if s.id != "" {
		a.Inputs = []string{s.id}
	}
	return a
}

// enter presses Enter for c.sent. One that was not handed over is pressed on
// a later Tick: the CLI holds the text.
func (c *codec) enter(now time.Time) Actions {
	s := c.sent
	s.pressed, s.entered = true, now
	return c.press(confirm, func(nothing bool) {
		if c.sent == s && nothing {
			s.pressed, s.owed = false, true
		}
	})
}

// press is the actions that hand over p, and call failed if it cannot be.
func (c *codec) press(p agent.Part, failed func(nothing bool)) Actions {
	return Actions{Keys: []agent.Part{p}, KeysFailed: func(nothing bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		failed(nothing)
	}}
}

// forget removes text from what was pasted.
func (c *codec) forget(text string) {
	for i, p := range c.pasted {
		if p.text == text {
			c.pasted = append(c.pasted[:i], c.pasted[i+1:]...)
			return
		}
	}
}

// sentInput is input written and not yet reported by a hook.
type sentInput struct {
	text    string    // normalized, as compared with the prompt
	typed   string    // as pasted
	id      string    // its Input.ID
	at      time.Time // the first Tick after pasting it, or after a turn that ran meanwhile
	pressed bool      // Enter was pressed for it
	entered time.Time // when Enter was last pressed for it
	again   int       // times Enter was pressed again

	unsure  bool      // its paste may have been handed over in part: no Enter is pressed for it
	refused bool      // its paste was not handed over: it is pasted again
	owed    bool      // its Enter was not handed over: it is pressed again
	since   time.Time // the first Tick after its paste was first refused, or after a turn that ran meanwhile
	tries   int       // times it was refused
	next    time.Time // when it is tried again; zero until the Tick after it was refused
}

// retry reports whether what was refused may be tried again now: after a
// second, then two, up to maxRetry.
func (s *sentInput) retry(now time.Time) bool {
	if s.next.IsZero() {
		s.tries++
		s.next = now.Add(min(time.Duration(s.tries)*time.Second, maxRetry))
	}
	if now.Before(s.next) {
		return false
	}
	s.next = time.Time{}
	return true
}

// maxRetry is how long an input the window refused waits at most between
// two tries. It is short, so that its Enter follows soon after the person
// in the window has left what kept it from taking input.
const maxRetry = 5 * time.Second

// pasteSettle is how long Enter waits after the paste. An Enter that comes
// with the paste can be taken as part of it.
const pasteSettle = 400 * time.Millisecond

// Tick acts on the codec's deadlines: Enter after a paste, input the CLI
// never reported, and questions whose hook has passed its timeout.
func (c *codec) Tick(now time.Time) Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := merge(merge(merge(c.tickScreen(now), c.tickMCP(now)), c.tickInput(now)), c.tickRequests(now))
	return c.number(merge(a, c.endWatch(false)))
}

// tickInput presses Enter once the paste has had pasteSettle, presses it
// again while the input box still holds the text (screen.go), and ends the
// wait for input the CLI never reported: after the input timeout the turn
// it was to start ends as failed, "not delivered".
//
// What the window did not take is tried again: a paste until the input
// timeout, after which the turn ends as failed, NotTaken; an Enter for as
// long as it is refused, as the CLI holds the text, and would submit it
// with the next input.
//
// A turn that started between the paste and the report, the agent's own
// after a background task or one typed at the terminal, is another one:
// Enter and the timeout wait for its end, so that the timeout never ends
// it. The text stays in the CLI's input box meanwhile (2.1.331,
// testdata/2.1.331/held_notification).
func (c *codec) tickInput(now time.Time) Actions {
	if c.sent == nil {
		return merge(c.holdPaste(now), c.flush())
	}
	if c.running {
		return Actions{}
	}
	switch s := c.sent; {
	case s.refused:
		if s.since.IsZero() {
			s.since = now
		}
		if now.Sub(s.since) >= c.inputTimeout() {
			return c.unsent(agent.NotTaken)
		}
		if !c.ready || !s.retry(now) {
			return Actions{}
		}
		s.refused, s.at = false, time.Time{}
		return c.paste()
	case s.owed && c.ready:
		if !s.retry(now) {
			return Actions{}
		}
		s.owed, s.at = false, now
		return c.enter(now)
	}
	if c.sent.at.IsZero() {
		c.sent.at = now // not before the paste: it is handed over before the next Tick
		return Actions{}
	}
	if now.Sub(c.sent.at) < c.inputTimeout() {
		switch {
		case !c.ready || c.sent.unsure || now.Sub(c.sent.at) < pasteSettle:
		case !c.sent.pressed:
			return c.enter(now)
		case c.stillInBox(now):
			c.sent.again++
			return c.enter(now)
		}
		return Actions{}
	}
	c.forget(c.sent.text)
	return c.unsent(agent.NotDelivered)
}

// unsent ends the turn of c.sent, which the CLI did not get, and writes the
// next input.
func (c *codec) unsent(why agent.Reason) Actions {
	id := c.sent.id
	c.sent = nil
	a := c.events(c.tasks.at(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: why, Input: id}))
	return merge(a, c.flush())
}

func (c *codec) inputTimeout() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return DefaultInputTimeout
}

// DefaultInputTimeout is how long input may go unreported by a hook.
const DefaultInputTimeout = 30 * time.Second

// Interrupt returns the key that stops a turn. The turn counts as
// interrupted only when the transcript says so.
//
// With input still held back, for the screen or for the MCP servers, or
// whose paste was refused, there is nothing in the CLI to stop, and no
// transcript will say anything: the turn that the oldest of it was to start
// ends here, and it is not written.
//
// While an Enter is owed the CLI holds the text, and has no turn to stop: a
// later Tick presses the Enter, as the next input would be submitted with it.
func (c *codec) Interrupt() Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running && c.sent == nil && len(c.queue) > 0 {
		q := c.queue[0]
		c.queue = c.queue[1:]
		return c.number(c.events(c.tasks.at(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted, Input: q.id})))
	}
	if !c.running && c.sent != nil && c.sent.refused {
		id := c.sent.id
		c.sent = nil // a Tick writes the next
		return c.number(c.events(c.tasks.at(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted, Input: id})))
	}
	if !c.running && c.sent != nil && c.sent.owed {
		return Actions{NotInterrupted: true}
	}
	return Actions{Keys: []agent.Part{stopKey}}
}

// Decode reads one recorded line. Lines it cannot read become one Unknown
// event; it never fails.
func (c *codec) Decode(src agent.Source, line []byte) Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.decode(src, line)
	return c.number(merge(a, c.endWatch(false)))
}

func (c *codec) decode(src agent.Source, line []byte) Actions {
	switch src {
	case agent.File:
		a := c.decodeTranscript(line)
		return merge(a, c.track(c.current, a)) // the prompt of the line just read
	case agent.Screen:
		return c.screen(line)
	case agent.Hook:
	default:
		return Actions{}
	}
	var k call
	if err := json.Unmarshal(line, &k); err != nil || k.Hook == "" {
		return c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
	}
	if c.seen[k.Hook] {
		return Actions{} // the same call again
	}
	c.seen[k.Hook] = true
	var p payload
	err := json.Unmarshal(k.Payload, &p)
	if err == nil && p.Event == mcpListedEvent {
		// Not the CLI's call but its MCP server's: it says nothing of the
		// CLI's start.
		return c.mcpListed(k.Payload, line)
	}
	c.started = true // no hook runs before a start-up dialog is answered
	if err != nil {
		return c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
	}
	var follow []string
	if p.TranscriptPath != "" && p.TranscriptPath != c.transcript {
		// A start, a resume, a clear or a compaction may name a new
		// transcript. Text from before is not produced again: it belongs to
		// prompts that ended, or that no hook of this session named.
		c.transcript = p.TranscriptPath
		follow = []string{p.TranscriptPath}
	}
	if p.Event == "SessionStart" && p.SessionID != "" {
		c.session = p.SessionID
		c.closed = false // after a clear, a new session goes on in the same CLI
	}
	// Text the transcript gave for this prompt before any hook named it
	// goes after the turn's start and before anything else of the turn.
	var replay Actions
	if p.PromptID != "" {
		t := c.turn(p.PromptID)
		if !t.known {
			t.known = true
			for _, it := range t.pending {
				replay = merge(replay, c.applyText(t, it))
			}
			t.pending = nil
			replay = merge(replay, c.reportUsage(t))
		}
	}
	// Text still held when a new prompt starts or the session ends was not
	// a final answer; it would otherwise be lost.
	var released Actions
	if p.Event == "UserPromptSubmit" || p.Event == "SessionEnd" {
		ids := make([]string, 0, len(c.turns))
		for id := range c.turns {
			ids = append(ids, id)
		}
		sort.Strings(ids) // a fixed order; usually at most one turn holds text
		for _, id := range ids {
			if p.Event == "SessionEnd" || id != p.PromptID {
				released = merge(released, c.release(c.turns[id]))
			}
		}
	}
	replay = merge(released, replay)
	if p.Event == "SessionEnd" {
		c.closed = true
	}
	h := c.decodeHook(k, p)
	if p.Event == "UserPromptSubmit" {
		h = merge(h, replay)
	} else {
		h = merge(replay, h)
	}
	h = merge(h, c.track(p.PromptID, h))
	h.Follow = append(follow, h.Follow...)
	return h
}

// track keeps what input needs to know, the session's start and whether a
// turn runs, from the events just produced, and writes held input when the
// CLI can take it.
// prompt is the prompt the events belong to; the end of another prompt's
// turn (an interrupt line read late) does not end the running one.
func (c *codec) track(prompt string, a Actions) Actions {
	for _, e := range a.Events {
		switch e.Kind {
		case agent.SessionReady:
			c.ready = true
		case agent.TurnStarted:
			c.running, c.runningPrompt = true, prompt
		case agent.TurnEnded:
			if prompt == "" || c.runningPrompt == "" || prompt == c.runningPrompt {
				c.running = false
				if c.sent != nil {
					// Its wait starts now (tickInput). An Enter before that
					// turn, pressed as it started or before a restart, may
					// not have reached the box; one in an empty box does
					// nothing (2.1.331, also with a suggested prompt shown).
					c.sent.at, c.sent.since, c.sent.pressed, c.sent.owed = time.Time{}, time.Time{}, false, false
				}
			}
		case agent.SessionEnded:
			c.ready, c.running = false, false
		}
	}
	return c.flush()
}

func merge(a, b Actions) Actions {
	a.StopScreen = a.StopScreen || b.StopScreen
	a.Events = append(a.Events, b.Events...)
	a.Keys = append(a.Keys, b.Keys...)
	a.Write = append(a.Write, b.Write...)
	a.Reply = append(a.Reply, b.Reply...)
	a.Follow = append(a.Follow, b.Follow...)
	a.Inputs = append(a.Inputs, b.Inputs...)
	if f, g := a.KeysFailed, b.KeysFailed; f == nil {
		a.KeysFailed = g
	} else if g != nil {
		a.KeysFailed = func(nothing bool) { f(nothing); g(nothing) }
	}
	return a
}

func (c *codec) turn(prompt string) *turnText {
	t := c.turns[prompt]
	if t == nil {
		t = &turnText{}
		c.turns[prompt] = t
	}
	return t
}

// transcriptLine is a line of the transcript, as far as the codec reads it.
type transcriptLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	SessionID   string `json:"sessionId"`
	PromptID    string `json:"promptId"`
	IsSidechain bool   `json:"isSidechain"`
	// Operation and Content are a queue-operation line's: a message the CLI
	// queued for the agent, such as the notification of a task's end.
	Operation string          `json:"operation"`
	Content   json.RawMessage `json:"content"`
	Message   struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   json.RawMessage `json:"usage"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// interruptedLine starts the text of the user line the CLI writes when a
// turn is interrupted. No hook reports an interrupt.
const interruptedLine = "[Request interrupted by user"

// decodeTranscript reads one transcript line: assistant text of the main
// conversation becomes progress text, and the interrupt line ends the
// turn. Everything else is skipped, without an error.
func (c *codec) decodeTranscript(raw []byte) Actions {
	var l transcriptLine
	if err := json.Unmarshal(raw, &l); err != nil || l.IsSidechain {
		return Actions{}
	}
	if c.session != "" && l.SessionID != "" && l.SessionID != c.session {
		return Actions{}
	}
	if l.UUID != "" {
		if c.lines[l.UUID] {
			return Actions{} // read before, from this file or another
		}
		c.lines[l.UUID] = true
	}
	var blocks []block
	if len(l.Message.Content) > 0 && l.Message.Content[0] == '[' {
		json.Unmarshal(l.Message.Content, &blocks)
	}
	var out Actions
	switch l.Type {
	case "queue-operation":
		// A task that ends while a turn runs is told to the agent within
		// the turn, and no hook says so.
		var text string
		if l.Operation == "enqueue" && json.Unmarshal(l.Content, &text) == nil {
			c.tasks.notified(text)
		}
	case "user":
		if l.PromptID != "" {
			c.current = l.PromptID
		}
		for _, b := range blocks {
			if b.Type == "text" && strings.HasPrefix(b.Text, interruptedLine) {
				out = merge(out, c.item(textItem{interrupt: true}))
			}
		}
	case "assistant":
		for _, b := range blocks {
			switch {
			case b.Type == "text" && strings.TrimSpace(b.Text) != "":
				out = merge(out, c.item(textItem{text: b.Text}))
			case b.Type == "tool_use" && c.current != "":
				// Text before a tool call is not the final answer.
				out = merge(out, c.release(c.turn(c.current)))
			}
		}
		out = merge(out, c.usageOf(l))
	}
	return out
}

// synthetic is the model of the assistant lines the CLI writes itself, such
// as an API error, without calling a model.
const synthetic = "<synthetic>"

// usageOf keeps the usage of an assistant line for the turn of the
// current prompt, and reports the turn's totals when they changed.
//
// The CLI writes one model message as several lines, one per content
// block, each with the message's id and usage; a message counts once, with
// the usage of its latest line. Usage of a shape the codec does not know is
// skipped, and reported as one Unknown event for the whole session.
//
// The transcript gives no context window, so context use is never filled.
// Usage read after the turn's end is reported Late, and only for the turn
// that ended last before any other started (reportUsage), so that the core
// counts it toward that turn and not toward one running then.
func (c *codec) usageOf(l transcriptLine) Actions {
	if len(l.Message.Usage) == 0 || string(l.Message.Usage) == "null" || l.Message.Model == synthetic {
		return Actions{}
	}
	var u tokens
	if err := json.Unmarshal(l.Message.Usage, &u); err != nil || !u.known() {
		if c.oddUsage {
			return Actions{}
		}
		c.oddUsage = true
		return c.events(agent.Event{Kind: agent.Unknown, Text: "usage of an unknown shape: " + string(l.Message.Usage)})
	}
	if c.current == "" {
		return Actions{}
	}
	key := l.Message.ID
	if key == "" {
		key = "line:" + l.UUID
	}
	t := c.turn(c.current)
	if t.usage == nil {
		t.usage = map[string]tokens{}
	}
	t.usage[key] = u
	if l.Message.Model != "" {
		t.model = l.Message.Model
	}
	if !t.known {
		return Actions{} // reported when a hook names the prompt
	}
	return c.reportUsage(t)
}

// reportUsage produces a Usage event with the totals of t's turn, unless
// they are the ones reported last. Once the turn has ended, it reports
// Late, and only while it is the turn that ended last and no turn has
// started since: Claude Code writes a turn's last message to its
// transcript after its Stop hook (2.1.330), and before the next prompt, so
// its usage is read after the end. The core counts a Late report toward the
// session's last ended turn, which is this one at least until the agent
// reports its next prompt: a caller's input may open the core's next turn
// before that, but nothing ends it.
//
// While the CLI waits in a question it reports Late too: the question ended
// the core's turn. The answer opens the next one, and nothing ends that
// before the CLI goes on.
func (c *codec) reportUsage(t *turnText) Actions {
	if len(t.usage) == 0 || (t.ended && (t != c.lastEnded || c.running)) {
		return Actions{}
	}
	n := agent.UsageCount{Model: t.model}
	for _, u := range t.usage {
		n.Input += *u.Input
		n.Output += *u.Output
		if u.CacheRead != nil {
			n.CacheRead += *u.CacheRead
		}
		if u.CacheWrite != nil {
			n.CacheWrite += *u.CacheWrite
		}
	}
	t.total = n
	n.Input = max(0, n.Input-t.base.Input)
	n.Output = max(0, n.Output-t.base.Output)
	n.CacheRead = max(0, n.CacheRead-t.base.CacheRead)
	n.CacheWrite = max(0, n.CacheWrite-t.base.CacheWrite)
	if n == t.reported {
		return Actions{}
	}
	t.reported = n
	return c.events(agent.Event{Kind: agent.Usage, Usage: &n, Late: t.ended || t.asking != ""})
}

func (c *codec) item(it textItem) Actions {
	if c.current == "" {
		return Actions{}
	}
	t := c.turn(c.current)
	if !t.known {
		t.pending = append(t.pending, it)
		return Actions{}
	}
	return c.applyText(t, it)
}

// applyText handles text or an interrupt of a turn that is this session's.
// The latest text is held back until something follows it, because the
// final answer arrives with Stop and is posted once, as final.
func (c *codec) applyText(t *turnText, it textItem) Actions {
	if it.interrupt {
		if t.ended {
			return Actions{}
		}
		a := merge(c.withdrawAll(), c.release(t))
		t.ended, t.interrupted = true, true
		c.lastEnded = t
		// The interrupt ends the turn, not the background tasks.
		return merge(a, c.events(c.tasks.at(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted})))
	}
	if t.ended {
		// The turn's end is reported, with its final answer; text read
		// later (a lagging transcript, or one read again) is not posted.
		return Actions{}
	}
	a := c.release(t)
	if c.closed { // nothing will follow to release it
		return merge(a, c.events(agent.Event{Kind: agent.Text, Text: it.text}))
	}
	t.held = it.text
	return a
}

// release produces the held text as progress.
func (c *codec) release(t *turnText) Actions {
	if t.held == "" || !t.known {
		return Actions{}
	}
	text := t.held
	t.held = ""
	return c.events(agent.Event{Kind: agent.Text, Text: text})
}

// end marks the turn of prompt ended with final as its answer, and
// returns the held text if it was not the final answer.
func (c *codec) end(prompt, final string) Actions {
	if prompt == "" {
		return Actions{}
	}
	t := c.turn(prompt)
	var a Actions
	if t.held != "" && t.held != final {
		a = c.release(t)
	}
	t.held, t.ended, t.final = "", true, final
	c.lastEnded = t
	return a
}

func (c *codec) decodeHook(k call, p payload) Actions {
	if (p.Event == "PreToolUse" || p.Event == "PermissionRequest") && c.abandoned(p) {
		return Actions{}
	}
	switch p.Event {
	case statusLineEvent:
		if r := decodeLimits(p.RateLimits, time.UnixMilli(k.At)); r != nil {
			return c.events(agent.Event{Kind: agent.Limits, Limits: r})
		}
		return Actions{}
	case "SessionStart":
		ready := agent.Event{Kind: agent.SessionReady, SessionID: p.SessionID, Model: p.Model, Effort: p.Effort.Level}
		switch p.Source {
		case "startup":
			return c.events(ready)
		case "resume":
			if c.spec.ResumeID != "" && p.SessionID != c.spec.ResumeID {
				// The CLI resumed some other conversation than the one asked for.
				return c.events(agent.Event{Kind: agent.SessionEnded, SessionID: p.SessionID, Reason: agent.WrongSession})
			}
			return c.events(ready)
		case "clear":
			// A person cleared the conversation at the terminal: the CLI
			// goes on with a new session id, which a later resume needs.
			return c.events(ready)
		}
		return Actions{} // a compaction keeps the session id: no news to the core
	case "UserPromptSubmit":
		by, id := c.starter(p.Prompt)
		if by == agent.ByAgent {
			c.tasks.notified(p.Prompt)
		}
		started := agent.Event{Kind: agent.TurnStarted, StartedBy: by, Input: id}
		if by == agent.ByTerminal {
			// The prompt a person typed at the terminal, which no caller
			// gave: the chat shows it (design, section 5.2).
			started.Text = strings.TrimSpace(unwrapPaste(p.Prompt))
		}
		return c.events(started)
	case "Stop":
		withdrawn := c.withdrawAll()
		c.listed(p)
		return merge(merge(withdrawn, c.end(p.PromptID, p.LastMessage)), c.events(
			agent.Event{Kind: agent.Text, Text: p.LastMessage, Final: true},
			c.tasks.at(agent.Event{Kind: agent.TurnEnded, Status: agent.Completed}),
		))
	case "StopFailure":
		c.listed(p)
		return merge(merge(c.withdrawAll(), c.end(p.PromptID, "")), c.events(
			c.tasks.at(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: failureReason(p.Error), Text: p.LastMessage})))
	case "PreToolUse":
		if p.ToolName == askTool {
			if !k.Wait {
				return Actions{} // the duplicate the settings file registers; the waiting call is the question
			}
			a := c.question(k, p)
			if p.PromptID != "" && len(a.Events) == 1 && a.Events[0].Kind == agent.Request {
				c.turn(p.PromptID).ask(p.ToolUseID)
			}
			return a
		}
		// agent_id is set in the hooks of a subagent's own tools, as in
		// its SubagentStart, and absent for the main agent's.
		return c.events(agent.Event{Kind: agent.ToolStarted, ID: p.ToolUseID, Tool: p.ToolName, Parent: p.AgentID})
	case "PostToolUse", "PostToolUseFailure":
		if p.ToolName == askTool {
			if t := c.turns[p.PromptID]; t != nil && t.asking == p.ToolUseID {
				t.goOn()
			}
			return c.withdrawTool(p.ToolUseID) // answered some other way, or refused
		}
		if p.AgentID == "" && !strings.HasPrefix(p.ToolName, "mcp__") {
			// Only what the main agent starts with the CLI's own tools:
			// while a subagent runs, it is counted itself.
			c.tasks.tool(p.ToolName, p.ToolResponse)
		}
		return c.events(agent.Event{Kind: agent.ToolEnded, ID: p.ToolUseID, Tool: p.ToolName, Parent: p.AgentID})
	case "PermissionRequest":
		if c.spec.Confine != nil && k.Wait {
			if path, ok := protectedWrite(c.spec.Dir, p, c.resolve); ok {
				return Actions{Reply: []agent.HookReply{{Call: k.Hook, Body: denyWrite(path)}}}
			}
		}
		if c.spec.Approvals == agent.AllowAll && k.Wait {
			return Actions{Reply: []agent.HookReply{{Call: k.Hook, Body: allow}}}
		}
		return Actions{}
	case "SubagentStart":
		c.tasks.resumed(p.AgentID)
		return c.events(agent.Event{Kind: agent.SubagentStarted, ID: p.AgentID, Tool: p.AgentType})
	case "SubagentStop":
		if p.AgentType == "" {
			return Actions{} // an agent the CLI runs for itself, such as the compaction summary
		}
		c.tasks.end(p.AgentID)
		return c.events(agent.Event{Kind: agent.SubagentEnded, ID: p.AgentID, Tool: p.AgentType})
	case "PreCompact":
		c.compact++
		return c.events(agent.Event{Kind: agent.ToolStarted, ID: compactID(c.compact), Tool: CompactTool})
	case "PostCompact":
		return c.events(agent.Event{Kind: agent.ToolEnded, ID: compactID(c.compact), Tool: CompactTool})
	case "SessionEnd":
		end := agent.Event{Kind: agent.SessionEnded, SessionID: p.SessionID, Reason: agent.Reason(p.Reason)}
		if c.spec.ResumeID != "" && c.session == "" {
			// Asked to resume, the CLI ends without ever starting: it no
			// longer has the conversation (it says "No conversation found"
			// on its terminal, and reports only this SessionEnd).
			end.Reason = agent.CannotResume
		}
		return merge(c.withdrawAll(), c.events(end))
	}
	return c.events(agent.Event{Kind: agent.Unknown, Text: p.Event})
}

// listed takes the background tasks that run at the end of a turn from the
// hook's list. Without one, what is known from before stays.
func (c *codec) listed(p payload) {
	if p.BackgroundTasks != nil {
		c.tasks.list(p.BackgroundTasks)
	}
}

// abandoned says whether a call comes from the main agent of a turn that
// was already interrupted. The runner gives the transcript before the hook
// calls of one step, so an interrupt written just after a call can be read
// first; the CLI has given that call up, and it opens nothing: no question,
// no tool, no approval. A subagent's calls (agent_id) go on after the main
// turn's end, so they are not judged by it.
func (c *codec) abandoned(p payload) bool {
	if p.AgentID != "" || p.PromptID == "" {
		return false
	}
	t := c.turns[p.PromptID]
	return t != nil && t.interrupted
}

// CompactTool is the tool name a compaction is reported under: it is
// activity of the agent like a tool, with a start and an end.
const CompactTool = "compaction"

func compactID(n int) string { return "compaction-" + strconv.Itoa(n) }

// allow is the reply that allows a permission request.
var allow = []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`)

// writeTools are the built-in tools that write a file, with the field of
// their input that names it.
var writeTools = map[string]string{"Write": "file_path", "Edit": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}

// protectedWrite says whether a permission request of a confined session
// asks to write a file it must not: one whose path, inside the session's
// directory, has a part that starts with a dot. That holds every settings,
// git and tool configuration file a project gives the CLI (.claude/,
// .mcp.json, .git/, .gitconfig, .vscode/, ...), the files --restricted
// leaves to a person or the permission handler, and a supervisor needs
// none of them (#423); and one in docs/ at the top of the directory,
// ProtectedDocs, which holds the daemon's copies of the files the operator
// lists (#640). A path outside the directory, or one that cannot be
// read, counts too; the CLI refuses the first itself. The directory is the
// session's, or the request's cwd when that ends with the session's: the
// CLI may name the same directory with a link above it resolved, such as
// /private/var/... for /var/.... A path counts both as it is spelled and
// with its links resolved by resolve. path is the path as the request gives it.
func protectedWrite(dir string, p payload, resolve func(string) (string, bool)) (path string, ok bool) {
	field, ok := writeTools[p.ToolName]
	if !ok {
		return "", false
	}
	var in map[string]any
	json.Unmarshal(p.ToolInput, &in)
	path, _ = in[field].(string)
	if path == "" {
		return path, true
	}
	bases := []string{dir}
	if cwd, d := filepath.Clean(p.Cwd), filepath.Clean(dir); dir != "" && cwd != d && strings.HasSuffix(cwd, d) {
		bases = append(bases, cwd)
	}
	for _, base := range bases {
		if base == "" || !filepath.IsAbs(base) {
			continue
		}
		full := path
		if !filepath.IsAbs(full) {
			full = filepath.Join(base, full)
		}
		protected, inside := protectedIn(filepath.Clean(base), filepath.Clean(full))
		if !inside {
			continue
		}
		if protected {
			return path, true
		}
		// A link in the directory may lead the path where it does not say,
		// as notes/x to ../docs: it counts as where it leads.
		rbase, ok := resolve(filepath.Clean(base))
		rfull, rok := resolve(filepath.Clean(full))
		if !ok || !rok {
			return path, true
		}
		if protected, inside := protectedIn(rbase, rfull); protected || !inside {
			return path, true
		}
		return "", false
	}
	return path, true
}

// protectedIn says whether full, inside base, is a path a confined session
// may not write (see protectedWrite), and whether it is inside base.
func protectedIn(base, full string) (protected, inside bool) {
	rel, err := filepath.Rel(base, full)
	if err != nil || !filepath.IsLocal(rel) {
		return false, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	// docs/ at its top is hataori's: the daemon's copies of the files
	// the operator lists (#640). Told apart from case on a file system
	// that does not.
	if strings.EqualFold(parts[0], ProtectedDocs) {
		return true, true
	}
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true, true
		}
	}
	return false, true
}

// ProtectedDocs is the directory at the top of a confined session's own
// that the session may not write: hataori's copies of the operator's files.
const ProtectedDocs = "docs"

// denyWrite is the reply that refuses a confined session's write to path.
func denyWrite(path string) []byte {
	msg := "hataori: a confined session may not write " + strconv.Quote(path) +
		": no settings, git or tool configuration file, nor any other file or directory whose name starts with a dot, nor a file in docs/, where hataori keeps its copies of the operator's files, nor a file outside its directory"
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest",
		"decision": map[string]any{"behavior": "deny", "message": msg}}})
	return b
}

// starter tells who started a turn from its prompt: text hataori pasted is
// the caller's, and its Input.ID is returned; a task notification is the
// agent's own, and anything else was typed at the terminal.
func (c *codec) starter(prompt string) (agent.Starter, string) {
	text := unwrapPaste(prompt)
	if c.spec.Confine != nil {
		// An input may have been pasted before a restart by a hataori that
		// left its "@" (before #887).
		text = strings.ReplaceAll(text, "@", atSign)
	}
	text = normalize(text)
	for i, p := range c.pasted {
		if p.text == text {
			c.pasted = append(c.pasted[:i], c.pasted[i+1:]...)
			if c.sent != nil && c.sent.text == text {
				c.sent = nil // delivered
			}
			return agent.ByCaller, p.id
		}
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), notificationTag) {
		return agent.ByAgent, ""
	}
	return agent.ByTerminal, ""
}

// unwrapPaste returns the text inside the <pasted_content> tag the CLI
// wraps a long paste in, or the prompt as it is.
func unwrapPaste(prompt string) string {
	s := strings.TrimSpace(prompt)
	if !strings.HasPrefix(s, "<pasted_content id=") {
		return prompt
	}
	open := strings.Index(s, ">")
	if open < 0 {
		return prompt
	}
	tag := s[len("<pasted_content"):open] // ` id="29fd"`
	end := "</pasted_content" + tag + ">"
	if !strings.HasSuffix(s, end) {
		return prompt
	}
	return strings.TrimSuffix(s[open+1:], end)
}

// normalize is the text an input and a prompt are compared by: trimmed,
// with CRLF as LF, and each tab as four spaces. The CLI's input box turns a
// pasted tab into exactly four spaces, not spaces up to the next tab stop
// (2.1.328, testdata/2.1.328/paste_tabs).
func normalize(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\t", "    ")
	return strings.TrimSpace(s)
}

// failureReason maps StopFailure's error type. Only model_not_found has
// been seen from the real CLI; the others are the documented values.
func failureReason(e string) agent.Reason {
	switch e {
	case "rate_limit", "billing_error":
		return agent.Limit
	case "authentication_failed", "oauth_token_expired", "unauthorized":
		return agent.Login
	}
	return agent.APIError
}

// events returns evs as actions; Decode numbers them in the order it
// returns them.
func (c *codec) events(evs ...agent.Event) Actions {
	return Actions{Events: evs}
}

func (c *codec) number(a Actions) Actions {
	for i := range a.Events {
		c.seq++
		a.Events[i].Seq = c.seq
	}
	return a
}
