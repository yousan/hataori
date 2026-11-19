package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func TestCapturedUsageAndLimits(t *testing.T) {
	lines := replay(t, "usage")
	c := NewCodec(agent.Spec{}, Options{})
	opening := c.Open()
	if parse(t, opening.Write[len(opening.Write)-1]).Method != RateLimitsRead {
		t.Fatal(opening.Write)
	}
	var response map[string]any
	for _, l := range lines {
		if l.Dir == "recv" {
			var m map[string]any
			json.Unmarshal(l.Msg, &m)
			if r, ok := m["result"].(map[string]any); ok && r["rateLimits"] != nil {
				response = m
				break
			}
		}
	}
	if response == nil {
		t.Fatal("missing response")
	}
	reads := []int{parse(t, opening.Write[len(opening.Write)-1]).ID}
	var usage []agent.Event
	var limits []agent.Event
	updates := 0
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		var m map[string]any
		json.Unmarshal(l.Msg, &m)
		if r, ok := m["result"].(map[string]any); ok && r["rateLimits"] != nil {
			continue
		}
		a := c.Decode(agent.Output, l.Msg)
		for _, e := range a.Events {
			if e.Kind == agent.Usage {
				usage = append(usage, e)
			}
			if e.Kind == agent.Limits {
				limits = append(limits, e)
			}
		}
		if m["method"] == RateLimitsUpdated {
			updates++
			if len(a.Write) != 1 {
				t.Fatal(a)
			}
			reads = append(reads, parse(t, a.Write[0]).ID)
		}
	}
	for _, id := range reads {
		response["id"] = id
		b, _ := json.Marshal(response)
		a := c.Decode(agent.Output, b)
		for _, e := range a.Events {
			if e.Kind == agent.Limits {
				limits = append(limits, e)
			}
		}
	}
	if len(reads) != updates+1 || len(limits) != len(reads) || len(usage) != 2 {
		t.Fatalf("reads %d updates %d limits %d usage %+v", len(reads), updates, len(limits), usage)
	}
	if usage[0].Usage.Input != 3394 || usage[1].Usage.Input != 6869 || usage[1].Usage.ContextUsed != 15000 || usage[1].Usage.ContextSize != 258400 {
		t.Fatal(usage)
	}
	for _, e := range limits {
		if e.Limits.Every != 5*time.Minute || e.Limits.At.IsZero() || len(e.Limits.Windows) != 2 || e.Limits.Windows[0].Used != 30 || e.Limits.Windows[1].Used != 13 {
			t.Fatal(e)
		}
	}
}

func TestLimitsWaitForCapturedTimestamp(t *testing.T) {
	lines := replay(t, "usage")
	c := NewCodec(agent.Spec{}, Options{})
	c.Open()
	var response, notification []byte
	for _, l := range lines {
		if l.Dir != "recv" {
			continue
		}
		var m map[string]any
		json.Unmarshal(l.Msg, &m)
		if r, ok := m["result"].(map[string]any); ok && r["rateLimits"] != nil {
			response = l.Msg
		}
		if notification == nil && m["emittedAtMs"] != nil {
			notification = l.Msg
		}
	}
	if response == nil || notification == nil {
		t.Fatal("capture missing response or timestamp")
	}
	if a := c.Decode(agent.Output, response); len(a.Events) != 0 {
		t.Fatal(a)
	}
	a := c.Decode(agent.Output, notification)
	found := false
	for _, e := range a.Events {
		if e.Kind == agent.Limits && !e.Limits.At.IsZero() {
			found = true
		}
	}
	if !found {
		t.Fatal(a)
	}
}
