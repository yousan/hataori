package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

const planText = "What would you like help with?\n- Plan a change (Recommended): Explore the project and prepare a concrete implementation plan.\n- Explain something: Answer a question or clarify a topic.\n- Review work: Inspect existing code, writing, or a design."

var planChoices = []string{"Plan a change (Recommended)", "Explain something", "Review work"}
var colourChoices = []string{"Blue", "Green", "Red", "Purple"}

// requestReplay feeds a capture to a codec and tracks its request IDs.
type requestReplay struct {
	t            *testing.T
	lines        []wireLine
	c            agent.Codec
	ids          map[int]int
	pos          int
	skipResolved bool
}

// newRequestReplay opens a codec and queues the capture’s input.
func newRequestReplay(t *testing.T, name string, policy agent.Policy) *requestReplay {
	t.Helper()
	lines := replay(t, name)
	spec, opts, text := fixture(t, lines)
	spec.Approvals = policy
	c := NewCodec(spec, opts)
	r := &requestReplay{t: t, lines: lines, c: c, ids: map[int]int{}}
	r.writes(c.Open())
	a, err := c.Input(agent.Input{ID: "first", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	r.writes(a)
	return r
}

// writes records the IDs the codec assigned to its requests.
func (r *requestReplay) writes(a agent.Actions) {
	for _, b := range a.Write {
		m := parse(r.t, b)
		if m.Method == "" {
			continue
		}
		for _, l := range r.lines {
			if l.Dir == "send" {
				captured := parse(r.t, l.Msg)
				if captured.Method == m.Method {
					r.ids[captured.ID] = m.ID
					break
				}
			}
		}
	}
}

// next feeds the next captured server message to the codec.
func (r *requestReplay) next() (message, agent.Actions, bool) {
	r.t.Helper()
	for r.pos < len(r.lines) {
		l := r.lines[r.pos]
		r.pos++
		if l.Dir != "recv" {
			continue
		}
		var m message
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			r.t.Fatal(err)
		}
		if r.skipResolved && m.Method == "serverRequest/resolved" {
			continue
		}
		raw := l.Msg
		if m.Method == "" && len(m.ID) > 0 {
			var id int
			if json.Unmarshal(m.ID, &id) == nil {
				if n, ok := r.ids[id]; ok {
					var v map[string]any
					_ = json.Unmarshal(raw, &v)
					v["id"] = n
					raw, _ = json.Marshal(v)
				}
			}
		}
		a := r.c.Decode(agent.Output, raw)
		r.writes(a)
		return m, a, true
	}
	return message{}, agent.Actions{}, false
}

// responseFor finds the captured response for a server request.
func responseFor(t *testing.T, lines []wireLine, id json.RawMessage) []byte {
	t.Helper()
	for _, l := range lines {
		if l.Dir != "send" {
			continue
		}
		var m message
		_ = json.Unmarshal(l.Msg, &m)
		if m.Method == "" && compactID(m.ID) == compactID(id) {
			var b bytes.Buffer
			if err := json.Compact(&b, l.Msg); err != nil {
				t.Fatal(err)
			}
			return append(b.Bytes(), '\n')
		}
	}
	t.Fatalf("missing response for %s", id)
	return nil
}

// requestEvent finds a request in the codec’s actions.
func requestEvent(t *testing.T, a agent.Actions) *agent.RequestInfo {
	t.Helper()
	for _, e := range a.Events {
		if e.Kind == agent.Request {
			return e.Request
		}
	}
	return nil
}

// checkWrite checks a single exact output line.
func checkWrite(t *testing.T, a agent.Actions, want []byte) {
	t.Helper()
	if len(a.Write) != 1 || !bytes.Equal(a.Write[0], want) {
		t.Fatalf("writes %q, want %q", a.Write, want)
	}
}

// checkMethod checks a request’s method and parameters.
func checkMethod(t *testing.T, a agent.Actions, want captured) captured {
	t.Helper()
	if len(a.Write) != 1 {
		t.Fatalf("writes: %q", a.Write)
	}
	got := parse(t, a.Write[0])
	if got.Method != want.Method || !reflect.DeepEqual(got.Params, want.Params) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	return got
}

// captureSend finds the nth captured client request of a method.
func captureSend(t *testing.T, lines []wireLine, method string, n int) captured {
	t.Helper()
	for _, l := range lines {
		if l.Dir == "send" {
			m := parse(t, l.Msg)
			if m.Method == method {
				if n == 0 {
					return m
				}
				n--
			}
		}
	}
	t.Fatalf("missing send %s", method)
	return captured{}
}

func TestApprovalsAskInChat(t *testing.T) {
	for _, name := range []string{"approval_accept", "approval_decline"} {
		t.Run(name, func(t *testing.T) {
			r := newRequestReplay(t, name, agent.AskInChat)
			count := 0
			for {
				m, a, ok := r.next()
				if !ok {
					break
				}
				if q := requestEvent(t, a); q != nil {
					want := []string{"/usr/bin/zsh -lc 'touch approved.txt'", "/tmp/capture-work/patch.txt"}
					if count >= len(want) || !q.Approval || q.Text != want[count] || len(q.Choices) != 0 || len(a.Write) != 0 {
						t.Fatalf("request %+v actions %+v", q, a)
					}
					count++
					reply, err := r.c.Answer(agent.Answer{Request: q.ID, Allow: name == "approval_accept"})
					if err != nil {
						t.Fatal(err)
					}
					checkWrite(t, reply, responseFor(t, r.lines, m.ID))
				}
			}
			if count != 2 {
				t.Fatalf("requests: %d", count)
			}
		})
	}
}
func TestApprovalsAllowAll(t *testing.T) {
	for _, name := range []string{"approval_accept", "approval_decline"} {
		t.Run(name, func(t *testing.T) {
			r := newRequestReplay(t, name, agent.AllowAll)
			count := 0
			for {
				m, a, ok := r.next()
				if !ok {
					break
				}
				if requestEvent(t, a) != nil {
					t.Fatal(a)
				}
				if m.Method == "serverRequest/resolved" && len(a.Events) != 0 {
					t.Fatal(a.Events)
				}
				if m.Method == "item/commandExecution/requestApproval" || m.Method == "item/fileChange/requestApproval" {
					count++
					want := []byte(fmt.Sprintf("{\"id\":%s,\"result\":{\"decision\":\"accept\"}}\n", compactID(m.ID)))
					checkWrite(t, a, want)
					if name == "approval_accept" && !bytes.Equal(want, responseFor(t, r.lines, m.ID)) {
						t.Fatal("capture differs")
					}
					var p struct {
						ItemID string `json:"itemId"`
					}
					_ = json.Unmarshal(m.Params, &p)
					if _, err := r.c.Answer(agent.Answer{Request: p.ItemID}); !errors.Is(err, ErrNotWaiting) {
						t.Fatal(err)
					}
				}
			}
			if count != 2 {
				t.Fatal(count)
			}
		})
	}
}

func TestAsyncQuestions(t *testing.T) {
	for _, name := range []string{"question_async", "question_after_turn"} {
		t.Run(name, func(t *testing.T) {
			r := newRequestReplay(t, name, agent.AllowAll)
			var req *agent.RequestInfo
			var before, after []agent.Event
			answered := false
			for {
				m, a, ok := r.next()
				if !ok {
					break
				}
				if answered {
					after = append(after, a.Events...)
				} else {
					before = append(before, a.Events...)
				}
				if q := requestEvent(t, a); q != nil {
					req = q
					var p params
					_ = json.Unmarshal(m.Params, &p)
					if q.ID != p.Item.ID || q.Text != "Which colour do you like?" || q.Approval || !reflect.DeepEqual(q.Choices, colourChoices) {
						t.Fatal(q)
					}
				}
				shouldAnswer := req != nil && !answered && (name == "question_async" || m.Method == "turn/completed")
				if shouldAnswer {
					reply, err := r.c.Answer(agent.Answer{Request: req.ID, Text: "blue"})
					if err != nil {
						t.Fatal(err)
					}
					method, index := "turn/steer", 0
					if name == "question_after_turn" {
						method, index = "turn/start", 1
					}
					got := checkMethod(t, reply, captureSend(t, r.lines, method, index))
					if name == "question_async" && len(reply.Inputs) != 0 {
						t.Fatal(reply.Inputs)
					}
					original := captureSend(t, r.lines, method, index)
					r.ids[original.ID] = got.ID
					answered = true
				}
			}
			if req == nil || !answered {
				t.Fatal("missing question")
			}
			all := append(append([]agent.Event{}, before...), after...)
			for _, e := range all {
				if e.Kind == agent.RequestWithdrawn || (e.Kind == agent.Text && strings.Contains(e.Text, "- Blue")) {
					t.Fatal(e)
				}
			}
			final := "You like blue."
			if name == "question_after_turn" {
				final = "Blue."
				assertEvents(t, before, "I haven’t received your answer yet. Which colour do you like?", false)
				requestAt, textAt := -1, -1
				for i, e := range before {
					if e.Kind == agent.Request {
						requestAt = i
					}
					if e.Kind == agent.Text && e.Final {
						textAt = i
					}
				}
				if requestAt < 0 || requestAt >= textAt {
					t.Fatalf("events %+v", before)
				}
			}
			assertEvents(t, after, final, name == "question_after_turn")
		})
	}
	t.Run("steer_refused", func(t *testing.T) {
		r := newRequestReplay(t, "question_async", agent.AllowAll)
		var steer captured
		for {
			_, a, ok := r.next()
			if !ok {
				t.Fatal("missing request")
			}
			if q := requestEvent(t, a); q != nil {
				reply, err := r.c.Answer(agent.Answer{Request: q.ID, Text: "blue"})
				if err != nil {
					t.Fatal(err)
				}
				steer = parse(t, reply.Write[0])
				break
			}
		}
		// Reuse the captured resume error as a turn/steer error.
		var raw map[string]any
		for _, l := range replay(t, "resume_unknown") {
			if strings.Contains(string(l.Msg), "no rollout found") {
				_ = json.Unmarshal(l.Msg, &raw)
				break
			}
		}
		raw["id"] = steer.ID
		b, _ := json.Marshal(raw)
		if a := r.c.Decode(agent.Output, b); len(a.Write) != 0 {
			t.Fatal(a)
		}
		for {
			m, a, ok := r.next()
			if !ok {
				t.Fatal("missing turn end")
			}
			if m.Method == "turn/completed" {
				if len(a.Write) != 1 {
					t.Fatal(a)
				}
				got := parse(t, a.Write[0])
				input := got.Params["input"].([]any)[0].(map[string]any)
				if got.Method != "turn/start" || input["text"] != "blue" {
					t.Fatal(got)
				}
				break
			}
		}
	})
}

// assertEvents checks the final text, turn end, and caller start.
func assertEvents(t *testing.T, ev []agent.Event, final string, caller bool) {
	t.Helper()
	texts, ends, starts := 0, 0, 0
	finalAt, endAt, startAt := -1, -1, -1
	for i, e := range ev {
		if e.Kind == agent.Text && e.Final && e.Text == final {
			texts++
			finalAt = i
		}
		if e.Kind == agent.TurnEnded {
			ends++
			endAt = i
		}
		if e.Kind == agent.TurnStarted && e.StartedBy == agent.ByCaller {
			starts++
			startAt = i
		}
	}
	if texts != 1 || ends != 1 || finalAt >= endAt || (caller && (starts != 1 || startAt >= finalAt)) {
		t.Fatalf("events %+v", ev)
	}
}

func TestPlanQuestion(t *testing.T) {
	for _, choice := range []bool{true, false} {
		name := "text"
		if choice {
			name = "choices"
		}
		t.Run(name, func(t *testing.T) {
			r := newRequestReplay(t, "question_plan", agent.AllowAll)
			for {
				m, a, ok := r.next()
				if !ok {
					t.Fatal("missing request")
				}
				if q := requestEvent(t, a); q != nil {
					if q.Approval || q.Text != planText || !reflect.DeepEqual(q.Choices, planChoices) {
						t.Fatal(q)
					}
					answer := agent.Answer{Request: q.ID}
					if choice {
						answer.Choices = []string{planChoices[0]}
					} else {
						answer.Text = planChoices[0]
					}
					reply, err := r.c.Answer(answer)
					if err != nil {
						t.Fatal(err)
					}
					checkWrite(t, reply, responseFor(t, r.lines, m.ID))
					return
				}
			}
		})
	}
}

func TestRequestWithdrawn(t *testing.T) {
	t.Run("resolved", func(t *testing.T) {
		r := newRequestReplay(t, "question_plan", agent.AllowAll)
		var id string
		for {
			m, a, ok := r.next()
			if !ok {
				t.Fatal("missing resolution")
			}
			if q := requestEvent(t, a); q != nil {
				id = q.ID
			}
			if m.Method == "serverRequest/resolved" {
				if id == "" || len(a.Events) != 1 || a.Events[0].Kind != agent.RequestWithdrawn || a.Events[0].Request.ID != id {
					t.Fatal(a)
				}
				return
			}
		}
	})
	t.Run("turn_end", func(t *testing.T) {
		r := newRequestReplay(t, "approval_accept", agent.AskInChat)
		var id string
		r.skipResolved = true
		for r.pos < len(r.lines) {
			m, a, ok := r.next()
			if !ok {
				break
			}
			if q := requestEvent(t, a); q != nil && id == "" {
				id = q.ID
			}
			if m.Method == "turn/completed" {
				if id == "" || len(a.Events) < 2 || a.Events[0].Kind != agent.RequestWithdrawn || a.Events[0].Request.ID != id || a.Events[len(a.Events)-1].Kind != agent.TurnEnded {
					t.Fatal(a)
				}
				return
			}
		}
		t.Fatal("missing turn end")
	})
	t.Run("answered", func(t *testing.T) {
		r := newRequestReplay(t, "approval_accept", agent.AskInChat)
		count := 0
		for {
			_, a, ok := r.next()
			if !ok {
				break
			}
			for _, e := range a.Events {
				if e.Kind == agent.RequestWithdrawn {
					t.Fatal(e)
				}
				if e.Kind == agent.Request {
					count++
					if _, err := r.c.Answer(agent.Answer{Request: e.Request.ID, Allow: true}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if count != 2 {
			t.Fatal(count)
		}
	})
	t.Run("new_turn", func(t *testing.T) {
		r := newRequestReplay(t, "question_after_turn", agent.AllowAll)
		var id string
		starts := 0
		ended := false
		for {
			m, a, ok := r.next()
			if !ok {
				t.Fatal("missing second turn")
			}
			for _, e := range a.Events {
				if e.Kind == agent.Request {
					id = e.Request.ID
				}
				if e.Kind == agent.RequestWithdrawn && m.Method != "turn/started" {
					t.Fatal(e)
				}
			}
			if m.Method == "turn/completed" {
				ended = true
			}
			if m.Method == "turn/started" {
				starts++
				if starts == 2 {
					if !ended || id == "" || len(a.Events) == 0 || a.Events[0].Kind != agent.RequestWithdrawn || a.Events[0].Request.ID != id {
						t.Fatal(a)
					}
					return
				}
			}
		}
	})
}
func TestUnknownServerRequest(t *testing.T) {
	for _, id := range []any{float64(7), "seven"} { // Derive the unknown request from a captured approval.
		var m map[string]any
		for _, l := range replay(t, "approval_accept") {
			_ = json.Unmarshal(l.Msg, &m)
			if m["method"] == "item/commandExecution/requestApproval" {
				break
			}
		}
		m["method"] = "currentTime/read"
		m["id"] = id
		b, _ := json.Marshal(m)
		a := NewCodec(agent.Spec{}, Options{}).Decode(agent.Output, b)
		rawID, _ := json.Marshal(id)
		want := []byte(fmt.Sprintf("{\"id\":%s,\"error\":{\"code\":-32601,\"message\":\"hataori does not handle currentTime/read\"}}\n", rawID))
		checkWrite(t, a, want)
		if len(a.Events) != 1 || a.Events[0].Kind != agent.Unknown || a.Events[0].Text != string(b) {
			t.Fatal(a.Events)
		}
	}
}
func TestRebuiltCodecAnswersOpenRequest(t *testing.T) {
	for _, name := range []string{"approval_accept", "question_plan", "question_async"} {
		t.Run(name, func(t *testing.T) {
			lines := replay(t, name)
			spec, opts, _ := fixture(t, lines)
			spec.Approvals = agent.AskInChat
			c := NewCodec(spec, opts)
			var id string
			var rpcID json.RawMessage
			for _, l := range lines {
				if l.Dir != "recv" {
					continue
				}
				var m message
				_ = json.Unmarshal(l.Msg, &m)
				c.Decode(agent.Output, l.Msg)
				if m.Method == "item/commandExecution/requestApproval" || m.Method == "item/tool/requestUserInput" {
					var p struct {
						ItemID string `json:"itemId"`
					}
					_ = json.Unmarshal(m.Params, &p)
					id = p.ItemID
					rpcID = m.ID
					break
				}
				if m.Method == "item/completed" {
					var p params
					_ = json.Unmarshal(m.Params, &p)
					if len(p.Item.Questions) > 0 {
						id = p.Item.ID
						break
					}
				}
			}
			if id == "" {
				t.Fatal("missing request")
			}
			answer := agent.Answer{Request: id, Allow: true, Text: "blue"}
			if name == "question_plan" {
				answer.Text = planChoices[0]
			}
			a, err := c.Answer(answer)
			if err != nil {
				t.Fatal(err)
			}
			if name == "question_async" {
				checkMethod(t, a, captureSend(t, lines, "turn/steer", 0))
			} else {
				checkWrite(t, a, responseFor(t, lines, rpcID))
			}
		})
	}
}

func TestFileChangePathsForgotten(t *testing.T) {
	for _, skipCompletion := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip_completion_%t", skipCompletion), func(t *testing.T) {
			lines := replay(t, "approval_accept")
			c := NewCodec(agent.Spec{}, Options{}).(*codec)
			var id string
			sawStart := false
			for _, l := range lines {
				if l.Dir != "recv" {
					continue
				}
				var m message
				_ = json.Unmarshal(l.Msg, &m)
				var p params
				_ = json.Unmarshal(m.Params, &p)
				if m.Method == "item/started" && p.Item.Type == "fileChange" {
					id = p.Item.ID
					c.Decode(agent.Output, l.Msg)
					sawStart = true
					if len(c.paths[id]) != 1 {
						t.Fatal(c.paths)
					}
					continue
				}
				if sawStart && m.Method == "item/completed" && p.Item.ID == id {
					if skipCompletion {
						continue
					}
					c.Decode(agent.Output, l.Msg)
					if _, ok := c.paths[id]; ok {
						t.Fatal(c.paths)
					}
					return
				}
				if sawStart && m.Method == "turn/completed" {
					c.Decode(agent.Output, l.Msg)
					if _, ok := c.paths[id]; ok {
						t.Fatal(c.paths)
					}
					return
				}
				c.Decode(agent.Output, l.Msg)
			}
			t.Fatal("missing file change completion")
		})
	}
}
