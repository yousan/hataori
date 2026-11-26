package claudecode

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func TestAnOwedEnterReportsItsWaitOnce(t *testing.T) {
	c, w := started(t), &handing{}
	w.input(t, c, "waiting", firstText)
	w.refuse = true
	now := time.Now()
	var notices []agent.Event
	for end := now.Add(3 * DefaultInputTimeout); now.Before(end); now = now.Add(200 * time.Millisecond) {
		for _, e := range w.act(c.Tick(now)).Events {
			if e.Kind == agent.TurnEnded {
				t.Fatalf("waiting input ended: %+v", e)
			}
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 || notices[0].Input != "waiting" || string(notices[0].Reason) != "Enter could not be handed to the agent's window" {
		t.Fatalf("wait notices %+v; want one explaining the refused Enter", notices)
	}
	if a := c.Interrupt(); !a.NotInterrupted {
		t.Fatalf("owed Enter interrupt: %+v", a)
	}
	w.refuse = false
	w.tick(c, now, 10*time.Second)
	w.want(t, firstText, enterKey)
}
