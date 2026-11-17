package fake

import (
	"bytes"
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
	running  bool              // a turn is running: input waits for its end, as an agent's own would
	queue    []agent.Input     // input held until the turn ends
	waiting  []*written        // inputs written and not yet reported, with an Input.ID
	timeout  time.Duration     // for waiting; DefaultInputTimeout when zero

	dialog   string // the start-up dialog to answer, if any
	screen   []byte // the screen so far, before the start
	answered bool   // Enter was sent for the dialog
	started  bool   // a hook was called: the screen must not come any more
}

// written is an input written and not yet reported.
type written struct {
	id string
	at time.Time // the first Tick after writing it
}

// DefaultInputTimeout is how long written input may go unreported.
const DefaultInputTimeout = 30 * time.Second

// report is any of the fake's reports.
type report struct {
	T          string   `json:"t"`
	Session    string   `json:"session"`
	Model      string   `json:"model"`
	Effort     string   `json:"effort"`
	Resume     string   `json:"resume"`
	Transcript string   `json:"transcript"`
	By         string   `json:"by"`
	Text       string   `json:"text"`
	Final      bool     `json:"final"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Parent     string   `json:"parent"`
	More       []string `json:"more"`
	Caption    string   `json:"caption"`
	End        bool     `json:"end"`
	Path       string   `json:"path"`
	Status     string   `json:"status"`
	Reason     string   `json:"reason"`
	Background int      `json:"background"`
	Input      string   `json:"input"`
}

// call is a hook call as the session host records it.
type call struct {
	Hook    string          `json:"hook"`
	Wait    bool            `json:"wait"`
	Payload json.RawMessage `json:"payload"`
}

// Open watches the screen when the fake shows a start-up dialog.
func (c *codec) Open() agent.Actions { return agent.Actions{WatchScreen: c.dialog != ""} }

// onScreen answers the dialog with Enter, once, when the screen shows it,
// and stops watching. The screen after that, after the start, or without a
// dialog to watch for, must never reach the codec.
func (c *codec) onScreen(b []byte) agent.Actions {
	if c.dialog == "" {
		panic("fake: the codec was given the screen without asking for it")
	}
	if c.started {
		panic("fake: the codec was given the screen after the start")
	}
	if c.answered {
		panic("fake: the codec was given the screen after it stopped watching")
	}
	c.screen = append(c.screen, b...)
	if !bytes.Contains(c.screen, []byte(c.dialog)) {
		return agent.Actions{}
	}
	c.answered = true
	return agent.Actions{Write: [][]byte{[]byte("\r")}, StopScreen: true}
}

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
	case src == agent.Screen:
		return c.onScreen(line)
	case src == agent.Hook:
		c.started = true
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
		a = c.events(agent.Event{Kind: agent.TurnStarted, StartedBy: by, Input: r.Input})
		c.running = true
		for i, w := range c.waiting {
			if w.id == r.Input {
				c.waiting = append(c.waiting[:i], c.waiting[i+1:]...)
				break
			}
		}
	case "text":
		a = c.events(agent.Event{Kind: agent.Text, Text: r.Text, Final: r.Final})
	case "tool":
		k := agent.ToolStarted
		if r.End {
			k = agent.ToolEnded
		}
		a = c.events(agent.Event{Kind: k, ID: r.ID, Tool: r.Name, Parent: r.Parent})
	case "subagent":
		k := agent.SubagentStarted
		if r.End {
			k = agent.SubagentEnded
		}
		a = c.events(agent.Event{Kind: k, ID: r.ID, Tool: r.Name})
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
		a = c.events(agent.Event{Kind: agent.FileOut, File: r.Path, More: r.More, Caption: r.Caption})
	case "end":
		e := agent.Event{Kind: agent.TurnEnded, Background: r.Background}
		switch r.Status {
		case "failed":
			e.Status, e.Reason = agent.Failed, agent.Reason(r.Reason)
		case "interrupted":
			e.Status = agent.Interrupted
		}
		a = c.events(e)
		a = merge(a, c.ended())
	case "interrupted":
		a = merge(c.events(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted}), c.ended())
	case "cannot-resume":
		a = c.events(agent.Event{Kind: agent.SessionEnded, SessionID: r.Session, Reason: agent.CannotResume})
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

// ended writes the input held during the turn that ended.
func (c *codec) ended() agent.Actions {
	c.running = false
	var a agent.Actions
	for _, in := range c.queue {
		a = merge(a, c.write(in))
	}
	c.queue = nil
	return a
}

func merge(a, b agent.Actions) agent.Actions {
	a.Events = append(a.Events, b.Events...)
	a.Write = append(a.Write, b.Write...)
	a.Reply = append(a.Reply, b.Reply...)
	a.Follow = append(a.Follow, b.Follow...)
	a.Inputs = append(a.Inputs, b.Inputs...)
	return a
}

// Input is one line on pipes, or one bracketed paste and Enter. During a
// turn it is held until the turn ends. Written input is only recorded.
func (c *codec) Input(in agent.Input) (agent.Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case in.Written:
		if in.ID != "" {
			c.waiting = append(c.waiting, &written{id: in.ID})
		}
		return agent.Actions{}, nil
	case c.running:
		c.queue = append(c.queue, in)
		return agent.Actions{}, nil
	}
	return c.write(in), nil
}

// write is what writes in, and records it as waiting for its report.
func (c *codec) write(in agent.Input) agent.Actions {
	var a agent.Actions
	if in.ID != "" {
		a.Inputs = []string{in.ID}
		c.waiting = append(c.waiting, &written{id: in.ID})
		in.Text = inputIDPrefix + in.ID + "\n" + in.Text
	}
	if c.terminal {
		a.Write = [][]byte{[]byte(pasteStart + in.Text + pasteEnd), []byte(pasteKey)}
		return a
	}
	b, _ := json.Marshal(map[string]string{"input": in.Text})
	a.Write = [][]byte{append(b, '\n')}
	return a
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

// Tick ends the wait for written input that was never reported: after the
// input timeout, as failed, "not delivered", naming the input.
func (c *codec) Tick(now time.Time) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	timeout := c.timeout
	if timeout <= 0 {
		timeout = DefaultInputTimeout
	}
	var evs []agent.Event
	kept := c.waiting[:0]
	for _, w := range c.waiting {
		switch {
		case w.at.IsZero():
			w.at = now
		case now.Sub(w.at) >= timeout:
			evs = append(evs, agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.NotDelivered, Input: w.id})
			continue
		}
		kept = append(kept, w)
	}
	c.waiting = kept
	return c.events(evs...)
}
