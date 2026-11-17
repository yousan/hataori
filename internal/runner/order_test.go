package runner

import (
	"context"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Input pending at a reconnect reaches the agent before input sent right
// after the reconnect.
func TestPendingInputGoesBeforeNewInput(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		root := t.TempDir()
		t.Cleanup(func() { killSessions(root) })
		d := runDaemon(t, root, terminal, "start", "tool wait 2s\nreply one")
		st := openStore(t, root)
		until(t, "the tool", st, "tool_started", 1)
		d.kill()
		// "reply two" is accepted, and the daemon dies before handing it over.
		runDaemon(t, root, terminal, "die", "send", "reply two").waitKilled(t)

		// The new daemon is this test process. It reads the logs only every
		// half second, so a Send right after the reconnect comes first
		// unless it waits for the pending input.
		log := &syncBuffer{}
		st2, c, r, err := build(root, options{terminal: terminal, poll: 500 * time.Millisecond}, log)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { r.Close(); st2.Close() }()
		if err := r.Reconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := owner(c).Send(context.Background(), thread, "d3", control.Input{Text: "reply three"}, control.Deliver); err != nil {
			t.Fatalf("send: %v\n%s", err, log.String())
		}
		until(t, "three turns", st, "turn_ended", 3)
		var got []string
		rows, err := st.DB.Query(`SELECT json_extract(data, '$.text') FROM events WHERE kind = 'text' ORDER BY pos`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			got = append(got, s)
		}
		rows.Close()
		if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
			t.Fatalf("the turns' answers in order: %q, want one, two, three", got)
		}
	})
}
