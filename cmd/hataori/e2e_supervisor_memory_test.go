package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
)

// The fake records the exact input handed to it. File-change context must
// precede the person's message without entering the public input event.
func TestConversationSharedMemoryContext(t *testing.T) {
	record := t.TempDir()
	f := newSvTalks(t, "", recordIn(record))
	a, b := f.talk(t, "memory-a"), f.talk(t, "memory-b")
	writes(t, f.owner, a, "memory-write", toolScript(t, supervisor.ToolWrite, map[string]any{"path": "notes/plan.md", "text": "Ship the fix.\n"}))
	send := func(id control.ThreadID, key string, changed bool) {
		t.Helper()
		text := "reply " + key
		sendAndRead(t, f.owner, id, key, text)
		var got string
		for _, start := range starts(t, record) {
			for _, in := range start.Inputs {
				if strings.HasSuffix(in, text) {
					got = in
				}
			}
		}
		if got == "" || strings.Contains(got, "notes/plan.md") != changed || !strings.HasSuffix(got, text) {
			t.Fatalf("changed=%v, agent got %q", changed, got)
		}
		for _, in := range inputsOf(t, f.owner, id) {
			if in["text"] == text && strings.Contains(fmt.Sprint(in), "notes/plan.md") {
				t.Fatalf("context entered input event: %v", in)
			}
		}
	}
	send(b, "memory-b-first", true)
	send(b, "memory-b-second", false)
	send(a, "memory-a-own", false)
	wake := func(n int, key string, changed bool) {
		t.Helper()
		_, pos := f.wt.worker(t, f.owner, "c1", key, "reply worker")
		f.wt.read(t, pos)
		f.wt.quiet(t)
		got := f.wt.woken(t, f.owner, f.sv.ID, n)
		if strings.Contains(got[n-1], "notes/plan.md") != changed {
			t.Fatalf("own wake changed=%v: %q", changed, got[n-1])
		}
		f.wt.read(t, f.wt.ended(t, f.sv.ID, n+1))
	}
	wake(1, "memory-worker-first", true)
	wake(2, "memory-worker-next", false)
	f.restart(t)
	send(b, "memory-after-restart", false)
	writes(t, f.owner, a, "memory-edit", toolScript(t, supervisor.ToolEdit, map[string]any{"path": "notes/plan.md", "old": "Ship", "new": "Review"}))
	send(b, "memory-after-edit", true)
}
