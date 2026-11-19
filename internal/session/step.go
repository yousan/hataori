// Package session holds the session state machine: the one place where a
// session's state changes, and where turns start and end.
//
// Step is a pure function from (state, input) to (state, events). Machine
// applies it to the store, committing the new state, the turn records and
// the events in one transaction.
package session

import (
	"errors"
	"fmt"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

// InputKind is the kind of an Input.
type InputKind int

// The inputs of the state machine.
const (
	Send         InputKind = iota // API: input from a caller
	Answer                        // API: the answer to an open request
	Close                         // command: close an idle, asleep or failed session; its process is stopped after
	Remove                        // command: mark a closed session removed, once its workspace is in trash
	AgentEvent                    // an event the agent reported
	Exit                          // the agent's process ended
	StartTimeout                  // timer: no SessionReady within the start timeout
	IdleTimeout                   // timer: idle for the idle period
	Wake                          // API: input for an asleep session, which is started again
)

var inputNames = [...]string{"send", "answer", "close", "remove", "agent event", "exit", "start timeout", "idle timeout", "wake"}

func (k InputKind) String() string {
	if int(k) < len(inputNames) {
		return inputNames[k]
	}
	return fmt.Sprintf("input(%d)", int(k))
}

// Input is one thing that happened to a session.
type Input struct {
	Kind      InputKind
	Principal control.Principal // Send, Answer: who, a person
	From      string            // Send, Answer: the "<client>:<id>" it came from, if relayed
	Key       string            // Send, Answer: the call's safe-to-repeat key
	Text      string            // Send, Answer
	Request   string            // Answer: the agent's id of the request answered
	Agent     agent.Event       // AgentEvent
	Reason    string            // Exit: why; empty for "crashed". Close, Remove: what asked for it, such as "command"
	Detail    string            // StartTimeout, Exit: what a person may want to read, such as the end of the terminal log
}

// given is the data of the input or answer event of in: its text, and who
// gave it, since protocol 2.2.
func given(in Input) map[string]any {
	d := map[string]any{"text": in.Text}
	if in.Principal != "" {
		d["principal"] = string(in.Principal)
	}
	if in.From != "" {
		d["from"] = in.From
	}
	return d
}

// Turn-end outcomes and reasons that the state machine adds to the agent's.
const (
	Completion = "completion"
	Blocked    = "blocked"
	Failure    = "failure"

	ReasonCrashed     = string(agent.Crashed)
	ReasonInterrupted = "interrupted"
)

// Who started a turn.
const (
	ByCaller   = "caller"
	ByAgent    = "agent"
	ByTerminal = "terminal"
)

// Event kinds written by the state machine.
const (
	KindState       control.EventKind = "state"
	KindTurnStarted control.EventKind = "turn_started"
	KindTurnEnded   control.EventKind = "turn_ended"
	KindInput       control.EventKind = "input"
	KindAnswer      control.EventKind = "answer"
	KindUsage       control.EventKind = "usage"
)

// Snapshot is what Step needs to know about a session.
type Snapshot struct {
	State control.State
	Turn  *OpenTurn // the running turn, if any
}

// OpenTurn is a turn that has started and not ended.
type OpenTurn struct {
	By        string // ByCaller, ByAgent or ByTerminal
	Principal control.Principal
	From      string // the "<client>:<id>" the input that started it came from
}

// Out is one event Step produces, before it has a position.
type Out struct {
	Kind control.EventKind
	Data map[string]any
}

// Result is the outcome of Step.
type Result struct {
	State control.State
	Start *OpenTurn // a turn to start, after End if both are set
	End   *Ended    // the open turn ends
	Key   string    // the key of the call that started Start
	Out   []Out     // in order; turn events are among them
}

// Ended is how the open turn ended.
type Ended struct {
	Outcome    string // Completion, Blocked or Failure
	Reason     string
	Background int
}

// ErrRefused is returned by Step for an input that is not allowed in a state.
var ErrRefused = errors.New("refused")

// Step returns what input in does to a session in state s. A refused input
// returns an error wrapping ErrRefused, and changes nothing.
func Step(s Snapshot, in Input) (Result, error) {
	r := Result{State: s.State}
	refuse := func() (Result, error) {
		return Result{}, fmt.Errorf("session: %s in state %s: %w", in.Kind, s.State, ErrRefused)
	}
	to := func(st control.State, reason string) {
		d := map[string]any{"from": string(s.State), "to": string(st)}
		if reason != "" {
			d["reason"] = reason
		}
		r.State = st
		r.Out = append(r.Out, Out{Kind: KindState, Data: d})
	}
	end := func(e Ended) {
		r.End = &e
		d := map[string]any{"by": s.Turn.By, "outcome": e.Outcome, "background": e.Background}
		if s.Turn.Principal != "" {
			d["principal"] = string(s.Turn.Principal)
		}
		if s.Turn.From != "" {
			d["from"] = s.Turn.From
		}
		if e.Reason != "" {
			d["reason"] = e.Reason
		}
		r.Out = append(r.Out, Out{Kind: KindTurnEnded, Data: d})
	}
	start := func(t OpenTurn, key string) {
		r.Start, r.Key = &t, key
		d := map[string]any{"by": t.By}
		if t.Principal != "" {
			d["principal"] = string(t.Principal)
		}
		if t.From != "" {
			d["from"] = t.From
		}
		if key != "" {
			d["key"] = key
		}
		r.Out = append(r.Out, Out{Kind: KindTurnStarted, Data: d})
	}
	inTurn := s.State == control.Running && s.Turn != nil

	switch in.Kind {
	case Send:
		switch {
		case s.State == control.Idle:
			to(control.Running, "")
			start(OpenTurn{By: ByCaller, Principal: in.Principal, From: in.From}, in.Key)
			r.Out = append(r.Out, Out{Kind: KindInput, Data: given(in)})
		case inTurn:
			r.Out = append(r.Out, Out{Kind: KindInput, Data: given(in)})
		default:
			return refuse()
		}
	case Answer:
		if s.State != control.Waiting {
			return refuse()
		}
		to(control.Running, "")
		start(OpenTurn{By: ByCaller, Principal: in.Principal, From: in.From}, in.Key)
		r.Out = append(r.Out, Out{Kind: KindAnswer, Data: given(in)})
	case Close:
		// No turn runs, no question is open and no session is starting:
		// closing loses nothing the agent was doing.
		switch s.State {
		case control.Idle, control.Asleep, control.Failed:
		default:
			return refuse()
		}
		to(control.Closed, in.Reason)
	case Remove:
		if s.State != control.Closed {
			return refuse()
		}
		to(control.Removed, in.Reason)
	case StartTimeout:
		if s.State != control.Creating {
			return refuse()
		}
		to(control.Failed, string(agent.BlockedAtStart))
		if in.Detail != "" {
			r.Out[len(r.Out)-1].Data["detail"] = in.Detail
		}
	case IdleTimeout:
		if s.State != control.Idle {
			return refuse()
		}
		to(control.Asleep, "idle")
	case Wake:
		if s.State != control.Asleep {
			return refuse()
		}
		to(control.Creating, "wake")
	case Exit:
		reason := in.Reason
		if reason == "" {
			reason = ReasonCrashed
		}
		switch s.State {
		case control.Creating:
			to(control.Failed, reason)
		case control.Idle, control.Waiting:
			to(control.Asleep, reason)
		case control.Running:
			if s.Turn != nil {
				end(Ended{Outcome: Failure, Reason: reason})
			}
			to(control.Asleep, reason)
		default:
			return refuse()
		}
	case AgentEvent:
		if !stepAgent(s, in.Agent, &r, to, end, start) {
			return refuse()
		}
	default:
		return refuse()
	}
	return r, nil
}

// stepAgent applies an agent event to r, through the helpers of Step. It
// reports false when the event is refused.
func stepAgent(s Snapshot, e agent.Event, r *Result,
	to func(control.State, string), end func(Ended), start func(OpenTurn, string)) bool {
	live := s.State == control.Idle || s.State == control.Running || s.State == control.Waiting
	switch e.Kind {
	case agent.SessionReady:
		if live {
			// The agent goes on under a new session id, as after a person
			// cleared the conversation: the machine stores the id.
			return true
		}
		if s.State != control.Creating && s.State != control.Asleep {
			return false
		}
		to(control.Idle, "")
	case agent.TurnStarted:
		switch s.State {
		case control.Idle:
			by := ByAgent
			if e.StartedBy == agent.ByTerminal {
				by = ByTerminal
			}
			to(control.Running, "")
			start(OpenTurn{By: by}, "")
		case control.Running:
			// The agent confirms the turn a caller started.
		default:
			return false
		}
	case agent.TurnEnded:
		if s.State != control.Running || s.Turn == nil {
			return false
		}
		ended := Ended{Outcome: Completion, Background: e.Background}
		switch e.Status {
		case agent.Interrupted:
			ended.Outcome, ended.Reason = Failure, ReasonInterrupted
		case agent.Failed:
			ended.Outcome, ended.Reason = Failure, string(e.Reason)
			if ended.Reason == "" {
				ended.Reason = string(agent.APIError)
			}
		}
		end(ended)
		to(control.Idle, "")
	case agent.Request:
		if s.State != control.Running || s.Turn == nil || e.Request == nil {
			return false
		}
		r.Out = append(r.Out, agentOut(e))
		end(Ended{Outcome: Blocked})
		to(control.Waiting, "")
	case agent.RequestWithdrawn:
		if s.State != control.Waiting {
			return false
		}
		r.Out = append(r.Out, agentOut(e))
		to(control.Running, "")
		start(OpenTurn{By: ByTerminal}, "")
	case agent.SessionEnded:
		if s.State == control.Creating && e.Reason != "" {
			// It ended before it was ready, for a reason it gave, such as
			// a conversation it cannot resume.
			to(control.Failed, string(e.Reason))
			return true
		}
		if !live {
			return false
		}
	case agent.Unknown:
		if !live {
			return false
		}
	default: // output: text, tools, subagents, files, usage
		if !live {
			return false
		}
		r.Out = append(r.Out, agentOut(e))
	}
	return true
}

var agentKinds = map[agent.Kind]control.EventKind{
	agent.Text:             "text",
	agent.ToolStarted:      "tool_started",
	agent.ToolEnded:        "tool_ended",
	agent.SubagentStarted:  "subagent_started",
	agent.SubagentEnded:    "subagent_ended",
	agent.Request:          "request",
	agent.RequestWithdrawn: "request_withdrawn",
	agent.FileOut:          "file_out",
	agent.Usage:            "usage",
}

// agentOut is the event that carries an agent's output to clients.
func agentOut(e agent.Event) Out {
	d := map[string]any{}
	switch e.Kind {
	case agent.Text:
		d["text"], d["final"] = e.Text, e.Final
	case agent.ToolStarted, agent.ToolEnded:
		d["id"], d["tool"] = e.ID, e.Tool
		if e.Kind == agent.ToolStarted && e.Parent != "" {
			d["parent"] = e.Parent
		}
	case agent.SubagentStarted, agent.SubagentEnded:
		d["id"] = e.ID
		if e.Kind == agent.SubagentStarted && e.Tool != "" {
			d["name"] = e.Tool
		}
	case agent.Request, agent.RequestWithdrawn:
		if e.Request != nil {
			d["request"] = e.Request.ID
			if e.Kind == agent.Request {
				d["text"], d["approval"], d["choices"] = e.Request.Text, e.Request.Approval, e.Request.Choices
			}
		}
	case agent.FileOut:
		d["file"] = e.File
		if len(e.More) > 0 {
			d["more"] = e.More
		}
		if e.Caption != "" {
			d["caption"] = e.Caption
		}
	case agent.Usage:
		if u := e.Usage; u != nil {
			d["input"], d["output"], d["cache_read"], d["cache_write"] = u.Input, u.Output, u.CacheRead, u.CacheWrite
			if u.Model != "" {
				d["model"] = u.Model
			}
			if u.ContextSize > 0 {
				d["context_used"], d["context_size"] = u.ContextUsed, u.ContextSize
			}
		}
	}
	return Out{Kind: agentKinds[e.Kind], Data: d}
}
