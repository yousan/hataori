package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// A question is the PreToolUse call of AskUserQuestion, waiting for its
// answer. The answer goes back as the reply to that call: allow the tool,
// with the tool's input and an answers map from each question to its
// answer. A free-text answer and several chosen options ("a, b") were
// both accepted by the CLI in this form (see the question-free and
// question-multi captures).

type question struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// request is a question that waits for its answer.
type request struct {
	input     map[string]json.RawMessage // the tool's input, echoed in the reply
	questions []question
	toolUseID string
	at        time.Time // when the host took the call; the first Tick if unknown
}

// withdrawAll withdraws every waiting question: the CLI went on without
// its answer (the turn ended, was interrupted, or the session ended).
func (c *codec) withdrawAll() Actions {
	ids := make([]string, 0, len(c.requests))
	for id := range c.requests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var a Actions
	for _, id := range ids {
		a = merge(a, c.withdraw(id))
	}
	return a
}

// withdrawTool withdraws the question of a tool call that finished.
func (c *codec) withdrawTool(toolUseID string) Actions {
	for id, r := range c.requests {
		if toolUseID != "" && r.toolUseID == toolUseID {
			return c.withdraw(id)
		}
	}
	return Actions{}
}

func (c *codec) withdraw(id string) Actions {
	delete(c.requests, id)
	return c.events(agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: id}})
}

// ErrNotWaiting is returned by Answer for a call that no longer waits.
var ErrNotWaiting = errors.New(Name + ": that question is no longer waiting")

func (c *codec) question(k call, p payload) Actions {
	var input map[string]json.RawMessage
	var qs struct {
		Questions []question `json:"questions"`
	}
	if json.Unmarshal(p.ToolInput, &input) != nil || json.Unmarshal(p.ToolInput, &qs) != nil || len(qs.Questions) == 0 {
		return c.events(agent.Event{Kind: agent.Unknown, Text: string(p.ToolInput)})
	}
	r := &request{input: input, questions: qs.Questions, toolUseID: p.ToolUseID}
	if k.At > 0 {
		r.at = time.UnixMilli(k.At) // the timeout runs from the call, also across a restart
	}
	c.requests[k.Hook] = r
	info := &agent.RequestInfo{ID: k.Hook, Text: render(qs.Questions)}
	if len(qs.Questions) == 1 {
		for _, o := range qs.Questions[0].Options {
			info.Choices = append(info.Choices, o.Label)
		}
		info.Multi = qs.Questions[0].MultiSelect
	}
	return c.events(agent.Event{Kind: agent.Request, Request: info})
}

// render shows the questions as text, the way phase 1 asks them in chat.
func render(qs []question) string {
	var b strings.Builder
	for i, q := range qs {
		if len(qs) > 1 {
			fmt.Fprintf(&b, "%d. ", i+1)
		}
		b.WriteString(q.Question)
		if q.MultiSelect {
			b.WriteString(" (one or more)")
		}
		b.WriteString("\n")
		for _, o := range q.Options {
			if o.Description != "" {
				fmt.Fprintf(&b, "- %s: %s\n", o.Label, o.Description)
			} else {
				fmt.Fprintf(&b, "- %s\n", o.Label)
			}
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Answer returns the reply to the waiting question a.Request. Chosen
// options are joined with ", " as the CLI joins them; otherwise the text
// is the answer. With several questions, the i-th non-empty line of the
// text answers the i-th question when the counts match; otherwise the
// whole text answers each.
func (c *codec) Answer(a agent.Answer) (Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.requests[a.Request]
	if r == nil {
		return Actions{}, ErrNotWaiting
	}
	answers := map[string]string{}
	whole := strings.TrimSpace(a.Text)
	if len(a.Choices) > 0 {
		whole = strings.Join(a.Choices, ", ")
	}
	var lines []string
	for _, l := range strings.Split(whole, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	for i, q := range r.questions {
		if len(r.questions) > 1 && len(lines) == len(r.questions) {
			answers[q.Question] = lines[i]
		} else {
			answers[q.Question] = whole
		}
	}
	input := map[string]any{}
	for k, v := range r.input {
		input[k] = v
	}
	input["answers"] = answers
	body, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": input}})
	if err != nil {
		return Actions{}, err
	}
	delete(c.requests, a.Request)
	return Actions{Reply: []agent.HookReply{{Call: a.Request, Body: body}}}, nil
}

// tickRequests withdraws questions whose hook has passed its timeout. The
// CLI then ignores the hook and shows its own dialog in the terminal (seen
// on the real CLI, recorded in question-timeout-q), so the question can no
// longer be answered from here.
func (c *codec) tickRequests(now time.Time) Actions {
	var a Actions
	for id, r := range c.requests {
		if r.at.IsZero() {
			r.at = now
			continue
		}
		if c.hookTimeout > 0 && now.Sub(r.at) >= c.hookTimeout {
			a = merge(a, c.withdraw(id))
		}
	}
	return a
}
