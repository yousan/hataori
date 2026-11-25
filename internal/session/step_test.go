package session

import (
	"errors"
	"slices"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

var allStates = []control.State{control.Creating, control.Idle, control.Running, control.Waiting,
	control.Asleep, control.Closed, control.Removed, control.Failed}

type namedInput struct {
	name string
	in   Input
}

func agentIn(k agent.Kind) Input {
	e := agent.Event{Kind: k}
	if k == agent.Request {
		e.Request = &agent.RequestInfo{ID: "r1"}
	}
	return Input{Kind: AgentEvent, Agent: e}
}

var allInputs = []namedInput{
	{"send", Input{Kind: Send}},
	{"answer", Input{Kind: Answer}},
	{"close", Input{Kind: Close}},
	{"remove", Input{Kind: Remove}},
	{"exit", Input{Kind: Exit}},
	{"start timeout", Input{Kind: StartTimeout}},
	{"idle timeout", Input{Kind: IdleTimeout}},
	{"wake", Input{Kind: Wake}},
	{"cannot resume", Input{Kind: AgentEvent, Agent: agent.Event{Kind: agent.SessionEnded, Reason: agent.CannotResume}}},
	{"ready", agentIn(agent.SessionReady)},
	{"turn started", agentIn(agent.TurnStarted)},
	{"turn ended", agentIn(agent.TurnEnded)},
	{"request", agentIn(agent.Request)},
	{"withdrawn", agentIn(agent.RequestWithdrawn)},
	{"text", agentIn(agent.Text)},
	{"tool started", agentIn(agent.ToolStarted)},
	{"tool ended", agentIn(agent.ToolEnded)},
	{"subagent started", agentIn(agent.SubagentStarted)},
	{"subagent ended", agentIn(agent.SubagentEnded)},
	{"file out", agentIn(agent.FileOut)},
	{"usage", agentIn(agent.Usage)},
	{"session ended", agentIn(agent.SessionEnded)},
	{"unknown", agentIn(agent.Unknown)},
}

// output are the agent events that change no state; they are allowed
// wherever an agent process is live.
var output = []string{"text", "tool started", "tool ended", "subagent started", "subagent ended",
	"file out", "usage", "session ended", "unknown"}

func init() {
	for _, st := range []control.State{control.Idle, control.Running, control.Waiting} {
		for _, o := range output {
			allowed[st][o] = st
		}
		allowed[st]["cannot resume"] = st // a session end like any other once it is live
		allowed[st]["ready"] = st         // a new session id, as after a clear
	}
}

// allowed lists every arrow: state, input → new state. Everything else is refused.
var allowed = map[control.State]map[string]control.State{
	control.Creating: {"ready": control.Idle, "exit": control.Failed, "start timeout": control.Failed,
		"cannot resume": control.Failed},
	control.Idle: {"send": control.Running, "turn started": control.Running, "idle timeout": control.Asleep,
		"exit": control.Asleep, "close": control.Closed},
	control.Running: {"send": control.Running, "turn started": control.Running, "turn ended": control.Idle,
		"request": control.Waiting, "exit": control.Asleep},
	control.Waiting: {"answer": control.Running, "withdrawn": control.Running, "exit": control.Asleep},
	control.Asleep:  {"ready": control.Idle, "close": control.Closed, "wake": control.Creating},
	control.Closed:  {"remove": control.Removed},
	control.Failed:  {"close": control.Closed},
}

func TestEveryStateAndInput(t *testing.T) {
	for _, st := range allStates {
		for _, ni := range allInputs {
			snap := Snapshot{State: st}
			if st == control.Running {
				snap.Turn = &OpenTurn{By: ByCaller}
			}
			r, err := Step(snap, ni.in)
			want, ok := allowed[st][ni.name]
			if !ok {
				if !errors.Is(err, ErrRefused) {
					t.Errorf("%s + %s: got %v, %v; want refused", st, ni.name, r.State, err)
				}
				if r.State != "" || r.Out != nil || r.Start != nil || r.End != nil {
					t.Errorf("%s + %s: refused input produced %+v", st, ni.name, r)
				}
				continue
			}
			if err != nil || r.State != want {
				t.Errorf("%s + %s: got %v, %v; want %v", st, ni.name, r.State, err, want)
			}
		}
	}
}

// A chat front end replies to the message that started a turn, so the
// turn's start names the key of that call.
func TestTurnStartedCarriesTheKey(t *testing.T) {
	r, err := Step(Snapshot{State: control.Idle}, Input{Kind: Send, Principal: "p", Key: "m1", Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range r.Out {
		if o.Kind == KindTurnStarted {
			if o.Data["key"] != "m1" {
				t.Fatalf("turn_started %v, want key m1", o.Data)
			}
			return
		}
	}
	t.Fatalf("no turn_started in %+v", r.Out)
}

// A chat front end names a subagent and counts its tool calls.
func TestSubagentNameAndToolParentReachTheEvents(t *testing.T) {
	if o := agentOut(agent.Event{Kind: agent.SubagentStarted, ID: "s1", Tool: "explorer"}); o.Data["name"] != "explorer" {
		t.Fatalf("subagent_started %v", o.Data)
	}
	if o := agentOut(agent.Event{Kind: agent.ToolStarted, ID: "t1", Tool: "Read", Parent: "s1"}); o.Data["parent"] != "s1" {
		t.Fatalf("tool_started %v", o.Data)
	}
	if o := agentOut(agent.Event{Kind: agent.ToolStarted, ID: "t2", Tool: "Read"}); o.Data["parent"] != nil {
		t.Fatalf("tool_started of the agent's own tool %v", o.Data)
	}
}

// A chat front end closes the subagents that a turn's end does not list.
func TestATurnsEndListsTheSubagentsThatStillRun(t *testing.T) {
	for _, st := range []agent.EndStatus{agent.Completed, agent.Interrupted, agent.Failed} {
		for _, ids := range [][]string{{"s1", "s2"}, nil} {
			r, err := Step(Snapshot{State: control.Running, Turn: &OpenTurn{By: ByCaller}}, Input{Kind: AgentEvent,
				Agent: agent.Event{Kind: agent.TurnEnded, Status: st, Background: 3, Subagents: ids}})
			if err != nil || len(r.Out) == 0 || r.Out[0].Kind != KindTurnEnded {
				t.Fatalf("status %v: result %+v, error %v", st, r, err)
			}
			got, _ := r.Out[0].Data["subagents"].([]string)
			if _, set := r.Out[0].Data["subagents"]; !slices.Equal(got, ids) || set != (len(ids) > 0) {
				t.Errorf("status %v: turn_ended %v, want the subagents %v", st, r.Out[0].Data, ids)
			}
		}
	}
}

func TestFileOutCarriesMoreAndCaption(t *testing.T) {
	o := agentOut(agent.Event{Kind: agent.FileOut, File: "/w/a", More: []string{"/w/b"}, Caption: "two"})
	if o.Data["file"] != "/w/a" || o.Data["caption"] != "two" || len(o.Data["more"].([]string)) != 1 {
		t.Fatalf("file_out %v", o.Data)
	}
}

func TestInputTakenHasNoOutput(t *testing.T) {
	r, err := Step(Snapshot{State: control.Running, Turn: &OpenTurn{By: ByCaller}}, Input{Kind: AgentEvent, Agent: agent.Event{Kind: agent.InputTaken, Input: "1"}})
	if err != nil || r.State != control.Running || len(r.Out) != 0 {
		t.Fatalf("result %+v, error %v", r, err)
	}
}

// A session that ends before it is ready at a start-up dialog it may not
// answer fails as blocked at start, with the agent's words for a person
// (#423).
func TestBlockedAtStartByTheAgentKeepsItsDetail(t *testing.T) {
	const why = "the folder-trust dialog says trusting the folder loads .claude/settings.json"
	r, err := Step(Snapshot{State: control.Creating}, Input{Kind: AgentEvent,
		Agent: agent.Event{Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Text: why}})
	if err != nil || r.State != control.Failed || len(r.Out) != 1 {
		t.Fatalf("result %+v, error %v", r, err)
	}
	if d := r.Out[0].Data; d["reason"] != string(agent.BlockedAtStart) || d["detail"] != why {
		t.Fatalf("state event %v", d)
	}
}

// A turn typed at the terminal writes its prompt as an input event of the
// person at the terminal, with via terminal: after its turn_started when it
// starts the turn, and also while a turn a caller started runs, which the
// typed prompt reached first. A caller's turn and the agent's own write
// none (#609).
func TestTypedPromptIsInput(t *testing.T) {
	typed := func(s Snapshot, e agent.Event) []Out {
		t.Helper()
		r, err := Step(s, Input{Kind: AgentEvent, Principal: "ada", Agent: e})
		if err != nil {
			t.Fatal(err)
		}
		return r.Out
	}
	inputs := func(out []Out) []map[string]any {
		var in []map[string]any
		for _, o := range out {
			if o.Kind == KindInput {
				in = append(in, o.Data)
			}
		}
		return in
	}
	out := typed(Snapshot{State: control.Idle}, agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByTerminal, Text: "fix it"})
	if len(out) < 2 || out[len(out)-1].Kind != KindInput || out[len(out)-2].Kind != KindTurnStarted {
		t.Fatalf("a typed turn from idle: %+v, want turn_started then input", out)
	}
	if d := inputs(out); len(d) != 1 || d[0]["text"] != "fix it" || d[0]["via"] != ViaTerminal || d[0]["principal"] != "ada" {
		t.Fatalf("the typed prompt's input %v", d)
	}
	running := Snapshot{State: control.Running, Turn: &OpenTurn{By: ByCaller}}
	if d := inputs(typed(running, agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByTerminal, Text: "first"})); len(d) != 1 || d[0]["text"] != "first" {
		t.Fatalf("a typed prompt while a caller's turn runs: %v", d)
	}
	for _, by := range []agent.Starter{agent.ByCaller, agent.ByAgent} {
		if d := inputs(typed(Snapshot{State: control.Idle}, agent.Event{Kind: agent.TurnStarted, StartedBy: by, Text: "not typed"})); len(d) != 0 {
			t.Fatalf("a turn by %v writes input %v", by, d)
		}
	}
}
