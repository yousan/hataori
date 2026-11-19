package codex

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// No capture shows a refused turn/steer (#317 did not capture one).
func TestSteerCapture(t *testing.T)     { replaySteerCapture(t, "steer") }
func TestInterruptCapture(t *testing.T) { replaySteerCapture(t, "interrupt") }

func replaySteerCapture(t *testing.T, scenario string) {
	{
		c := NewCodec(agent.Spec{Effort: "low"}, Options{})
		c.Open()
		ids := map[string]json.RawMessage{}
		var events []agent.Event
		var clean []byte
		var steerID json.RawMessage
		var takenAtResponse bool
		for _, l := range replay(t, scenario) {
			var m message
			if err := json.Unmarshal(l.Msg, &m); err != nil {
				t.Fatal(err)
			}
			if l.Dir == "send" {
				switch m.Method {
				case "turn/start", "turn/steer":
					var p struct {
						Input []struct {
							Text string `json:"text"`
						} `json:"input"`
					}
					json.Unmarshal(m.Params, &p)
					id := "start"
					if m.Method == "turn/steer" {
						id = "steered"
						steerID = m.ID
					}
					a, err := c.Input(agent.Input{ID: id, Text: p.Input[0].Text})
					if err != nil {
						t.Fatal(err)
					}
					if len(a.Write) == 1 {
						var sent message
						json.Unmarshal(a.Write[0], &sent)
						ids[string(m.ID)] = sent.ID
					}
					if m.Method == "turn/steer" {
						equalCapture(t, a.Write, l.Msg)
						now := time.Unix(100, 0)
						c.Tick(now)
						if expired := c.Tick(now.Add(time.Hour)); len(expired.Events) != 0 {
							t.Fatalf("pending steer expired: %+v", expired)
						}
					}
				case "turn/interrupt":
					a := c.Interrupt()
					equalCapture(t, a.Write, l.Msg)
					var sent message
					json.Unmarshal(a.Write[0], &sent)
					ids[string(m.ID)] = sent.ID
				case "thread/backgroundTerminals/clean":
					equalCapture(t, [][]byte{clean}, l.Msg)
					var sent message
					json.Unmarshal(clean, &sent)
					ids[string(m.ID)] = sent.ID
				}
				continue
			}
			line := l.Msg
			if m.Method == "" {
				if id, ok := ids[string(m.ID)]; ok {
					var raw map[string]json.RawMessage
					json.Unmarshal(line, &raw)
					raw["id"] = id
					line, _ = json.Marshal(raw)
				}
			}
			a := c.Decode(agent.Output, line)
			if m.Method == "turn/completed" {
				var p params
				json.Unmarshal(m.Params, &p)
				if p.Turn.Status == "interrupted" {
					if len(a.Write) != 1 {
						t.Fatalf("clean writes: %q", a.Write)
					}
					clean = a.Write[0]
				}
			}
			if m.Method == "item/completed" && clean != nil {
				var p params
				json.Unmarshal(m.Params, &p)
				if p.Item.Type == "commandExecution" && len(a.Events) > 0 {
					t.Fatalf("late completion: %+v", a.Events)
				}
			}
			if m.Method == "" && string(m.ID) == string(steerID) && len(steerID) > 0 {
				for _, e := range a.Events {
					if e.Kind == agent.InputTaken && e.Input == "steered" {
						takenAtResponse = true
					}
				}
			}
			events = append(events, a.Events...)
		}
		starts, ends, taken := 0, 0, 0
		var final string
		var interrupted bool
		for _, e := range events {
			switch e.Kind {
			case agent.TurnStarted:
				starts++
			case agent.TurnEnded:
				ends++
				interrupted = interrupted || e.Status == agent.Interrupted
			case agent.InputTaken:
				if e.Input == "steered" {
					taken++
				}
			case agent.Text:
				if e.Final {
					final = e.Text
				}
			}
		}
		if scenario == "steer" {
			if starts != 1 || ends != 1 || taken != 1 || !takenAtResponse || final != "done\n\nsteered" {
				t.Fatalf("starts %d ends %d taken %d response %v final %q", starts, ends, taken, takenAtResponse, final)
			}
		} else if starts != 2 || ends != 2 || !interrupted || final != "ok" {
			t.Fatalf("starts %d ends %d interrupted %v final %q", starts, ends, interrupted, final)
		}
	}
}
func equalCapture(t *testing.T, writes [][]byte, want []byte) {
	t.Helper()
	if len(writes) != 1 {
		t.Fatalf("writes %q", writes)
	}
	// Request IDs are assigned by the codec and differ from the capture.
	var a, b map[string]any
	if json.Unmarshal(writes[0], &a) != nil || json.Unmarshal(want, &b) != nil {
		t.Fatalf("invalid JSON: %s or %s", writes[0], want)
	}
	delete(a, "id")
	delete(b, "id")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s want %s", writes[0], want)
	}
}

func TestInterruptCaptureQueuedInput(t *testing.T) {
	c := NewCodec(agent.Spec{Effort: "low"}, Options{})
	c.Open()
	var ended bool
	for _, l := range replay(t, "interrupt") {
		var m message
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			t.Fatal(err)
		}
		if l.Dir == "send" {
			if m.Method == "turn/start" && !ended {
				var p struct {
					Input []struct {
						Text string `json:"text"`
					} `json:"input"`
				}
				if err := json.Unmarshal(m.Params, &p); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Input(agent.Input{ID: "first", Text: p.Input[0].Text}); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		if m.Method == "turn/completed" {
			// A refused steer holds later inputs until the running turn ends.
			c.(*codec).steerBlocked = true
			a, err := c.Input(agent.Input{ID: "queued", Text: "queued"})
			if err != nil || len(a.Write) != 0 {
				t.Fatalf("queue: %+v %v", a, err)
			}
			a = c.Decode(agent.Output, l.Msg)
			if len(a.Write) != 2 || len(a.Inputs) != 1 || a.Inputs[0] != "queued" {
				t.Fatalf("completion: %+v", a)
			}
			var first, second message
			json.Unmarshal(a.Write[0], &first)
			json.Unmarshal(a.Write[1], &second)
			if first.Method != "thread/backgroundTerminals/clean" || second.Method != "turn/start" {
				t.Fatalf("writes: %s %s", a.Write[0], a.Write[1])
			}
			ended = true
			continue
		}
		a := c.Decode(agent.Output, l.Msg)
		if ended && m.Method == "item/completed" {
			var p params
			json.Unmarshal(m.Params, &p)
			if p.Item.Type == "commandExecution" {
				if len(a.Events) != 0 {
					t.Fatalf("late completion: %+v", a.Events)
				}
				return
			}
		}
	}
	t.Fatal("missing late command completion")
}
