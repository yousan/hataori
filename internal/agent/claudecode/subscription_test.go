package claudecode

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// These are extracted real records, with turn identifiers removed. The
// correlation below belongs to the harness, not to the captured payload.
func TestSubscriptionRejectionFromCapturedRecords(t *testing.T) {
	const dir = "testdata/2.1.332/subscription_unavailable/"
	transcript, err := os.ReadFile(dir + "assistant.json")
	if err != nil {
		t.Fatal(err)
	}
	hook, err := os.ReadFile(dir + "StopFailure.json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(hook, &p); err != nil {
		t.Fatal(err)
	}
	p["prompt_id"] = "test-prompt"
	hook, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, transcriptFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "transcript first", false: "hook first"}[transcriptFirst], func(t *testing.T) {
			c := newTestCodec()
			c.current = "test-prompt"
			c.turn(c.current).known = true
			var all Actions
			if transcriptFirst {
				all = merge(all, c.Decode(agent.File, transcript))
			}
			all = merge(all, c.Decode(agent.Hook, envelope("failure", "", hook)))
			if !transcriptFirst {
				all = merge(all, c.Decode(agent.File, transcript))
			}
			if len(all.Events) != 1 {
				t.Fatalf("wanted one failure without duplicate transcript text, got %+v", all.Events)
			}
			e := all.Events[0]
			if e.Kind != agent.TurnEnded || e.Status != agent.Failed || string(e.Reason) != "unavailable" || e.Text != p["last_assistant_message"] {
				t.Fatalf("subscription failure lost classification or provider message: %+v", e)
			}
		})
	}
}
