package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func TestLimitsSelectDirectoryByReceiptNotOldestObservation(t *testing.T) {
	f := newFixture(t, fakeOwner{})
	_, a := f.running(t, "a")
	_, b := f.running(t, "b")
	for id, dir := range map[int64]string{a: "a", b: "b"} {
		data, _ := json.Marshal(map[string]any{"spec": agent.Spec{ConfigDir: dir}})
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET spec=? WHERE id=?`, string(data), id); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Unix(10000, 0)
	push := func(sid int64, seq uint64, minute, used int) {
		e := limitsEvent(at.Add(time.Duration(minute)*time.Minute), used)
		e.Limits.Cached = true
		f.agentEvent(t, sid, seq, e)
	}
	push(a, 2, 0, 10)
	push(b, 2, 1, 20)
	push(a, 3, 2, 10) // A last reported, but its observation is still oldest
	ls, err := f.owner.Limits(context.Background())
	if err != nil || len(ls) != 1 || len(ls[0].Windows) != 1 || ls[0].Windows[0].Used != 10 || !ls[0].At.Equal(at) {
		t.Fatalf("directory selection used observation age: %+v %v", ls, err)
	}
}
