package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/yousan/hataori/internal/agent"
)

// ErrNotWaiting is returned when an answer has no open request.
var ErrNotWaiting = errors.New("codex: that request is no longer waiting")

// questionOption is an option of a tool input question.
type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// wireQuestion is a tool input question.
type wireQuestion struct {
	ID       string           `json:"id"`
	Question string           `json:"question"`
	Options  []questionOption `json:"options"`
}

// asyncWireQuestion is a question in an agent message.
type asyncWireQuestion struct {
	Title   string   `json:"title"`
	Options []string `json:"options"`
}

// openRequest is an unanswered server request or async question.
type openRequest struct {
	id        json.RawMessage
	thread    string
	questions []wireQuestion
	async     bool
	approval  bool
}

// requestState holds the requests and file changes of the current turns.
type requestState struct {
	open        map[string]*openRequest // unanswered requests by event id
	paths       map[string][]string     // file change paths by item id
	pathThreads map[string]string       // thread of each file change
	steers      map[int]string          // answers awaiting a steer response
}

// compactID returns a JSON-RPC id in its compact form.
func compactID(id json.RawMessage) string {
	var b bytes.Buffer
	_ = json.Compact(&b, id)
	return b.String()
}

// rememberItem records a file change for a later approval.
func (c *codec) rememberItem(p params) {
	if p.Item.Type != "fileChange" {
		return
	}
	if c.paths == nil {
		c.paths = map[string][]string{}
		c.pathThreads = map[string]string{}
	}
	c.pathThreads[p.Item.ID] = p.ThreadID
	for _, v := range p.Item.Changes {
		c.paths[p.Item.ID] = append(c.paths[p.Item.ID], v.Path)
	}
}

// forgetItem removes completed file change paths.
func (c *codec) forgetItem(id string) {
	delete(c.paths, id)
	delete(c.pathThreads, id)
}

// forgetThread removes file changes left open at turn end.
func (c *codec) forgetThread(thread string) {
	for id, owner := range c.pathThreads {
		if thread == c.session || owner == thread {
			c.forgetItem(id)
		}
	}
}

// reply writes a JSON-RPC response to a server request.
func (c *codec) reply(a *agent.Actions, id json.RawMessage, result any) {
	b, _ := json.Marshal(struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result"`
	}{id, result})
	a.Write = append(a.Write, append(b, '\n'))
}

// serverRequest handles an app-server request.
func (c *codec) serverRequest(a *agent.Actions, m message, line []byte) {
	var p struct {
		ThreadID  string         `json:"threadId"`
		ItemID    string         `json:"itemId"`
		Command   string         `json:"command"`
		Reason    string         `json:"reason"`
		Questions []wireQuestion `json:"questions"`
	}
	_ = json.Unmarshal(m.Params, &p)
	approval := m.Method == "item/commandExecution/requestApproval" || m.Method == "item/fileChange/requestApproval"
	if !approval && m.Method != "item/tool/requestUserInput" {
		b, _ := json.Marshal(struct {
			ID    json.RawMessage `json:"id"`
			Error any             `json:"error"`
		}{m.ID, map[string]any{"code": -32601, "message": "hataori does not handle " + m.Method}})
		a.Write = append(a.Write, append(b, '\n'))
		c.unknown(a, line)
		return
	}
	if approval && c.spec.Approvals == agent.AllowAll {
		c.reply(a, m.ID, map[string]any{"decision": "accept"})
		return
	}
	id := p.ItemID
	if id == "" {
		id = "rpc-" + compactID(m.ID)
	}
	r := &openRequest{id: append(json.RawMessage(nil), m.ID...), thread: p.ThreadID, questions: p.Questions, approval: approval}
	info := &agent.RequestInfo{ID: id, Approval: approval}
	if approval {
		info.Text = p.Command
		if m.Method == "item/fileChange/requestApproval" {
			info.Text = strings.Join(c.paths[p.ItemID], "\n")
			if info.Text == "" {
				info.Text = "Change files"
			}
		}
		if p.Reason != "" {
			info.Text += "\n" + p.Reason
		}
	} else {
		info.Text = renderQuestions(p.Questions)
		if len(p.Questions) == 1 {
			for _, o := range p.Questions[0].Options {
				info.Choices = append(info.Choices, o.Label)
			}
		}
	}
	c.withdrawAsync(a)
	if c.open == nil {
		c.open = map[string]*openRequest{}
	}
	c.open[id] = r
	c.emit(a, agent.Event{Kind: agent.Request, Request: info})
}

// renderQuestions renders questions for chat.
func renderQuestions(qs []wireQuestion) string {
	var b strings.Builder
	for i, q := range qs {
		if i > 0 {
			b.WriteByte('\n')
		}
		if len(qs) > 1 {
			fmt.Fprintf(&b, "%d. ", i+1)
		}
		b.WriteString(q.Question)
		for _, o := range q.Options {
			if len(qs) == 1 && o.Description == "" {
				continue
			}
			fmt.Fprintf(&b, "\n- %s", o.Label)
			if o.Description != "" {
				b.WriteString(": " + o.Description)
			}
		}
	}
	return b.String()
}

// asyncQuestion opens a question delivered in an agent message.
func (c *codec) asyncQuestion(a *agent.Actions, p params) {
	var qs []wireQuestion
	for _, q := range p.Item.Questions {
		w := wireQuestion{Question: q.Title}
		for _, o := range q.Options {
			w.Options = append(w.Options, questionOption{Label: o})
		}
		qs = append(qs, w)
	}
	c.withdrawAsync(a)
	if c.open == nil {
		c.open = map[string]*openRequest{}
	}
	c.open[p.Item.ID] = &openRequest{async: true, thread: p.ThreadID, questions: qs}
	info := &agent.RequestInfo{ID: p.Item.ID, Text: renderQuestions(qs)}
	if len(qs) == 1 {
		for _, o := range qs[0].Options {
			info.Choices = append(info.Choices, o.Label)
		}
	}
	c.emit(a, agent.Event{Kind: agent.Request, Request: info})
}

// answer answers an open request.
func (c *codec) answer(answer agent.Answer) (agent.Actions, error) {
	var a agent.Actions
	r := c.open[answer.Request]
	if r == nil {
		return a, ErrNotWaiting
	}
	delete(c.open, answer.Request)
	if r.approval {
		decision := "decline"
		if answer.Allow {
			decision = "accept"
		}
		c.reply(&a, r.id, map[string]any{"decision": decision})
		return a, nil
	}
	whole := strings.TrimSpace(answer.Text)
	if len(answer.Choices) > 0 {
		whole = strings.Join(answer.Choices, ", ")
	}
	if r.async {
		if c.turnID != "" {
			id := c.write(&a, "turn/steer", map[string]any{"threadId": c.session, "expectedTurnId": c.turnID, "input": []any{map[string]any{"type": "text", "text": whole}}}, true)
			if c.steers == nil {
				c.steers = map[int]string{}
			}
			c.steers[id] = whole
		} else {
			c.queue = append(c.queue, agent.Input{Text: whole})
			c.flush(&a)
		}
		return a, nil
	}
	var lines []string
	for _, v := range strings.Split(whole, "\n") {
		if v = strings.TrimSpace(v); v != "" {
			lines = append(lines, v)
		}
	}
	answers := map[string]any{}
	for i, q := range r.questions {
		values := []string{whole}
		if len(r.questions) == 1 && len(answer.Choices) > 0 {
			values = answer.Choices
		} else if len(r.questions) > 1 && len(lines) == len(r.questions) {
			values = []string{lines[i]}
		}
		answers[q.ID] = map[string]any{"answers": values}
	}
	c.reply(&a, r.id, map[string]any{"answers": answers})
	return a, nil
}

// steerFailed queues a refused steer for the next turn.
func (c *codec) steerFailed(a *agent.Actions, id int) {
	if text, ok := c.steers[id]; ok {
		delete(c.steers, id)
		c.queue = append(c.queue, agent.Input{Text: text})
		c.flush(a)
	}
}

// withdraw withdraws matching open requests.
func (c *codec) withdraw(a *agent.Actions, match func(*openRequest) bool) {
	var ids []string
	for id, r := range c.open {
		if match(r) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		delete(c.open, id)
		c.emit(a, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: id}})
	}
}

// withdrawThread withdraws server requests at turn end.
func (c *codec) withdrawThread(a *agent.Actions, thread string) {
	if thread == "" {
		thread = c.session
	}
	c.withdraw(a, func(r *openRequest) bool {
		owner := r.thread
		if owner == "" {
			owner = c.session
		}
		return !r.async && owner == thread
	})
}

// withdrawAsync withdraws unanswered async questions.
func (c *codec) withdrawAsync(a *agent.Actions) {
	c.withdraw(a, func(r *openRequest) bool { return r.async })
}

// resolved withdraws a request resolved by the app-server.
func (c *codec) resolved(a *agent.Actions, raw json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	c.withdraw(a, func(r *openRequest) bool { return !r.async && compactID(r.id) == compactID(p.RequestID) })
}
