package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// stopHooks is the core's Hooks for a test: before_agent_run stops each
// input whose text holds word.
type stopHooks struct {
	word string
	mu   sync.Mutex
	seen map[string]int // texts at before_agent_run, by how often
}

func (h *stopHooks) Run(_ context.Context, c control.HookCall) control.HookResult {
	if c.Point != core.PointBeforeAgentRun {
		return control.HookResult{Text: c.Text}
	}
	h.mu.Lock()
	h.seen[c.Text]++
	h.mu.Unlock()
	if strings.Contains(c.Text, h.word) {
		return control.HookResult{Text: c.Text, Stopped: true, By: "scan", Reason: "MARKER-reason"}
	}
	return control.HookResult{Text: c.Text}
}

// times is how often a text holding s came to before_agent_run.
func (h *stopHooks) times(s string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for text, k := range h.seen {
		if strings.Contains(text, s) {
			n += k
		}
	}
	return n
}

// A wake a plugin stops is dropped, not tried again: the plugin sees it
// once, an hour later too, and the next turn end wakes the supervisor
// with a digest that names only its own thread.
func TestWakeStoppedByAPluginIsDropped(t *testing.T) {
	wt := newWakeTest(t)
	h := &stopHooks{word: "> STOPME", seen: map[string]int{}} // in the digest only
	clock := coreHook
	coreHook = func(c *core.Core) { clock(c); c.Hooks = h }
	stop, owner, sv := wt.start(t)
	defer stop()
	a, pos := wt.worker(t, owner, "c1", "a", "reply STOPME")
	wt.read(t, pos)
	wt.quiet(t)
	eventually(t, "the wake to reach the plugin", 10*time.Second, func() bool { return h.times("hataori: ") > 0 })
	wt.hour()
	time.Sleep(200 * time.Millisecond)
	if n := h.times("> STOPME"); n != 1 {
		t.Fatalf("the stopped wake came to the plugin %d times, want once", n)
	}
	if got := wt.wakes(t, sv.ID); len(got) != 0 {
		t.Fatalf("the stopped wake reached the supervisor: %q", got)
	}
	b, pos := wt.worker(t, owner, "c1", "b", "reply fine")
	wt.read(t, pos)
	wt.quiet(t)
	got := wt.woken(t, owner, sv.ID, 1)
	if !strings.Contains(got[0], fmt.Sprintf("Thread %d:", b)) || strings.Contains(got[0], fmt.Sprintf("Thread %d:", a)) {
		t.Fatalf("the next wake:\n%s", got[0])
	}
}
