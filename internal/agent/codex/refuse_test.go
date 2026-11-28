package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func TestOversizedSteerCanBeExplicitlyRefused(t *testing.T) {
	c := NewCodec(agent.Spec{}, Options{}).(*codec)
	c.Open()
	refused := false
	for _, line := range replay(t, "steer") {
		var m message
		if err := json.Unmarshal(line.Msg, &m); err != nil {
			t.Fatal(err)
		}
		if line.Dir == "send" {
			if m.Method != "turn/steer" {
				continue
			}
			a, err := c.Input(agent.Input{ID: "oversized", Text: strings.Repeat("x", 70000)})
			if err != nil || len(a.Write) != 1 || len(c.sent) != 1 || !c.sent[0].steer {
				t.Fatalf("no running steer: %+v %v", a, err)
			}
			turn := c.turnID
			a = c.RefuseInput([]string{"oversized"}, agent.NotDelivered)
			if len(a.Events) != 1 || a.Events[0].Input != "oversized" || a.Events[0].Reason != agent.NotDelivered {
				t.Fatalf("missing visible refusal: %+v", a.Events)
			}
			if len(c.sent) != 0 || !c.running || c.turnID != turn {
				t.Fatal("refusal retained steer or ended the original turn")
			}
			if a := c.Tick(time.Now().Add(time.Hour)); len(a.Events) != 0 {
				t.Fatalf("refusal repeated: %+v", a.Events)
			}
			refused = true
			continue
		}
		// Captured real output establishes readiness and the original turn;
		// the rejected steer has no response to feed into this codec.
		if refused && len(m.ID) > 0 {
			continue
		}
		c.Decode(agent.Output, line.Msg)
	}
	if !refused {
		t.Fatal("capture had no steering input")
	}
	a, err := c.Input(agent.Input{ID: "next", Text: "continue"})
	if err != nil || len(a.Write) != 1 {
		t.Fatalf("later input remains blocked: %+v %v", a, err)
	}
}
