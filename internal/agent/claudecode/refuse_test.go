package claudecode

import (
	"github.com/yousan/hataori/internal/agent"
	"testing"
)

func TestConfirmedUnsentInputDoesNotHoldTheEditor(t *testing.T) {
	c := started(t)
	if a, err := c.Input(agent.Input{ID: "rejected", Text: "unsupported action"}); err != nil || len(a.Keys) == 0 {
		t.Fatalf("no attempted paste: %+v %v", a, err)
	}
	a := c.RefuseInput([]string{"rejected"}, agent.NotDelivered)
	if len(a.Events) != 1 || a.Events[0].Input != "rejected" || a.Events[0].Reason != agent.NotDelivered || c.sent != nil || c.holdsBox() {
		t.Fatalf("rejection retained editor state: %+v", a.Events)
	}
	if a, err := c.Input(agent.Input{ID: "next", Text: "next"}); err != nil || len(a.Keys) == 0 {
		t.Fatalf("subsequent input blocked: %+v %v", a, err)
	}
}
