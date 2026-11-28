package fake

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// State fixture only: no simulated service output, process or terminal.
func TestDeferredFakeSeparatesLaterHeldInput(t *testing.T) {
	c := &codec{running: true}
	c.Input(agent.Input{ID: "deferred:1", Text: "management", IdleOnly: true})
	c.Input(agent.Input{ID: "2", Text: "ordinary"})
	a := c.ended()
	if e := (&Adapter{}).ValidateDeferredInput(a); e != nil {
		t.Fatal("deferred submission mixed with later input", e)
	}
	if len(a.Inputs) != 1 || a.Inputs[0] != "deferred:1" {
		t.Fatal(a.Inputs)
	}
	a = c.Tick(time.Now())
	if len(a.Inputs) != 1 || a.Inputs[0] != "2" || len(a.Write) != 1 {
		t.Fatal("later ordinary input stranded", a.Inputs)
	}
}

func TestDeferredFakeStartsDeadlineOnlyAfterReceipt(t *testing.T) {
	c := &codec{timeout: time.Second}
	c.Input(agent.Input{ID: "deferred:1", Text: "management", IdleOnly: true, Written: true})
	if a := c.Tick(time.Now().Add(time.Hour)); len(a.Events) != 0 {
		t.Fatal("unstarted deferred write expired", a.Events)
	}
	if a := c.Tick(time.Now().Add(2 * time.Hour)); len(a.Events) != 0 {
		t.Fatal("unstarted deferred write acquired a deadline on a prior tick", a.Events)
	}
	c.Input(agent.Input{ID: "deferred:1", Text: "management", IdleOnly: true, Written: true, WrittenAt: time.Now().Add(-time.Minute)})
	if a := c.Tick(time.Now()); len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered {
		t.Fatal("actual completed write did not retain its deadline", a.Events)
	}
}
