package claudecode

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/yousan/hataori/internal/agent"
)

// NewCodec returns the translator for one running session.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec {
	return &codec{spec: spec, seen: map[string]bool{}}
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
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
	Model     string `json:"model"`
	Effort    struct {
		Level string `json:"level"`
	} `json:"effort"`
	Prompt          string `json:"prompt"`
	ToolName        string `json:"tool_name"`
	ToolUseID       string `json:"tool_use_id"`
	LastMessage     string `json:"last_assistant_message"`
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
	c.pasted = append(c.pasted, normalize(in.Text))
	return Actions{}, nil
}

func (c *codec) Answer(agent.Answer) (Actions, error) { return Actions{}, nil }
func (c *codec) Interrupt() Actions                   { return Actions{} }

// Decode reads one recorded line. Lines it cannot read become one Unknown
// event; it never fails.
func (c *codec) Decode(src agent.Source, line []byte) Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	if src != agent.Hook {
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
	return c.decodeHook(k, p)
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
		return c.events(
			agent.Event{Kind: agent.Text, Text: p.LastMessage, Final: true},
			agent.Event{Kind: agent.TurnEnded, Status: agent.Completed, Background: running},
		)
	case "StopFailure":
		return c.events(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: failureReason(p.Error), Text: p.LastMessage})
	case "PreToolUse":
		if p.ToolName == askTool {
			return Actions{} // a question; reported by its own issue's decoding
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

// events numbers evs and returns them.
func (c *codec) events(evs ...agent.Event) Actions {
	for i := range evs {
		c.seq++
		evs[i].Seq = c.seq
	}
	return Actions{Events: evs}
}
