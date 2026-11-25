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
	f.at(t, id, 55, "subagent_ended", map[string]any{"id": "s1"}) // after its turn: no second line
	f.at(t, id, 60, "text", map[string]any{"text": "marker", "final": false})
	got := texts(f.postedUntil(t, id, "marker"))
	want := "the prompt|Subagent a started.|Subagent s2 started.|" +
		"Subagent a ended with its turn after 0m 45s, 0 tool calls.|Subagent s2 ended with its turn after 0m 44s, 0 tool calls.|marker"
	if got != want {
		t.Fatalf("posts\n%s\nwant\n%s", got, want)
	}
}

// A subagent in the background, which its turn's end lists as still
// running, gets its one end line when it ends, in whichever turn, with the
// whole time and every tool call; also across a restart of the front end
// (#802).
func TestSubagentInTheBackgroundEndsOnceWhenItEnds(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	stop := f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 10, "subagent_started", map[string]any{"id": "s1", "name": "a"})
	f.at(t, id, 11, "subagent_started", map[string]any{"id": "s2", "name": "b"})
	f.at(t, id, 12, "tool_started", map[string]any{"id": "t1", "tool": "Read", "parent": "s1"})
	f.at(t, id, 20, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 2, "subagents": []string{"s1"}})
	f.at(t, id, 30, "tool_started", map[string]any{"id": "t2", "tool": "Read", "parent": "s1"}) // between the turns
	f.at(t, id, 40, "turn_started", map[string]any{"turn": 2, "by": "caller"})
	f.at(t, id, 50, "turn_ended", map[string]any{"turn": 2, "by": "caller", "outcome": "failure", "reason": "api_error", "background": 1, "subagents": []string{"s1"}})
	f.at(t, id, 51, "text", map[string]any{"text": "first marker", "final": false})
	f.postedUntil(t, id, "first marker")
	stop()
	f.showing(t)
	f.at(t, id, 60, "turn_started", map[string]any{"turn": 3, "by": "caller"})
	f.at(t, id, 61, "tool_started", map[string]any{"id": "t3", "tool": "Read", "parent": "s1"})
	f.at(t, id, 75, "subagent_ended", map[string]any{"id": "s1"})
	f.at(t, id, 80, "turn_ended", map[string]any{"turn": 3, "by": "caller", "outcome": "completion", "background": 0})
	f.at(t, id, 90, "text", map[string]any{"text": "marker", "final": false})
	got := texts(f.postedUntil(t, id, "marker"))
	want := "the prompt|Subagent a started.|Subagent b started.|Subagent b ended with its turn after 0m 09s, 0 tool calls.|The turn failed: error (api_error).|first marker|" +
		"Subagent a ended after 1m 05s, 3 tool calls.|marker"
	if got != want {
		t.Fatalf("posts\n%s\nwant\n%s", got, want)
	}
}

// A subagent in the background whose end is never reported is closed once:
// when a turn ends that no longer lists it, or when its session's agent is
// stopped.
func TestSubagentInTheBackgroundWithoutAnEndIsClosedOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		kind string
		data map[string]any
		want string
	}{
		"a later turn": {"turn_ended", map[string]any{"turn": 2, "by": "agent", "outcome": "completion", "background": 1},
			"Subagent a ended with its turn after 1m 30s, 0 tool calls."},
		"a later failed turn": {"turn_ended", map[string]any{"turn": 2, "by": "agent", "outcome": "failure", "reason": "crashed", "background": 0},
			"The turn failed: the agent crashed.|Subagent a ended with its turn after 1m 30s, 0 tool calls."},
		"asleep": {"state", map[string]any{"from": "idle", "to": "asleep", "reason": "idle"},
			"Subagent a ended with its agent after 1m 30s, 0 tool calls."},
		"closed": {"state", map[string]any{"from": "idle", "to": "closed"},
			"Subagent a ended with its agent after 1m 30s, 0 tool calls."},
	} {
		t.Run(name, func(t *testing.T) {
			c := &clock{t: t0}
			f := newFront(t, workspaces{}, withClock(c))
			id, trigger := f.started(t)
			f.showing(t)
			f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
			f.at(t, id, 10, "subagent_started", map[string]any{"id": "s1", "name": "a"})
			f.at(t, id, 20, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "completion", "background": 1, "subagents": []string{"s1"}})
			f.at(t, id, 100, tc.kind, tc.data)
			f.at(t, id, 101, "subagent_ended", map[string]any{"id": "s1"})
			f.at(t, id, 102, "turn_ended", map[string]any{"turn": 3, "by": "agent", "outcome": "completion", "background": 0})
			f.at(t, id, 110, "text", map[string]any{"text": "marker", "final": false})
			got := texts(f.postedUntil(t, id, "marker"))
			if want := "the prompt|Subagent a started.|" + tc.want + "|marker"; got != want {
				t.Fatalf("posts\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A turn that ends on a question says nothing of the subagents: they go on,
// in the background or when the question is answered.
func TestSubagentIsNotClosedByAQuestion(t *testing.T) {
	c := &clock{t: t0}
	f := newFront(t, workspaces{}, withClock(c))
	id, trigger := f.started(t)
	f.showing(t)
	f.at(t, id, 0, "turn_started", map[string]any{"turn": 1, "by": "caller", "key": trigger.Ref.ID})
	f.at(t, id, 10, "subagent_started", map[string]any{"id": "s1", "name": "a"})
	f.at(t, id, 20, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "blocked", "background": 0})
	f.at(t, id, 30, "turn_started", map[string]any{"turn": 2, "by": "caller"})
	f.at(t, id, 75, "subagent_ended", map[string]any{"id": "s1"})
	f.at(t, id, 90, "text", map[string]any{"text": "marker", "final": false})
	got := texts(f.postedUntil(t, id, "marker"))
	if want := "the prompt|Subagent a started.|Subagent a ended after 1m 05s, 0 tool calls.|marker"; got != want {
		t.Fatalf("posts\n%s\nwant\n%s", got, want)
	}
}
