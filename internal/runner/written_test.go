package runner

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/store"
)

// lastTurnReason is the reason of the last turn_ended event.
func lastTurnReason(t *testing.T, st *store.Store) string {
	t.Helper()
	var r *string
	st.DB.QueryRow(`SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'turn_ended' ORDER BY pos DESC LIMIT 1`).Scan(&r)
	if r == nil {
		return ""
	}
	return *r
}

// The daemon dies right after it wrote input to the agent, before the
// agent's report of it is read: the new daemon tells the codec, so the
// input runs once, its id comes back and its row clears.
func TestWrittenInputIsNotWrittenAgain(t *testing.T) {
	cases := []struct {
		name  string
		setup string // the first turn's script
		ready string // the event the first turn must have reached
	}{
		{name: "send to an idle thread", setup: "reply one", ready: "turn_ended"},
		// Held by the codec during the turn, written when it ends.
		{name: "send during a turn", setup: "tool wait 2s\nreply one", ready: "tool_started"},
	}
	forms(t, func(t *testing.T, terminal bool) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				t.Cleanup(func() { killSessions(root) })
				d := runDaemon(t, root, terminal, "start+slow", tc.setup)
				st := openStore(t, root)
				until(t, "the first turn", st, tc.ready, 1)
				d.kill()
				runDaemon(t, root, terminal, "die-send+slow", "input written", "reply two").waitKilled(t)
				d = runDaemon(t, root, terminal, "resume+slow")
				until(t, "the second turn's end", st, "turn_ended", 2)
				time.Sleep(2 * time.Second) // a second copy would have been taken by now
				d.kill()
				got := counts(t, st)
				if got["turn_started"] != 2 || got["turn_ended"] != 2 || withText(t, st, "text", "two") != 1 || withText(t, st, "input", "reply two") != 1 {
					t.Errorf("events %v; %d answers \"two\"", got, withText(t, st, "text", "two"))
				}
				if r := lastTurnReason(t, st); r != "" {
					t.Errorf("the second turn ended with %q", r)
				}
				if n := count(t, st, "pending_inputs"); n != 0 {
					t.Errorf("%d pending inputs left: its id did not come back", n)
				}
			})
		}
	})
}

// The daemon dies after it marked input written and before it wrote it:
// the input is not lost silently, its turn fails as "not delivered", and
// its row clears.
func TestInputMarkedButNotWrittenFailsVisibly(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		root := t.TempDir()
		t.Cleanup(func() { killSessions(root) })
		d := runDaemon(t, root, terminal, "start", "reply one")
		st := openStore(t, root)
		until(t, "the first turn", st, "turn_ended", 1)
		d.kill()
		runDaemon(t, root, terminal, "die-send", "input marked", "reply two").waitKilled(t)
		d = runDaemon(t, root, terminal, "resume+quick")
		until(t, "the second turn's end", st, "turn_ended", 2)
		time.Sleep(300 * time.Millisecond)
		d.kill()
		if r := lastTurnReason(t, st); r != "not delivered" {
			t.Errorf("the second turn ended with %q, want \"not delivered\"", r)
		}
		if n := withText(t, st, "text", "two"); n != 0 {
			t.Errorf("%d answers \"two\"", n)
		}
		if n := count(t, st, "pending_inputs"); n != 0 {
			t.Errorf("%d pending inputs left", n)
		}
	})
}
