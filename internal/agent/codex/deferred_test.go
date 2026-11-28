package codex

import (
	"encoding/json"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// These are actual captured protocol lines. Reordering a response relative
// to notifications models reconnect boundaries; no service output is invented.
func deferredCapture(t *testing.T) (before []wireLine, command, response, started, user, ended []byte) {
	t.Helper()
	for _, l := range replay(t, "start") {
		var m message
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			t.Fatal(err)
		}
		if l.Dir == "send" && m.Method == "turn/start" {
			command = l.Msg
			continue
		}
		if command == nil {
			before = append(before, l)
			continue
		}
		if l.Dir != "recv" {
			continue
		}
		if string(m.ID) == "3" && len(m.Result) != 0 {
			response = l.Msg
		}
		switch m.Method {
		case "turn/started":
			started = l.Msg
		case "turn/completed":
			ended = l.Msg
		case "item/started":
			var p params
			json.Unmarshal(m.Params, &p)
			if p.Item.Type == "userMessage" {
				user = l.Msg
			}
		}
	}
	if command == nil || response == nil || started == nil || user == nil || ended == nil {
		t.Fatal("incomplete actual capture")
	}
	return
}

func deferredTaken(events []agent.Event) int {
	n := 0
	for _, e := range events {
		if e.Input == "deferred-input" && (e.Kind == agent.InputTaken || e.Kind == agent.TurnStarted) {
			n++
		}
	}
	return n
}

func TestDeferredCodexExactCapturedAcknowledgementAcrossReconstruction(t *testing.T) {
	before, command, response, started, user, _ := deferredCapture(t)
	var sent struct {
		Params struct {
			Input []struct {
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	json.Unmarshal(command, &sent)
	for _, warmResponse := range []bool{false, true} {
		for range 2 {
			c := NewCodec(agent.Spec{}, Options{})
			feed(t, c, before, 0)
			if warmResponse {
				c.Decode(agent.Output, response)
			}
			a, err := c.Input(agent.Input{ID: "deferred-input", Text: sent.Params.Input[0].Text, IdleOnly: true, Written: true, Command: [][]byte{command}})
			if err != nil || len(a.Write) != 0 {
				t.Fatalf("restore: %+v %v", a, err)
			}
			n := deferredTaken(a.Events)
			for _, line := range [][]byte{started, user, user} {
				a = c.Decode(agent.Output, line)
				if deferredTaken(a.Events) != 0 {
					t.Fatal("notification/equal text falsely confirmed deferred input")
				}
			}
			a = c.Decode(agent.Output, response)
			n += deferredTaken(a.Events)
			if n != 1 {
				t.Fatalf("exact response acknowledged %d inputs", n)
			}
			if a = c.Decode(agent.Output, response); deferredTaken(a.Events) != 0 {
				t.Fatal("duplicate response acknowledged twice")
			}
		}
	}
}

func TestDeferredCodexHoldsAcrossLateNativeTurnWithoutSteering(t *testing.T) {
	before, _, _, started, user, ended := deferredCapture(t)
	c := NewCodec(agent.Spec{}, Options{})
	feed(t, c, before, 0)
	c.Decode(agent.Output, started)
	c.Decode(agent.Output, user) // native turn is now reported, so ordinary input can steer
	a, err := c.Input(agent.Input{ID: "deferred-input", Text: "a later instruction", IdleOnly: true})
	if err != nil || len(a.Write) != 0 {
		t.Fatalf("deferred input entered native turn: %+v %v", a, err)
	}
	a = c.Decode(agent.Output, ended)
	if len(a.Write) != 1 {
		t.Fatalf("eligible boundary emitted %d submissions", len(a.Write))
	}
	if err := (&Adapter{}).ValidateDeferredInput(a); err != nil {
		t.Fatal(err)
	}
	var m message
	json.Unmarshal(a.Write[0], &m)
	if m.Method != "turn/start" {
		t.Fatalf("deferred method %q", m.Method)
	}
}
