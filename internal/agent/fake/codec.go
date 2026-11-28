package fake

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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
	early    []agent.Event     // text read from the transcript before the hook call that starts its turn
	turn     int               // the number of the last turn reported since the program started
	queue    []agent.Input     // input held until the turn ends
	waiting  []*written        // inputs written and not yet reported, with an Input.ID
	timeout  time.Duration     // for waiting; DefaultInputTimeout when zero

	dialog   string   // the start-up dialog to answer, if any
	trust    []string // the files the dialog names, when the session may not trust them all: not answered
	screen   []byte   // the screen so far
	answered bool     // Enter was sent for the dialog
	started  bool     // a hook was called: the dialog is past
	opened   bool     // Open was called: a codec rebuilt after a restart is not
	stopped  bool     // the watch ended: the screen must not come any more
	after    []byte   // the screen the codec was given after it decoded the start

	// A session a person started and hataori took in (agent.Spec.TakenIn).
	takenIn bool
	session string
	model   string
}

// written is an input written and not yet reported.
type written struct {
	id           string
	at           time.Time // the first Tick after writing it
	awaitReceipt bool
}

func (c *codec) ReconcileHooks(waiting []string) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.terminal {
		return agent.Actions{}
	}
	var events []agent.Event
	for id := range c.asked {
		if !slices.Contains(waiting, id) {
			delete(c.asked, id)
			events = append(events, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: id}})
		}
	}
	return c.events(events...)
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
	Choices    []string `json:"choices"` // ask: its options, in order
	Multi      bool     `json:"multi"`   // ask: several options may be chosen
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
	Prompt     string   `json:"prompt"` // turn by the terminal: the line typed
	N          int      `json:"n"`      // turn: its number since the program started
	Turn       int      `json:"turn"`   // text in the transcript: the number of its turn
	// usage; "input" is the turn's input id above, so the counts are "in"
	Tokens
	// limits
	At      int64 `json:"at"`    // Unix ms
	Every   int64 `json:"every"` // seconds
	Windows []struct {
		Name    string `json:"name"`
		Minutes int64  `json:"minutes"`
		Used    int    `json:"used"`
		Resets  int64  `json:"resets"` // Unix seconds; 0 for none
	} `json:"windows"`
}

// Tokens are the counts of a usage report.
type Tokens struct {
	In          int64 `json:"in"`
	Late        bool  `json:"late"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheWrite  int64 `json:"cache_write"`
	ContextUsed int64 `json:"context_used"`
	ContextSize int64 `json:"context_size"`
}

// call is a hook call as the session host records it.
type call struct {
	Hook    string          `json:"hook"`
	Wait    bool            `json:"wait"`
	Payload json.RawMessage `json:"payload"`
}

// Open watches the screen when the fake shows a start-up dialog.
func (c *codec) Open() agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened = true
	if c.takenIn {
		return c.events(agent.Event{Kind: agent.SessionReady, SessionID: c.session, Model: c.model})
	}
	return agent.Actions{WatchScreen: c.dialog != ""}
}

// onScreen answers the dialog with Enter, once, when the screen shows it
// before the start. It watches on, as the Claude Code codec does for its
// first input, until its first turn ends. The screen after that, or
// without a dialog to watch for, must never reach the codec.
func (c *codec) onScreen(b []byte) agent.Actions {
	if c.dialog == "" || !c.opened {
		panic("fake: the codec was given the screen without asking for it")
	}
	if c.stopped {
		panic("fake: the codec was given the screen after it stopped watching")
	}
	c.screen = append(c.screen, b...)
	if c.started {
		c.after = append(c.after, b...)
	}
	if c.answered || c.started || !bytes.Contains(c.screen, []byte(c.dialog)) {
		return agent.Actions{}
	}
	c.answered = true
	if len(c.trust) > 0 {
		return c.events(agent.Event{Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Trust: slices.Clone(c.trust),
			Text: "the dialog names " + strings.Join(c.trust, ", ") + "; this session may not trust them"})
	}
	return agent.Actions{Keys: []agent.Part{{Key: agent.Enter}}}
}

// ScreenAfterStart returns the screen the codec was given after it decoded
// the session's start: what the runner kept giving it past SessionReady.
func ScreenAfterStart(c agent.Codec) []byte {
	f := c.(*codec)
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.after)
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
	// In a person's window every report names the transcript: the "ready"
	// one went to the dashboard's list before the take-in.
	var follow []string
	if r.T != "ready" && r.Transcript != "" && c.terminal && !c.followed[r.Transcript] {
		c.followed[r.Transcript] = true
		follow = []string{r.Transcript}
	}
	switch r.T {
	case "ready":
		id := r.Session
		if r.Resume != "" {
			id = r.Resume
		}
		a = c.events(agent.Event{Kind: agent.SessionReady, SessionID: id, Model: r.Model, Effort: r.Effort})
		c.turn, c.early = 0, nil // a program that started again counts from 1
		if r.Transcript != "" && c.terminal && !c.followed[r.Transcript] {
			c.followed[r.Transcript] = true
			a.Follow = []string{r.Transcript}
		}
	case "turn":
		by := agent.ByCaller
		switch r.By {
		case "agent":
			by = agent.ByAgent
		case "terminal":
			by = agent.ByTerminal
		}
		started := agent.Event{Kind: agent.TurnStarted, StartedBy: by, Input: r.Input}
		if by == agent.ByTerminal {
			started.Text = r.Prompt // the prompt typed, as Claude Code's hook gives it
		}
		a = c.events(append([]agent.Event{started}, c.early...)...)
		c.early, c.turn = nil, r.N
		c.running = true
		for i, w := range c.waiting {
			if w.id == r.Input {
				c.waiting = append(c.waiting[:i], c.waiting[i+1:]...)
				break
			}
		}
	case "text":
		e := agent.Event{Kind: agent.Text, Text: r.Text, Final: r.Final}
		if c.terminal && src == agent.File && r.Turn > c.turn {
			// The runner gives the transcript before the hook calls of one
			// step, so the text of a turn that no caller started (typed at
			// the terminal, or the agent's own) can come before the call
			// that starts it. It belongs to that turn, as the Claude Code
			// codec also holds it.
			c.early = append(c.early, e)
			return agent.Actions{}
		}
		a = c.events(e)
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
		a = c.events(agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: id, Text: r.Text, Choices: r.Choices, Multi: r.Multi}})
	case "usage":
		a = c.events(agent.Event{Kind: agent.Usage, Late: r.Late, Usage: &agent.UsageCount{Model: r.Model, Input: r.In, Output: r.Output,
			CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, ContextUsed: r.ContextUsed, ContextSize: r.ContextSize}})
	case "limits":
		l := &agent.LimitReport{At: time.UnixMilli(r.At), Every: time.Duration(r.Every) * time.Second}
		for _, w := range r.Windows {
			lw := agent.LimitWindow{Name: w.Name, Minutes: w.Minutes, Used: w.Used}
			if w.Resets > 0 {
				lw.Resets = time.Unix(w.Resets, 0)
			}
			l.Windows = append(l.Windows, lw)
		}
		a = c.events(agent.Event{Kind: agent.Limits, Limits: l})
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
		if c.opened && c.dialog != "" && !c.stopped { // only a watch it asked for
			c.stopped, a.StopScreen = true, true // the first turn ended: the screen is not needed
		}
	case "interrupted":
		a = merge(c.events(agent.Event{Kind: agent.TurnEnded, Status: agent.Interrupted}), c.ended())
	case "cannot-resume":
		a = c.events(agent.Event{Kind: agent.SessionEnded, SessionID: r.Session, Reason: agent.CannotResume})
	case "session-end":
		a = c.events(agent.Event{Kind: agent.SessionEnded, SessionID: r.Session, Reason: agent.Reason(r.Reason)})
	default:
		a = c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
	}
	a.Follow = append(follow, a.Follow...)
	return a
}

func (c *codec) events(evs ...agent.Event) agent.Actions {
	for i := range evs {
		c.seq++
		evs[i].Seq = c.seq
	}
	return agent.Actions{Events: evs}
}

// The keys the codec presses on a terminal: Enter after a paste, Escape to
// interrupt.
var (
	pasteKey  = agent.Part{Key: agent.Enter}
	escapeKey = agent.Part{Key: agent.Escape}
)

// ended writes the input held during the turn that ended.
func (c *codec) ended() agent.Actions {
	c.running = false
	var a agent.Actions
	for i, in := range c.queue {
		a = merge(a, c.write(in))
		if in.IdleOnly {
			c.queue = c.queue[i+1:]
			return a // one complete deferred submission, never mixed with later input
		}
	}
	c.queue = nil
	return a
}

func merge(a, b agent.Actions) agent.Actions {
	a.StopScreen = a.StopScreen || b.StopScreen
	a.Events = append(a.Events, b.Events...)
	a.Keys = append(a.Keys, b.Keys...)
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
			found := false
			for _, w := range c.waiting {
				if w.id == in.ID {
					w.at = in.WrittenAt
					w.awaitReceipt = in.IdleOnly && in.WrittenAt.IsZero()
					found = true
					break
				}
			}
			if !found {
				c.waiting = append(c.waiting, &written{id: in.ID, at: in.WrittenAt, awaitReceipt: in.IdleOnly && in.WrittenAt.IsZero()})
			}
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
		c.waiting = append(c.waiting, &written{id: in.ID, at: in.WrittenAt})
		in.Text = inputIDPrefix + in.ID + "\n" + in.Text
	}
	if c.terminal {
		a.Keys = []agent.Part{{Paste: in.Text}, pasteKey}
		return a
	}
	b, _ := json.Marshal(map[string]string{"input": in.Text})
	a.Write = [][]byte{append(b, '\n')}
	return a
}

// ErrNotAsked is returned by Answer for a question that is not open.
var ErrNotAsked = errors.New(Name + ": no such question is open")

// Answer replies to the hook call on a terminal, or writes the answer on
// pipes. An answer with chosen options is their tokens, joined by ", ",
// as the Claude Code adapter answers: the choices win over any text.
func (c *codec) Answer(a agent.Answer) (agent.Actions, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.asked[a.Request]
	if !ok {
		return agent.Actions{}, ErrNotAsked
	}
	delete(c.asked, a.Request)
	text := a.Text
	if len(a.Choices) > 0 {
		text = strings.Join(a.Choices, ", ")
	}
	if c.terminal {
		return agent.Actions{Reply: []agent.HookReply{{Call: a.Request, Body: []byte(text)}}}, nil
	}
	b, _ := json.Marshal(map[string]any{"answer": map[string]string{"id": q, "text": text}})
	return agent.Actions{Write: [][]byte{append(b, '\n')}}, nil
}

// Interrupt stops the running turn: Escape on a terminal.
func (c *codec) Interrupt() agent.Actions {
	if c.terminal {
		return agent.Actions{Keys: []agent.Part{escapeKey}}
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
		if w.awaitReceipt {
			kept = append(kept, w)
			continue
		}
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
	a := c.events(evs...)
	if !c.running && len(c.queue) != 0 {
		a = merge(a, c.ended())
	}
	return a
}
