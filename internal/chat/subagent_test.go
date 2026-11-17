package chat

import (
	"testing"
)

func TestSubagentStartAndEndLines(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 1, "subagent_started", map[string]any{"id": "s1", "name": "explorer"})
	f.at(t, id, 2, "tool_started", map[string]any{"id": "t1", "tool": "Read", "parent": "s1"})
	f.at(t, id, 3, "tool_started", map[string]any{"id": "t2", "tool": "Bash"}) // the agent's own
	f.at(t, id, 4, "tool_started", map[string]any{"id": "t3", "tool": "Grep", "parent": "s1"})
	f.at(t, id, 66, "subagent_ended", map[string]any{"id": "s1"})
	f.at(t, id, 70, "text", map[string]any{"text": "Done.", "final": true})
	calls := f.postedUntil(t, id, "Done.")
	want := "the prompt|Subagent explorer started.|Subagent explorer ended after 1m 05s, 2 tool calls.|Done."
	if got := texts(calls); got != want {
		t.Fatalf("posts\n%s\nwant\n%s", got, want)
	}
	for _, c := range calls[1:3] {
		if c.Message.ReplyTo != nil || !c.Message.Quiet {
			t.Fatalf("subagent line %+v, want quiet and no reply", c.Message)
		}
	}
}

func TestSubagentRunningAcrossARestartEndsOnce(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	stop := f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 10, "subagent_started", map[string]any{"id": "s1", "name": "explorer"})
	f.at(t, id, 11, "tool_started", map[string]any{"id": "t1", "tool": "Read", "parent": "s1"})
	f.postedUntil(t, id, "Subagent explorer started.")
	f.advance(t, c, 12)
	stop()
	f.at(t, id, 80, "subagent_ended", map[string]any{"id": "s1"})
	stop = f.showing(t)
	f.postedUntil(t, id, "Subagent explorer ended after 1m 10s, 1 tool call.")
	stop()
	// Read everything again: nothing is posted twice.
	f.fe.Outbox.Store.DB.Exec(`DELETE FROM chat_cursors`)
	f.showing(t)
	f.at(t, id, 90, "text", map[string]any{"text": "marker", "final": false})
	got := texts(f.postedUntil(t, id, "marker"))
	if got != "the prompt|Subagent explorer started.|Subagent explorer ended after 1m 10s, 1 tool call.|marker" {
		t.Fatalf("posts %s", got)
	}
}

func TestSubagentWithoutAnEndIsClosedWithItsTurn(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 5, "subagent_started", map[string]any{"id": "s1", "name": "a"})
	f.at(t, id, 6, "subagent_started", map[string]any{"id": "s2"})
	f.at(t, id, 50, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 0})
	f.at(t, id, 60, "text", map[string]any{"text": "marker", "final": false})
	got := texts(f.postedUntil(t, id, "marker"))
	want := "the prompt|Subagent a started.|Subagent s2 started.|" +
		"Subagent a ended with its turn after 0m 45s, 0 tool calls.|Subagent s2 ended with its turn after 0m 44s, 0 tool calls.|marker"
	if got != want {
		t.Fatalf("posts\n%s\nwant\n%s", got, want)
	}
}
