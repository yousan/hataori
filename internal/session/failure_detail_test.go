package session

import (
	"encoding/json"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

func TestFailedTurnPreservesProviderDetail(t *testing.T) {
	for _, st := range []agent.EndStatus{agent.Completed, agent.Interrupted, agent.Failed} {
		r, err := Step(Snapshot{State: control.Running, Turn: &OpenTurn{By: ByCaller}}, Input{Kind: AgentEvent,
			Agent: agent.Event{Kind: agent.TurnEnded, Status: st, Reason: agent.APIError, Text: "provider explanation"}})
		if err != nil {
			t.Fatal(err)
		}
		d := r.Out[0].Data
		if st == agent.Failed {
			if d["detail"] != "provider explanation" {
				t.Fatalf("lost provider detail: %v", d)
			}
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			cut := control.WithoutText(control.Event{Kind: KindTurnEnded, Data: raw})
			var hidden map[string]any
			if err := json.Unmarshal(cut.Data, &hidden); err != nil {
				t.Fatal(err)
			}
			if _, ok := hidden["detail"]; ok {
				t.Fatal("provider detail is visible to a caller without text access")
			}
			if hidden["reason"] != "api_error" {
				t.Fatal("redaction removed the failure classification")
			}
		} else if _, ok := d["detail"]; ok {
			t.Fatalf("nonfailed turn got provider detail: %v", d)
		}
	}
}
