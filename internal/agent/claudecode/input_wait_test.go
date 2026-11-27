package claudecode

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
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
	if len(notices) != 1 || notices[0].Kind != agent.InputWaiting || notices[0].Input != "waiting" || string(notices[0].Reason) != "Enter could not be handed to the agent's window" {
		t.Fatalf("wait notices %+v; want one explaining the refused Enter", notices)
	}
	r, err := session.Step(session.Snapshot{State: control.Running, Turn: &session.OpenTurn{By: session.ByCaller}}, session.Input{Kind: session.AgentEvent, Agent: notices[0]})
	if err != nil || r.End != nil || r.State != control.Running || len(r.Out) != 1 || r.Out[0].Kind != "input_waiting" || r.Out[0].Data["input"] != "waiting" || r.Out[0].Data["reason"] != string(notices[0].Reason) {
		t.Fatalf("waiting event through session: %+v, %v", r, err)
	}
	if a := c.Interrupt(); !a.NotInterrupted {
		t.Fatalf("owed Enter interrupt: %+v", a)
	}
	w.refuse = false
	w.tick(c, now, 10*time.Second)
	w.want(t, firstText, enterKey)
}
