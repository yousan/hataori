package codex

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// skippedMethods lists notifications the codec reads without acting on them.
var skippedMethods = map[string]bool{
	"item/agentMessage/delta":           true,
	"item/reasoning/summaryTextDelta":   true,
	"item/reasoning/textDelta":          true,
	"item/commandExecution/outputDelta": true,
	"item/fileChange/outputDelta":       true,
	"thread/status/changed":             true,
	"thread/readState/changed":          true,
	"account/updated":                   true,
	"mcpServer/startupStatus/updated":   true,
	"remoteControl/status/changed":      true,
	"turn/moderationMetadata":           true,
	"turn/diff/updated":                 true,
	"deprecationNotice":                 true,
	"thread/goal/cleared":               true,
	"thread/settings/updated":           true,
}

// skippedItems lists items the codec reads without acting on them.
var skippedItems = map[string]bool{"reasoning": true}

// DefaultInputTimeout is the time allowed for an input to be reported.
const DefaultInputTimeout = 30 * time.Second

// Options configures a Codex codec.
type Options struct {
	// Sandbox is the app-server sandbox policy; empty leaves its default.
	Sandbox string
	// ApprovalPolicy is the app-server approval policy; empty leaves its default.
	ApprovalPolicy string
	// InputTimeout is the delivery deadline; zero uses DefaultInputTimeout.
	InputTimeout time.Duration
}

type pendingInput struct {
	input   agent.Input
	at      time.Time
	request int
	turn    string
}

type codec struct {
	mu       sync.Mutex      // protects all codec state
	spec     agent.Spec      // session configuration
	opts     Options         // app-server policies and input timeout
	seq      uint64          // last event sequence number
	next     int             // next request id
	requests map[int]string  // outstanding request methods by id
	session  string          // current thread id
	ready    bool            // the thread has been reported
	closed   bool            // the session has ended
	running  bool            // a turn is running or requested
	started  bool            // the current turn's start has been reported
	turnID   string          // current turn id
	sent     []*pendingInput // written inputs awaiting their report
	queue    []agent.Input   // inputs waiting to be written
	requestState
	toolState
	usageState
	held *string // latest agent text that may be final
}

// NewCodec returns a translator for one app-server process.
func NewCodec(spec agent.Spec, opts Options) agent.Codec {
	if opts.InputTimeout == 0 {
		opts.InputTimeout = DefaultInputTimeout
	}
	return &codec{spec: spec, opts: opts, next: 1, requests: make(map[int]string)}
}

// emit numbers and appends an event.
func (c *codec) emit(a *agent.Actions, e agent.Event) {
	c.seq++
	e.Seq = c.seq
	a.Events = append(a.Events, e)
}

// write appends a JSON-RPC message and records its request id.
func (c *codec) write(a *agent.Actions, method string, p any, request bool) int {
	m := map[string]any{"method": method, "params": p}
	id := 0
	if request {
		id = c.next
		c.next++
		m["id"] = id
		c.requests[id] = method
	}
	b, _ := json.Marshal(m)
	a.Write = append(a.Write, append(b, '\n'))
	return id
}

// Open starts the app-server session.
func (c *codec) Open() agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	var a agent.Actions
	c.write(&a, "initialize", map[string]any{"clientInfo": map[string]any{"name": "hataori", "version": "0"}, "capabilities": map[string]any{"experimentalApi": true}}, true)
	c.write(&a, "initialized", map[string]any{}, false)
	p := map[string]any{}
	if c.spec.Dir != "" {
		p["cwd"] = c.spec.Dir
	}
	if c.spec.Model != "" {
		p["model"] = c.spec.Model
	}
	if c.spec.Instructions != "" {
		p["developerInstructions"] = c.spec.Instructions
	}
	if c.opts.Sandbox != "" {
		p["sandbox"] = c.opts.Sandbox
	}
	if c.opts.ApprovalPolicy != "" {
		p["approvalPolicy"] = c.opts.ApprovalPolicy
	}
	method := "thread/start"
	if c.spec.ResumeID != "" {
		method = "thread/resume"
		p["threadId"] = c.spec.ResumeID
	} else if c.spec.Effort != "" {
		p["config"] = map[string]any{"model_reasoning_effort": c.spec.Effort}
	}
	c.write(&a, method, p, true)
	c.readLimits(&a)
	return a
}

// flush writes the next queued input when the session can accept it.
func (c *codec) flush(a *agent.Actions) {
	if !c.ready || c.closed || c.running || len(c.sent) > 0 || len(c.queue) == 0 {
		return
	}
	in := c.queue[0]
	c.queue = c.queue[1:]
	p := map[string]any{"threadId": c.session, "input": []any{map[string]any{"type": "text", "text": in.Text}}}
	if c.spec.Effort != "" {
		p["effort"] = c.spec.Effort
	}
	id := c.write(a, "turn/start", p, true)
	a.Inputs = append(a.Inputs, in.ID)
	c.sent = append(c.sent, &pendingInput{input: in, request: id})
	c.running = true
}

// Input records or queues an input.
func (c *codec) Input(in agent.Input) (agent.Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var a agent.Actions
	if in.Written {
		c.sent = append(c.sent, &pendingInput{input: in})
		return a, nil
	}
	c.queue = append(c.queue, in)
	c.flush(&a)
	return a, nil
}

// Answer answers an open request.
func (c *codec) Answer(answer agent.Answer) (agent.Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.answer(answer)
}

// Interrupt returns no actions until interrupts are supported.
func (c *codec) Interrupt() agent.Actions { c.mu.Lock(); defer c.mu.Unlock(); return agent.Actions{} }

// Tick expires unreported inputs and writes the next queued input.
func (c *codec) Tick(now time.Time) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	var a agent.Actions
	remaining := c.sent[:0]
	for _, s := range c.sent {
		if s.at.IsZero() {
			s.at = now
		}
		if !now.Before(s.at.Add(c.opts.InputTimeout)) {
			c.emit(&a, agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.NotDelivered, Input: s.input.ID})
			if s.request != 0 {
				delete(c.requests, s.request)
				c.running = false
			}
		} else {
			remaining = append(remaining, s)
		}
	}
	c.sent = remaining
	c.flush(&a)
	return a
}

// Decode translates one app-server line into actions.
func (c *codec) Decode(src agent.Source, line []byte) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	var a agent.Actions
	if src != agent.Output {
		return a
	}
	var m message
	if json.Unmarshal(line, &m) != nil {
		c.unknown(&a, line)
		return a
	}
	if c.closed {
		return a
	}
	c.timestamp(&a, m.EmittedAtMS)
	if m.Method == "" {
		c.response(&a, m, line)
		return a
	}
	if len(m.ID) != 0 {
		c.serverRequest(&a, m, line)
		return a
	}
	var p params
	if json.Unmarshal(m.Params, &p) != nil {
		c.unknown(&a, line)
		return a
	}
	if m.Method == "serverRequest/resolved" {
		c.resolved(&a, m.Params)
		return a
	}
	if m.Method == "item/started" {
		c.rememberItem(p)
	}
	if m.Method == "item/completed" {
		c.forgetItem(p.Item.ID)
	}
	if m.Method == "turn/completed" {
		c.forgetThread(p.ThreadID)
		c.withdrawThread(&a, p.ThreadID)
	}
	if m.Method == "turn/started" && (p.ThreadID == "" || c.session == "" || p.ThreadID == c.session) {
		c.withdrawAsync(&a)
	}
	if p.ThreadID != "" && c.session != "" && p.ThreadID != c.session {
		if _, ok := c.subagents[p.ThreadID]; ok {
			switch m.Method {
			case "item/started", "item/completed":
				c.toolItem(&a, p, m.Method)
			case "turn/completed":
				c.endSubagent(&a, p.ThreadID)
			}
		}
		return a
	}
	switch m.Method {
	case RateLimitsUpdated:
		c.readLimits(&a)
	case "thread/tokenUsage/updated":
		c.usage(&a, m.Params)
	case "thread/started":
	case "turn/started":
		c.turnStarted(&a, p)
	case "turn/completed":
		c.turnCompleted(&a, p)
	case "item/started", "item/completed":
		c.item(&a, p, m.Method, line)
	default:
		if !skippedMethods[m.Method] {
			c.unknown(&a, line)
		}
	}
	return a
}

// unknown reports a line the codec does not recognize.
func (c *codec) unknown(a *agent.Actions, line []byte) {
	c.emit(a, agent.Event{Kind: agent.Unknown, Text: string(line)})
}

// response matches a response and updates session or turn state.
func (c *codec) response(a *agent.Actions, m message, line []byte) {
	if len(m.ID) == 0 {
		c.unknown(a, line)
		return
	}
	var id int
	if json.Unmarshal(m.ID, &id) != nil {
		c.unknown(a, line)
		return
	}
	if id >= c.next {
		c.next = id + 1
	}
	method := c.requests[id]
	delete(c.requests, id)
	if m.Error != nil {
		msg := m.Error.Message
		switch {
		case method == "thread/resume" || (method == "" && strings.HasPrefix(msg, "no rollout found for thread id")):
			reason := agent.Reason(msg)
			lower := strings.ToLower(msg)
			if strings.Contains(lower, "not found") || strings.Contains(lower, "no rollout found") {
				reason = agent.CannotResume
			}
			c.closed = true
			c.emit(a, agent.Event{Kind: agent.SessionEnded, Reason: reason})
		case method == "initialize" || method == "thread/start":
			c.closed = true
			c.emit(a, agent.Event{Kind: agent.SessionEnded, Reason: agent.Reason(msg)})
		case method == "turn/steer":
			c.steerFailed(a, id)
		case method == "turn/start":
			for i, s := range c.sent {
				if s.request == id {
					c.emit(a, agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.APIError, Input: s.input.ID})
					c.sent = append(c.sent[:i], c.sent[i+1:]...)
					break
				}
			}
			c.running = false
			c.flush(a)
		default:
			c.unknown(a, line)
		}
		return
	}
	delete(c.steers, id)
	if c.limitResponse(a, method, m.Result, line) {
		return
	}
	var r result
	if json.Unmarshal(m.Result, &r) != nil {
		c.unknown(a, line)
		return
	}
	if r.Thread != nil && !c.ready && !c.closed {
		if c.spec.ResumeID != "" && r.Thread.ID != c.spec.ResumeID {
			c.closed = true
			c.emit(a, agent.Event{Kind: agent.SessionEnded, Reason: agent.WrongSession})
			return
		}
		c.model = r.Model
		c.session = r.Thread.ID
		c.ready = true
		c.emit(a, agent.Event{Kind: agent.SessionReady, SessionID: c.session, Model: r.Model, Effort: r.Effort})
		c.flush(a)
	}
	if r.Turn != nil {
		for _, s := range c.sent {
			if s.request == id {
				s.turn = r.Turn.ID
				break
			}
		}
	}
}

// reportStart emits the current turn start and associates its input.
func (c *codec) reportStart(a *agent.Actions, starter agent.Starter, index int) {
	if c.started {
		return
	}
	e := agent.Event{Kind: agent.TurnStarted, StartedBy: starter}
	if index >= 0 {
		e.Input = c.sent[index].input.ID
		c.sent = append(c.sent[:index], c.sent[index+1:]...)
	}
	c.emit(a, e)
	c.started = true
}

// turnStarted records a turn start and reports it when its input is known.
func (c *codec) turnStarted(a *agent.Actions, p params) {
	c.running = true
	c.started = false
	c.beginUsage()
	c.turnID = p.Turn.ID
	c.held = nil
	for i, s := range c.sent {
		if s.request != 0 && (s.turn == "" || s.turn == p.Turn.ID) {
			c.reportStart(a, agent.ByCaller, i)
			return
		}
	}
}

// normalize normalizes input text for matching.
func normalize(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }

// item handles a started or completed item.
func (c *codec) item(a *agent.Actions, p params, method string, line []byte) {
	if c.toolItem(a, p, method) {
		return
	}
	switch p.Item.Type {
	case "userMessage":
		if method == "item/started" && p.ThreadID == c.session {
			c.withdrawAsync(a)
		}
		if !c.started && method == "item/started" {
			var parts []string
			for _, v := range p.Item.Content {
				parts = append(parts, v.Text)
			}
			text := normalize(strings.Join(parts, ""))
			for i, s := range c.sent {
				if normalize(s.input.Text) == text {
					c.reportStart(a, agent.ByCaller, i)
					return
				}
			}
			c.reportStart(a, agent.ByTerminal, -1)
		}
	case "agentMessage":
		if !c.started {
			c.reportStart(a, agent.ByAgent, -1)
		}
		if method != "item/completed" {
			return
		}
		if c.held != nil {
			c.emit(a, agent.Event{Kind: agent.Text, Text: *c.held})
			c.held = nil
		}
		if p.Item.Delivery == "async" && len(p.Item.Questions) > 0 {
			c.asyncQuestion(a, p)
			return
		}
		if p.Item.Phase == "commentary" {
			c.emit(a, agent.Event{Kind: agent.Text, Text: p.Item.Text})
		} else {
			text := p.Item.Text
			c.held = &text
		}
	default:
		if !c.started {
			c.reportStart(a, agent.ByAgent, -1)
		}
		if !skippedItems[p.Item.Type] {
			c.unknown(a, line)
		}
	}
}

// turnCompleted reports the final text and end of a turn.
func (c *codec) turnCompleted(a *agent.Actions, p params) {
	if !c.started {
		c.reportStart(a, agent.ByAgent, -1)
	}
	c.endTools(a)
	if c.held != nil {
		c.emit(a, agent.Event{Kind: agent.Text, Text: *c.held, Final: true})
		c.held = nil
	}
	e := agent.Event{Kind: agent.TurnEnded}
	switch p.Turn.Status {
	case "interrupted":
		e.Status = agent.Interrupted
	case "failed":
		e.Status = agent.Failed
		e.Reason = agent.APIError
		// No failed turn has been captured; these names come from the 0.163.0 schema.
		var kind string
		_ = json.Unmarshal(p.Turn.Error.CodexErrorInfo, &kind)
		switch kind {
		case "usageLimitExceeded":
			e.Reason = agent.Limit
		case "unauthorized":
			e.Reason = agent.Login
		}
	}
	c.emit(a, e)
	c.running = false
	c.started = false
	c.turnID = ""
	c.flush(a)
}
