package runner

import (
	"context"
	"github.com/yousan/hataori/internal/control"
	"testing"
	"time"
)

func TestAwayIdleBudgetDoesNotExpireDuringDisconnection(t *testing.T) {
	f, _ := remoteFixture(t, false)
	f.run.IdlePeriod = 2 * time.Second
	c := enableFixtureAway(t, f)
	seedRemote(t, f, 1, "reply idle before cut")
	waitFor(t, "idle before disconnection", 5*time.Second, f.has("turn_ended", 1))
	c.connected.Store(false)
	if a, err := f.run.MachineAvailability(context.Background(), "build"); err != nil || a.State == "ready" {
		t.Fatalf("cut not observed: %+v %v", a, err)
	}
	time.Sleep(2500 * time.Millisecond)
	var state control.State
	if err := f.st.DB.QueryRow(`SELECT state FROM sessions WHERE id=1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != control.Idle {
		t.Fatalf("idle expired during absence: %s", state)
	}
	c.generation.Add(1)
	c.connected.Store(true)
	waitFor(t, "idle catch-up finished", 8*time.Second, func() bool {
		a, e := f.run.MachineAvailability(context.Background(), "build")
		return e == nil && a.State == "ready"
	})
	time.Sleep(500 * time.Millisecond)
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	f.run.checkIdle(lv)
	if err := f.st.DB.QueryRow(`SELECT state FROM sessions WHERE id=1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != control.Idle {
		t.Fatalf("remaining idle budget lost after recovery: %s", state)
	}
	waitFor(t, "idle expires after remaining active time", 5*time.Second, func() bool {
		var s control.State
		return f.st.DB.QueryRow(`SELECT state FROM sessions WHERE id=1`).Scan(&s) == nil && s == control.Asleep
	})
}
