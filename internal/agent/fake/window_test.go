package fake

import (
	"encoding/json"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// A session taken in is past its start: Open reports it ready and watches
// no screen, even with a start-up dialog configured, and the transcript a
// later report names is followed.
func TestTakenInCodec(t *testing.T) {
	a, err := New([]byte(`{"command":["fake"],"terminal":true,"dialog":"Trust?"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := a.NewCodec(agent.Spec{ResumeID: "s1", Model: "m1", TakenIn: true})
	open := c.Open()
	if open.WatchScreen || len(open.Events) != 1 || open.Events[0].Kind != agent.SessionReady || open.Events[0].SessionID != "s1" {
		t.Fatalf("Open: %+v", open)
	}
	payload, _ := json.Marshal(map[string]any{"t": "turn", "by": "terminal", "n": 1, "session": "s1", "transcript": "/tmp/t.jsonl"})
	line, _ := json.Marshal(map[string]any{"hook": "h1", "wait": false, "at": 1, "payload": json.RawMessage(payload)})
	got := c.Decode(agent.Hook, line)
	if len(got.Follow) != 1 || got.Follow[0] != "/tmp/t.jsonl" || len(got.Events) != 1 || got.Events[0].Kind != agent.TurnStarted {
		t.Fatalf("a report naming the transcript: %+v", got)
	}
}
