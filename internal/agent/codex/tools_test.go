package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// messageCapture reads server messages from a confinement capture.
func messageCapture(t *testing.T, scenario string) []wireLine {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata/0.163.0", scenario, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []wireLine
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		lines = append(lines, wireLine{Dir: "recv", Msg: append([]byte(nil), s.Bytes()...)})
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	return lines
}

// checkPairs checks that each tool start has one matching later end.
func checkPairs(t *testing.T, ev []agent.Event, name string) {
	t.Helper()
	open := map[string]agent.Event{}
	starts := 0
	for _, e := range ev {
		switch e.Kind {
		case agent.ToolStarted:
			if _, ok := open[e.ID]; ok {
				t.Fatalf("duplicate start: %+v", e)
			}
			open[e.ID] = e
			if e.Tool == name {
				starts++
			}
		case agent.ToolEnded:
			v, ok := open[e.ID]
			if !ok || v.Kind != agent.ToolStarted || v.Tool != e.Tool || v.Parent != e.Parent {
				t.Fatalf("unmatched end: %+v", e)
			}
			open[e.ID] = e
		}
	}
	for _, e := range open {
		if e.Kind != agent.ToolEnded {
			t.Fatalf("open tool: %+v", e)
		}
	}
	if starts != 1 {
		t.Fatalf("%s starts = %d: %+v", name, starts, ev)
	}
}

func TestCapturedTools(t *testing.T) {
	for _, tt := range []struct {
		scenario, tool string
		messages       bool
	}{
		{"command", "shell", false},
		{"file_change", "apply_patch", false},
		{"subagent", "wait", false},
		{"workspace_write_mcp", "sv/ping", true},
		{"workspace_write_no_tools_webfetch", "web_search", true},
		{"workspace_write_subagent", "spawnAgent", true},
	} {
		t.Run(tt.scenario, func(t *testing.T) {
			var lines []wireLine
			c := NewCodec(agent.Spec{}, Options{})
			if tt.messages {
				lines = messageCapture(t, tt.scenario) // The server-only capture has no thread result; use its captured main thread ID.
				for _, l := range lines {
					var m struct {
						Params params `json:"params"`
					}
					json.Unmarshal(l.Msg, &m)
					if m.Params.ThreadID != "" {
						c.(*codec).session = m.Params.ThreadID
						break
					}
				}
			} else {
				lines = replay(t, tt.scenario)
				c.Open()
			}
			var ev []agent.Event
			activityCompleted := false
			for _, l := range lines {
				if l.Dir != "recv" {
					continue
				}
				var m struct {
					Method string `json:"method"`
					Params params `json:"params"`
				}
				if err := json.Unmarshal(l.Msg, &m); err != nil {
					t.Fatal(err)
				}
				a := c.Decode(agent.Output, l.Msg)
				firstCompletion := tt.scenario == "subagent" && !activityCompleted && m.Params.Item.Type == "subAgentActivity" && m.Params.Item.Kind == "completed"
				if firstCompletion {
					activityCompleted = true
				}
				foundSubagentEnd := false
				for _, e := range a.Events {
					if e.Kind == agent.ToolEnded && (m.Method != "item/completed" || m.Params.Item.ID != e.ID) {
						t.Fatalf("tool ended on %s for item %q: %+v", m.Method, m.Params.Item.ID, e)
					}
					if tt.scenario == "subagent" && e.Kind == agent.SubagentEnded {
						if !firstCompletion || e.ID != m.Params.Item.AgentThreadID {
							t.Fatalf("subagent ended on wrong line: %+v", m)
						}
						foundSubagentEnd = true
					}
				}
				if firstCompletion && !foundSubagentEnd {
					t.Fatalf("no subagent end on first completion: %+v", a.Events)
				}
				ev = append(ev, a.Events...)
			}
			if tt.scenario == "subagent" && !activityCompleted {
				t.Fatal("missing completed subagent activity")
			}
			checkPairs(t, ev, tt.tool)
			if tt.scenario == "subagent" || tt.scenario == "workspace_write_subagent" {
				var id string
				for _, l := range lines {
					if l.Dir != "recv" {
						continue
					}
					var m struct {
						Method string `json:"method"`
						Params params `json:"params"`
					}
					if err := json.Unmarshal(l.Msg, &m); err != nil {
						t.Fatal(err)
					}
					it := m.Params.Item
					if tt.scenario == "subagent" && it.Type == "subAgentActivity" && it.Kind == "started" {
						id = it.AgentThreadID
					}
					if tt.scenario == "workspace_write_subagent" && m.Method == "item/completed" && it.Type == "collabAgentToolCall" && it.Tool == "spawnAgent" && len(it.ReceiverThreadIDs) > 0 {
						id = it.ReceiverThreadIDs[0]
					}
				}
				if id == "" {
					t.Fatal("missing captured subagent thread")
				}
				started, ended, turnStarts, turnEnds := -1, -1, 0, 0
				for i, e := range ev {
					switch e.Kind {
					case agent.SubagentStarted:
						if started >= 0 || e.ID != id {
							t.Fatal(ev)
						}
						started = i
						if tt.scenario == "subagent" && e.Tool != "echo_subagent" {
							t.Fatal(e)
						}
					case agent.SubagentEnded:
						if tt.scenario == "subagent" {
							if ended >= 0 || e.ID != id || e.Tool != "echo_subagent" {
								t.Fatal(ev)
							}
							ended = i
						}
					case agent.TurnStarted:
						turnStarts++
					case agent.TurnEnded:
						turnEnds++
					case agent.ToolStarted, agent.ToolEnded:
						if tt.scenario == "subagent" && e.Parent != "" {
							t.Fatal(e)
						}
					}
				}
				if started < 0 {
					t.Fatal(ev)
				}
				if tt.scenario == "subagent" && (ended <= started || turnStarts != 1 || turnEnds != 1) {
					t.Fatal(ev)
				}
			}
		})
	}
}

func TestSubagentToolParent(t *testing.T) {
	lines := replay(t, "subagent")
	commands := replay(t, "command")
	c := NewCodec(agent.Spec{}, Options{})
	c.Open()
	var id string
	var events []agent.Event
	injected := false
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		events = append(events, c.Decode(agent.Output, l.Msg).Events...)
		var m map[string]any
		json.Unmarshal(l.Msg, &m)
		p, _ := m["params"].(map[string]any)
		it, _ := p["item"].(map[string]any)
		if !injected && it["type"] == "subAgentActivity" && it["kind"] == "started" {
			id = it["agentThreadId"].(string)
			injected = true
			// No capture has a tool inside a subagent; adapt the captured command lines.
			for _, cl := range commands {
				if cl.Dir != "recv" {
					continue
				}
				var x map[string]any
				json.Unmarshal(cl.Msg, &x)
				cp, _ := x["params"].(map[string]any)
				ci, _ := cp["item"].(map[string]any)
				if ci["type"] == "commandExecution" && (x["method"] == "item/started" || x["method"] == "item/completed") {
					cp["threadId"] = id
					b, _ := json.Marshal(x)
					events = append(events, c.Decode(agent.Output, b).Events...)
				}
			}
		}
	}
	checkPairs(t, events, "shell")
	for _, e := range events {
		if (e.Kind == agent.ToolStarted || e.Kind == agent.ToolEnded) && e.Tool == "shell" && e.Parent != id {
			t.Fatal(e)
		}
	}
}

func TestUnknownCapturedItem(t *testing.T) {
	lines := replay(t, "command")
	c := NewCodec(agent.Spec{}, Options{})
	c.Open()
	changed := 0
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		var m map[string]any
		json.Unmarshal(l.Msg, &m)
		p, _ := m["params"].(map[string]any)
		it, _ := p["item"].(map[string]any)
		if it["type"] == "commandExecution" && (m["method"] == "item/started" || m["method"] == "item/completed") {
			it["type"] = "noSuchItem"
			b, _ := json.Marshal(m)
			a := c.Decode(agent.Output, b)
			for _, e := range a.Events {
				if e.Kind == agent.ToolStarted || e.Kind == agent.ToolEnded {
					t.Fatal(e)
				}
			}
			changed++
		} else {
			a := c.Decode(agent.Output, l.Msg)
			if changed == 2 && m["method"] == "turn/completed" {
				for _, e := range a.Events {
					if e.Kind == agent.TurnEnded {
						return
					}
				}
			}
		}
	}
	t.Fatal("decoding did not continue to turn end")
}

func TestOpenToolEndsBeforeTurn(t *testing.T) {
	lines := replay(t, "command")
	c := NewCodec(agent.Spec{}, Options{})
	c.Open()
	var events []agent.Event
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Params params `json:"params"`
		}
		json.Unmarshal(l.Msg, &m)
		if m.Method == "item/completed" && m.Params.Item.Type == "commandExecution" {
			continue
		}
		events = append(events, c.Decode(agent.Output, l.Msg).Events...)
	}
	checkPairs(t, events, "shell")
	end, turn := -1, -1
	for i, e := range events {
		if e.Kind == agent.ToolEnded {
			end = i
		}
		if e.Kind == agent.TurnEnded {
			turn = i
		}
	}
	if end < 0 || turn <= end {
		t.Fatal(events)
	}
}

func TestSubagentActivityStartsTurn(t *testing.T) {
	lines := replay(t, "subagent")
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		var m struct {
			Params params `json:"params"`
		}
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			t.Fatal(err)
		}
		if m.Params.Item.Type != "subAgentActivity" || m.Params.Item.Kind != "started" {
			continue
		}
		c := NewCodec(agent.Spec{}, Options{}).(*codec)
		c.session = m.Params.ThreadID
		a := c.Decode(agent.Output, l.Msg)
		if len(a.Events) != 2 || a.Events[0].Kind != agent.TurnStarted || a.Events[0].StartedBy != agent.ByAgent || a.Events[1].Kind != agent.SubagentStarted {
			t.Fatal(a.Events)
		}
		return
	}
	t.Fatal("missing subagent activity")
}

func TestSubagentThreadCompletionEndsSubagent(t *testing.T) {
	lines := replay(t, "subagent")
	c := NewCodec(agent.Spec{}, Options{})
	c.Open()
	var id string
	ends := 0
	foundCompletion := false
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Params params `json:"params"`
		}
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			t.Fatal(err)
		}
		it := m.Params.Item
		if it.Type == "subAgentActivity" && it.Kind == "started" {
			id = it.AgentThreadID
		}
		if it.Type == "subAgentActivity" && it.Kind == "completed" {
			continue
		}
		a := c.Decode(agent.Output, l.Msg)
		isCompletion := id != "" && m.Method == "turn/completed" && m.Params.ThreadID == id
		if isCompletion {
			foundCompletion = true
		}
		foundEnd := false
		for _, e := range a.Events {
			if e.Kind == agent.SubagentEnded {
				ends++
				if !isCompletion || e.ID != id || e.Tool != "echo_subagent" {
					t.Fatalf("unexpected end: %+v on %+v", e, m)
				}
				foundEnd = true
			}
		}
		if isCompletion && !foundEnd {
			t.Fatalf("no subagent end on thread completion: %+v", a.Events)
		}
	}
	if id == "" || !foundCompletion || ends != 1 {
		t.Fatalf("id %q, completion %v, ends %d", id, foundCompletion, ends)
	}
}
