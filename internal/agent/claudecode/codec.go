package claudecode

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// NewCodec returns the translator for one running session.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec {
	return &codec{spec: spec, seen: map[string]bool{}, turns: map[string]*turnText{}, lines: map[string]bool{},
		timeout: a.inputTimeout, hookTimeout: a.hookTimeout, requests: map[string]*request{}}
}

// Actions is agent.Actions.
type Actions = agent.Actions

// codec turns hook calls into events. It does no I/O: it reads the lines
// the session host recorded and keeps a little state between them.
type codec struct {
	mu      sync.Mutex
	spec    agent.Spec
	seq     uint64
	seen    map[string]bool // hook call ids already decoded
	pasted  []string        // inputs hataori pasted that no prompt has matched yet
	compact int             // compactions seen, to pair PreCompact with PostCompact

	// From the transcript.
	transcript string               // the transcript being followed
	session    string               // its session id; lines of another are skipped
	current    string               // prompt id of the transcript's latest user line
	turns      map[string]*turnText // by prompt id
	closed     bool                 // SessionEnd was reported

	// Input.
	ready         bool          // the session reported its start
	running       bool          // a turn is running
	runningPrompt string        // the prompt id of that turn
	queue         []string      // input held until the CLI can take it
	sent          *sentInput    // input written, not yet reported
	timeout       time.Duration // for sent; DefaultInputTimeout when zero

	// Questions.
	requests    map[string]*request // by hook call id, while the call waits
	hookTimeout time.Duration       // after it the CLI gives up on a waiting hook
	lines       map[string]bool     // uuids of transcript lines already read
}

// turnText is what the codec knows about the text of one prompt's turn.
// Prompt ids tie the two sources together: every hook of a turn carries
// prompt_id, and the transcript's user lines carry the same promptId.
type turnText struct {
	known   bool       // a hook named this prompt: the turn is this session's
	ended   bool       // its end was reported
	final   string     // the final answer Stop reported
	held    string     // the latest assistant text, held back: it may be the final answer
	pending []textItem // read from the transcript before any hook named the prompt
}

type textItem struct {
	text      string
	interrupt bool
}

// call is a hook call as the session host records it.
type call struct {
	Hook    string          `json:"hook"`
	Wait    bool            `json:"wait"`
	Payload json.RawMessage `json:"payload"`
}

// payload holds the fields of every hook the codec reads; a field a hook
// does not have stays empty, and fields the codec does not know are ignored.
type payload struct {
	Event          string `json:"hook_event_name"`
	TranscriptPath string `json:"transcript_path"`
	PromptID       string `json:"prompt_id"`
	SessionID      string `json:"session_id"`
	Source         string `json:"source"`
	Model          string `json:"model"`
	Effort         struct {
		Level string `json:"level"`
	} `json:"effort"`
	Prompt          string          `json:"prompt"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	ToolUseID       string          `json:"tool_use_id"`
	LastMessage     string          `json:"last_assistant_message"`
	BackgroundTasks []struct {
		Status string `json:"status"`
	} `json:"background_tasks"`
	Error     string `json:"error"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

const askTool = "AskUserQuestion"

func (c *codec) Open() Actions { return Actions{} }

// Input records what hataori pastes, so that the prompt it produces is
// known as the caller's. Writing it to the terminal is not done here yet.
func (c *codec) Input(in agent.Input) (Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(c.queue, cleanPaste(in.Text))
	return c.number(c.flush()), nil
}

// Bracketed paste: what the terminal sends around pasted text.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
	confirm    = "\r"   // Enter
	stopKey    = "\x1b" // Escape
)

// cleanPaste removes the bracketed-paste markers from text, so that text
// cannot end the paste early and have the rest read as keys.
func cleanPaste(text string) string {
	for strings.Contains(text, pasteEnd) || strings.Contains(text, pasteStart) {
		text = strings.ReplaceAll(strings.ReplaceAll(text, pasteEnd, ""), pasteStart, "")
	}
	return text
}

// flush writes the next queued input when the CLI can take it: after the
// session reported its start, with no turn running and no input waiting to
// be reported. Input is one bracketed paste and Enter; it counts as
// delivered only when UserPromptSubmit reports the same text.
func (c *codec) flush() Actions {
	if !c.ready || c.running || c.sent != nil || len(c.queue) == 0 {
		return Actions{}
	}
	text := c.queue[0]
	c.queue = c.queue[1:]
	c.sent = &sentInput{text: normalize(text)}
	c.pasted = append(c.pasted, normalize(text))
	return Actions{Write: [][]byte{[]byte(pasteStart + text + pasteEnd), []byte(confirm)}}
}

// sentInput is input written and not yet reported by a hook.
type sentInput struct {
	text string
	at   time.Time // the first Tick after writing it
}

// Tick acts on the codec's deadlines: input the CLI never reported, and
// questions whose hook has passed its timeout.
func (c *codec) Tick(now time.Time) Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.number(merge(c.tickInput(now), c.tickRequests(now)))
}

// tickInput ends the wait for input the CLI never reported: after the
// input timeout the turn it was to start ends as failed, "not delivered".
func (c *codec) tickInput(now time.Time) Actions {
	if c.sent == nil {
		return Actions{}
	}
	if c.sent.at.IsZero() {
		c.sent.at = now
		return Actions{}
	}
	if now.Sub(c.sent.at) < c.inputTimeout() {
		return Actions{}
	}
	for i, p := range c.pasted {
		if p == c.sent.text {
			c.pasted = append(c.pasted[:i], c.pasted[i+1:]...)
			break
		}
	}
	c.sent = nil
	a := c.events(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.NotDelivered})
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
func (c *codec) Interrupt() Actions {
	return Actions{Write: [][]byte{[]byte(stopKey)}}
}

// Decode reads one recorded line. Lines it cannot read become one Unknown
// event; it never fails.
func (c *codec) Decode(src agent.Source, line []byte) Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.number(c.decode(src, line))
}

func (c *codec) decode(src agent.Source, line []byte) Actions {
	switch src {
	case agent.File:
		a := c.decodeTranscript(line)
		return merge(a, c.track(c.current, a)) // the prompt of the line just read
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
	if err := json.Unmarshal(k.Payload, &p); err != nil {
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
			}
		case agent.SessionEnded:
			c.ready, c.running = false, false
		}
	}
	return c.flush()
}

func merge(a, b Actions) Actions {
	a.Events = append(a.Events, b.Events...)
	a.Write = append(a.Write, b.Write...)
	a.Reply = append(a.Reply, b.Reply...)
	a.Follow = append(a.Follow, b.Follow...)
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
	Message     struct {
		Content json.RawMessage `json:"content"`
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
	}
	return out
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
		a := c.release(t)
		t.ended = true
		return merge(a, c.events(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted}))
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
	return a
}

func (c *codec) decodeHook(k call, p payload) Actions {
	switch p.Event {
	case "SessionStart":
		// A start or a resume opens the session; a compaction or a clear
		// starts it again inside, which is no news to the core.
		if p.Source != "startup" && p.Source != "resume" {
			return Actions{}
		}
		return c.events(agent.Event{Kind: agent.SessionReady, SessionID: p.SessionID, Model: p.Model, Effort: p.Effort.Level})
	case "UserPromptSubmit":
		return c.events(agent.Event{Kind: agent.TurnStarted, StartedBy: c.starter(p.Prompt)})
	case "Stop":
		running := 0
		for _, t := range p.BackgroundTasks {
			if t.Status == "running" {
				running++
			}
		}
		return merge(c.end(p.PromptID, p.LastMessage), c.events(
			agent.Event{Kind: agent.Text, Text: p.LastMessage, Final: true},
			agent.Event{Kind: agent.TurnEnded, Status: agent.Completed, Background: running},
		))
	case "StopFailure":
		return merge(c.end(p.PromptID, ""), c.events(
			agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: failureReason(p.Error), Text: p.LastMessage}))
	case "PreToolUse":
		if p.ToolName == askTool {
			if !k.Wait {
				return Actions{} // the duplicate the settings file registers; the waiting call is the question
			}
			return c.question(k, p)
		}
		return c.events(agent.Event{Kind: agent.ToolStarted, ID: p.ToolUseID, Tool: p.ToolName})
	case "PostToolUse", "PostToolUseFailure":
		if p.ToolName == askTool {
			return Actions{}
		}
		return c.events(agent.Event{Kind: agent.ToolEnded, ID: p.ToolUseID, Tool: p.ToolName})
	case "PermissionRequest":
		if c.spec.Approvals == agent.AllowAll && k.Wait {
			return Actions{Reply: []agent.HookReply{{Call: k.Hook, Body: allow}}}
		}
		return Actions{}
	case "SubagentStart":
		return c.events(agent.Event{Kind: agent.SubagentStarted, ID: p.AgentID, Tool: p.AgentType})
	case "SubagentStop":
		if p.AgentType == "" {
			return Actions{} // an agent the CLI runs for itself, such as the compaction summary
		}
		return c.events(agent.Event{Kind: agent.SubagentEnded, ID: p.AgentID, Tool: p.AgentType})
	case "PreCompact":
		c.compact++
		return c.events(agent.Event{Kind: agent.ToolStarted, ID: compactID(c.compact), Tool: CompactTool})
	case "PostCompact":
		return c.events(agent.Event{Kind: agent.ToolEnded, ID: compactID(c.compact), Tool: CompactTool})
	case "SessionEnd":
		return c.events(agent.Event{Kind: agent.SessionEnded})
	}
	return c.events(agent.Event{Kind: agent.Unknown, Text: p.Event})
}

// CompactTool is the tool name a compaction is reported under: it is
// activity of the agent like a tool, with a start and an end.
const CompactTool = "compaction"

func compactID(n int) string { return "compaction-" + strconv.Itoa(n) }

// allow is the reply that allows a permission request.
var allow = []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`)

// starter tells who started a turn from its prompt: text hataori pasted is
// the caller's, a task notification is the agent's own, and anything else
// was typed at the terminal.
func (c *codec) starter(prompt string) agent.Starter {
	text := normalize(unwrapPaste(prompt))
	for i, p := range c.pasted {
		if p == text {
			c.pasted = append(c.pasted[:i], c.pasted[i+1:]...)
			if c.sent != nil && c.sent.text == text {
				c.sent = nil // delivered
			}
			return agent.ByCaller
		}
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "<task-notification>") {
		return agent.ByAgent
	}
	return agent.ByTerminal
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

func normalize(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
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
