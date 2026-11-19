package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

type captured struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	Result struct {
		Thread *struct {
			ID string `json:"id"`
		} `json:"thread"`
	} `json:"result"`
}

func parse(t *testing.T, b []byte) captured {
	t.Helper()
	var m captured
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func send(t *testing.T, lines []wireLine, method string) captured {
	t.Helper()
	for _, l := range lines {
		m := parse(t, l.Msg)
		if l.Dir == "send" && m.Method == method {
			return m
		}
	}
	t.Fatalf("missing %s", method)
	return captured{}
}

func fixture(t *testing.T, lines []wireLine) (agent.Spec, Options, string) {
	t.Helper()
	p := send(t, lines, "thread/start").Params
	turn := send(t, lines, "turn/start").Params
	cfg := p["config"].(map[string]any)
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	return agent.Spec{Dir: str(p, "cwd"), Model: str(p, "model"), Effort: str(cfg, "model_reasoning_effort"), Instructions: str(p, "developerInstructions")}, Options{Sandbox: str(p, "sandbox"), ApprovalPolicy: str(p, "approvalPolicy")}, turn["input"].([]any)[0].(map[string]any)["text"].(string)
}

func clean(evs []agent.Event) []agent.Event {
	var out []agent.Event
	for _, e := range evs {
		if e.Kind != agent.Unknown {
			out = append(out, e)
		}
	}
	return out
}

func splitThread(t *testing.T, lines []wireLine) int {
	t.Helper()
	for i, l := range lines {
		if l.Dir == "recv" && parse(t, l.Msg).Result.Thread != nil {
			return i + 1
		}
	}
	t.Fatal("missing thread result")
	return 0
}

func findMethod(t *testing.T, lines []wireLine, method string) int {
	t.Helper()
	for i, l := range lines {
		if l.Dir == "recv" && parse(t, l.Msg).Method == method {
			return i
		}
	}
	t.Fatal("missing " + method)
	return 0
}

func setup(t *testing.T) (agent.Codec, []wireLine, string) {
	t.Helper()
	lines := replay(t, "start")
	spec, opts, text := fixture(t, lines)
	c := NewCodec(spec, opts)
	c.Open()
	c.Input(agent.Input{ID: "in-1", Text: text})
	return c, lines, text
}

func TestStart(t *testing.T) {
	// Start has only one final message; usage has the same prompt and captures
	// the commentary and answer needed to check the complete event order.
	for _, scenario := range []string{"usage", "start"} {
		t.Run(scenario, func(t *testing.T) {
			lines := replay(t, scenario)
			spec, opts, text := fixture(t, lines)
			c := NewCodec(spec, opts)
			c.Open()
			c.Input(agent.Input{ID: "in-1", Text: text})
			ev := clean(feed(t, c, lines, 0))
			var core []agent.Event
			for _, e := range ev {
				if e.Kind == agent.SessionReady || e.Kind == agent.TurnStarted || e.Kind == agent.Text || e.Kind == agent.TurnEnded {
					core = append(core, e)
				}
			}
			ev = core
			want := []agent.Kind{agent.SessionReady, agent.TurnStarted, agent.Text, agent.TurnEnded}
			if scenario == "usage" {
				want = []agent.Kind{agent.SessionReady, agent.TurnStarted, agent.Text, agent.Text, agent.TurnEnded}
			}
			if len(ev) != len(want) {
				t.Fatalf("events: %+v", ev)
			}
			for i, k := range want {
				if ev[i].Kind != k {
					t.Fatalf("events: %+v", ev)
				}
			}
			if ev[0].SessionID != send(t, lines, "turn/start").Params["threadId"] || ev[1].StartedBy != agent.ByCaller || ev[1].Input != "in-1" || ev[len(ev)-1].Status != agent.Completed {
				t.Fatalf("events: %+v", ev)
			}
			if scenario == "usage" {
				if ev[2].Final || ev[2].Text != "I’ll run `echo start-ok`, then answer the arithmetic question." || !ev[3].Final || ev[3].Text != "5" {
					t.Fatalf("texts: %+v", ev)
				}
			} else if !ev[2].Final {
				t.Fatalf("text: %+v", ev)
			}
		})
	}
}

func TestWrittenBytes(t *testing.T) {
	lines := replay(t, "start")
	spec, opts, text := fixture(t, lines)
	c := NewCodec(spec, opts)
	a := c.Open()
	c.Input(agent.Input{ID: "in-1", Text: text})
	n := splitThread(t, lines)
	var writes [][]byte
	writes = append(writes, a.Write...)
	for _, l := range lines[:n] {
		if l.Dir == "recv" {
			writes = append(writes, c.Decode(agent.Output, l.Msg).Write...)
		}
	}
	if len(writes) != 5 {
		t.Fatalf("writes: %q", writes)
	}
	for i, method := range []string{"initialize", "initialized", "thread/start", "account/rateLimits/read", "turn/start"} {
		got := parse(t, writes[i])
		if method == RateLimitsRead {
			if got.Method != method || len(got.Params) != 0 {
				t.Fatal(got)
			}
			continue
		}
		want := send(t, lines, method)
		if got.Method != want.Method {
			t.Fatalf("%s: %+v", method, got)
		}
		if method == "initialize" {
			if got.Params["capabilities"].(map[string]any)["experimentalApi"] != true || got.Params["clientInfo"].(map[string]any)["name"] != "hataori" {
				t.Fatal(got.Params)
			}
			continue
		}
		if !reflect.DeepEqual(got.Params, want.Params) {
			t.Fatalf("%s: got %v want %v", method, got.Params, want.Params)
		}
	}
}

func TestResume(t *testing.T) {
	lines := replay(t, "resume")
	id := send(t, lines, "thread/resume").Params["threadId"].(string)
	for _, wrong := range []bool{false, true} {
		resume := id
		if wrong {
			resume = "another-thread"
		}
		c := NewCodec(agent.Spec{ResumeID: resume}, Options{})
		a := c.Open()
		text := send(t, lines, "turn/start").Params["input"].([]any)[0].(map[string]any)["text"].(string)
		c.Input(agent.Input{ID: "in-r", Text: text})
		if parse(t, a.Write[2]).Params["threadId"] != resume {
			t.Fatal("resume id missing")
		}
		ev := clean(feed(t, c, lines, 0))
		if wrong {
			if len(ev) == 0 || ev[0].Kind != agent.SessionEnded || ev[0].Reason != agent.WrongSession {
				t.Fatalf("wrong resume: %+v", ev)
			}
			continue
		}
		if len(ev) < 4 || ev[0].Kind != agent.SessionReady || ev[0].SessionID != id {
			t.Fatalf("resume: %+v", ev)
		}
		var started, ended bool
		finals := 0
		for _, e := range ev {
			started = started || (e.Kind == agent.TurnStarted && e.StartedBy == agent.ByCaller && e.Input == "in-r")
			if e.Kind == agent.Text && e.Final {
				finals++
			}
			ended = ended || (e.Kind == agent.TurnEnded && e.Status == agent.Completed)
		}
		if !started || finals != 1 || !ended {
			t.Fatalf("resume turn: %+v", ev)
		}
	}
}

func TestResumeUnknown(t *testing.T) {
	lines := replay(t, "resume_unknown")
	id := send(t, lines, "thread/resume").Params["threadId"].(string)
	c := NewCodec(agent.Spec{ResumeID: id}, Options{})
	c.Open()
	ev := clean(feed(t, c, lines, 0))
	if len(ev) != 1 || ev[0].Kind != agent.SessionEnded || ev[0].Reason != agent.CannotResume {
		t.Fatal(ev)
	}
}

// Removing the Written recording or text matching makes this test fail.
func TestWrittenReported(t *testing.T) {
	lines := replay(t, "start")
	spec, opts, text := fixture(t, lines)
	c := NewCodec(spec, opts)
	n := splitThread(t, lines)
	feed(t, c, lines[:n], 0)
	a, _ := c.Input(agent.Input{ID: "in-w", Text: text, Written: true})
	if len(a.Write) != 0 || len(a.Inputs) != 0 {
		t.Fatal(a)
	}
	ev := clean(feed(t, c, lines[n:], 0))
	found := false
	for _, e := range ev {
		if e.Kind == agent.TurnStarted && e.Input == "in-w" && e.StartedBy == agent.ByCaller {
			found = true
		}
	}
	if !found {
		t.Fatal(ev)
	}
	now := time.Now()
	c.Tick(now)
	if a := c.Tick(now.Add(time.Hour)); len(a.Events) != 0 {
		t.Fatal(a)
	}
}

// Removing the Written deadline tracking makes this test fail.
func TestWrittenTimeout(t *testing.T) {
	c := NewCodec(agent.Spec{}, Options{InputTimeout: time.Second})
	c.Input(agent.Input{ID: "in-x", Text: "unreported", Written: true})
	now := time.Now()
	c.Tick(now)
	if a := c.Tick(now.Add(time.Millisecond)); len(a.Events) != 0 {
		t.Fatal(a)
	}
	a := c.Tick(now.Add(time.Second))
	if len(a.Events) != 1 || a.Events[0].Kind != agent.TurnEnded || a.Events[0].Status != agent.Failed || a.Events[0].Reason != agent.NotDelivered || a.Events[0].Input != "in-x" {
		t.Fatal(a)
	}
	if a = c.Tick(now.Add(time.Hour)); len(a.Events) != 0 {
		t.Fatal(a)
	}
}

func TestHeldInput(t *testing.T) {
	c, lines, _ := setup(t)
	start := findMethod(t, lines, "turn/started")
	end := findMethod(t, lines, "turn/completed")
	feed(t, c, lines[:start+1], 0)
	if a, _ := c.Input(agent.Input{ID: "later", Text: "later"}); len(a.Write) != 0 {
		t.Fatal(a)
	}
	for _, l := range lines[start+1 : end] {
		if l.Dir == "recv" {
			a := c.Decode(agent.Output, l.Msg)
			for _, w := range a.Write {
				if parse(t, w).Method == "turn/start" {
					t.Fatal(a)
				}
			}
		}
	}
	a := c.Decode(agent.Output, lines[end].Msg)
	if len(a.Write) != 1 || len(a.Inputs) != 1 || a.Inputs[0] != "later" {
		t.Fatal(a)
	}
	written := parse(t, a.Write[0])
	if written.Method != "turn/start" {
		t.Fatal(a)
	}
	inputs, ok := written.Params["input"].([]any)
	if !ok || len(inputs) != 1 {
		t.Fatal(a)
	}
	input, ok := inputs[0].(map[string]any)
	if !ok || input["text"] != "later" {
		t.Fatal(a)
	}
}

func TestUnknown(t *testing.T) {
	c, lines, _ := setup(t)
	end := findMethod(t, lines, "turn/completed")
	feed(t, c, lines[:end], 0)
	a := c.Decode(agent.Output, []byte(`{"method":"no/such/method","params":{}}`))
	if len(a.Events) != 1 || a.Events[0].Kind != agent.Unknown {
		t.Fatal(a)
	}
	a = c.Decode(agent.Output, lines[end].Msg)
	if len(a.Events) == 0 || a.Events[len(a.Events)-1].Kind != agent.TurnEnded {
		t.Fatal(a)
	}
}

func TestRebuiltRequestIDAndTurnError(t *testing.T) {
	lines := replay(t, "start")
	spec, opts, text := fixture(t, lines)
	c := NewCodec(spec, opts)
	n := splitThread(t, lines)
	feed(t, c, lines[:n], 0)
	// Change the id of a captured response to exercise the recovered high-water mark.
	var m map[string]any
	for _, l := range lines {
		if l.Dir == "recv" && parse(t, l.Msg).ID == 1 {
			json.Unmarshal(l.Msg, &m)
			break
		}
	}
	m["id"] = float64(100)
	b, _ := json.Marshal(m)
	c.Decode(agent.Output, b)
	a, _ := c.Input(agent.Input{ID: "in-error", Text: text})
	req := parse(t, a.Write[0])
	if req.ID <= 100 {
		t.Fatal(req.ID)
	}
	// Reuse the captured resume error object as a turn/start error.
	errLines := replay(t, "resume_unknown")
	var raw map[string]any
	for _, l := range errLines {
		if strings.Contains(string(l.Msg), "no rollout found") {
			json.Unmarshal(l.Msg, &raw)
			break
		}
	}
	raw["id"] = req.ID
	b, _ = json.Marshal(raw)
	a = c.Decode(agent.Output, b)
	if len(a.Events) != 1 || a.Events[0].Kind != agent.TurnEnded || a.Events[0].Reason != agent.APIError || a.Events[0].Input != "in-error" {
		t.Fatal(a)
	}
}

func TestOtherCaptureReplays(t *testing.T) {
	for _, scenario := range []string{"command", "exit", "usage", "subagent", "question_after_turn"} {
		t.Run(scenario, func(t *testing.T) {
			lines := replay(t, scenario)
			spec, opts, text := fixture(t, lines)
			c := NewCodec(spec, opts)
			c.Open()
			c.Input(agent.Input{ID: "capture", Text: text})
			server := 0
			if scenario == "exit" {
				server = 1
			}
			events := clean(feed(t, c, lines, server))
			if len(events) == 0 || events[0].Kind != agent.SessionReady {
				t.Fatalf("events: %+v", events)
			}
			for i, e := range events {
				if e.Seq == 0 || (i > 0 && e.Seq <= events[i-1].Seq) {
					t.Fatalf("sequence: %+v", events)
				}
			}
		})
	}
}
