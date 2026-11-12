package fake

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// codec reads the fake's reports. It does no I/O.
type codec struct {
	mu       sync.Mutex
	terminal bool
	seq      uint64
	followed map[string]bool
	asked    map[string]string // request id → the fake's question id (pipes) or hook call id (terminal)
}

// report is any of the fake's reports.
type report struct {
	T          string `json:"t"`
	Session    string `json:"session"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	Resume     string `json:"resume"`
	Transcript string `json:"transcript"`
	By         string `json:"by"`
	Text       string `json:"text"`
	Final      bool   `json:"final"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	End        bool   `json:"end"`
	Path       string `json:"path"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Background int    `json:"background"`
}

// call is a hook call as the session host records it.
type call struct {
	Hook    string          `json:"hook"`
	Wait    bool            `json:"wait"`
	Payload json.RawMessage `json:"payload"`
}

func (c *codec) Open() agent.Actions { return agent.Actions{} }

// Decode reads one line. In the terminal form only hook calls and the
// transcript are read: the terminal's own output must never reach a codec,
// and the fake fails loudly when it does.
func (c *codec) Decode(src agent.Source, line []byte) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	var r report
	var hookID string
	switch {
	case c.terminal && src == agent.Output:
		panic("fake: the codec was given the terminal's output")
	case src == agent.Hook:
		var k call
		if json.Unmarshal(line, &k) != nil || json.Unmarshal(k.Payload, &r) != nil {
			return c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
		}
		hookID = k.Hook
	default:
		if json.Unmarshal(line, &r) != nil {
			return c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
		}
	}
	var a agent.Actions
	switch r.T {
	case "ready":
		id := r.Session
		if r.Resume != "" {
			id = r.Resume
		}
		a = c.events(agent.Event{Kind: agent.SessionReady, SessionID: id, Model: r.Model, Effort: r.Effort})
		if r.Transcript != "" && c.terminal && !c.followed[r.Transcript] {
			c.followed[r.Transcript] = true
			a.Follow = []string{r.Transcript}
		}
	case "turn":
		by := agent.ByCaller
		if r.By == "agent" {
			by = agent.ByAgent
		}
		a = c.events(agent.Event{Kind: agent.TurnStarted, StartedBy: by})
	case "text":
		a = c.events(agent.Event{Kind: agent.Text, Text: r.Text, Final: r.Final})
	case "tool":
		k := agent.ToolStarted
		if r.End {
			k = agent.ToolEnded
		}
		a = c.events(agent.Event{Kind: k, ID: r.ID, Tool: r.Name})
	case "subagent":
		k := agent.SubagentStarted
		if r.End {
			k = agent.SubagentEnded
		}
		a = c.events(agent.Event{Kind: k, ID: r.ID})
	case "ask":
		id := r.ID
		if c.terminal {
			id = hookID
		}
		if c.asked == nil {
			c.asked = map[string]string{}
		}
		c.asked[id] = r.ID
		a = c.events(agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: id, Text: r.Text}})
	case "file":
		a = c.events(agent.Event{Kind: agent.FileOut, File: r.Path})
	case "end":
		e := agent.Event{Kind: agent.TurnEnded, Background: r.Background}
		switch r.Status {
		case "failed":
			e.Status, e.Reason = agent.Failed, agent.Reason(r.Reason)
		case "interrupted":
			e.Status = agent.Interrupted
		}
		a = c.events(e)
	case "interrupted":
		a = c.events(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted})
	default:
		a = c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
	}
	return a
}

func (c *codec) events(evs ...agent.Event) agent.Actions {
	for i := range evs {
		c.seq++
		evs[i].Seq = c.seq
	}
	return agent.Actions{Events: evs}
}

const (
	pasteKey  = "\r"
	escapeKey = "\x1b"
)

// Input is one line on pipes, or one bracketed paste and Enter.
func (c *codec) Input(in agent.Input) (agent.Actions, error) {
	if c.terminal {
		return agent.Actions{Write: [][]byte{[]byte(pasteStart + in.Text + pasteEnd), []byte(pasteKey)}}, nil
	}
	b, _ := json.Marshal(map[string]string{"input": in.Text})
	return agent.Actions{Write: [][]byte{append(b, '\n')}}, nil
}

// ErrNotAsked is returned by Answer for a question that is not open.
var ErrNotAsked = errors.New(Name + ": no such question is open")

// Answer replies to the hook call on a terminal, or writes the answer on
// pipes.
func (c *codec) Answer(a agent.Answer) (agent.Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.asked[a.Request]
	if !ok {
		return agent.Actions{}, ErrNotAsked
	}
	delete(c.asked, a.Request)
	if c.terminal {
		return agent.Actions{Reply: []agent.HookReply{{Call: a.Request, Body: []byte(a.Text)}}}, nil
	}
	b, _ := json.Marshal(map[string]any{"answer": map[string]string{"id": q, "text": a.Text}})
	return agent.Actions{Write: [][]byte{append(b, '\n')}}, nil
}

// Interrupt stops the running turn: Escape on a terminal.
func (c *codec) Interrupt() agent.Actions {
	if c.terminal {
		return agent.Actions{Write: [][]byte{[]byte(escapeKey)}}
	}
	return agent.Actions{Write: [][]byte{[]byte("{\"interrupt\":true}\n")}}
}

// Tick does nothing: the fake has no deadlines.
func (c *codec) Tick(time.Time) agent.Actions { return agent.Actions{} }
