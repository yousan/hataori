package session

import (
	"database/sql"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

// setAside makes the machine's session run one input in a conversation of
// its own: its own conversation, "main", is set aside, and what it follows
// there is marked.
func setAside(t *testing.T, m *Machine) {
	t.Helper()
	for _, q := range []string{
		`UPDATE sessions SET agent_session_id = 'fresh', set_aside = 'main' WHERE id = 1`,
		`INSERT INTO session_files (session_id, path, offset, aside) VALUES (1, 'main.jsonl', 10, 0), (1, 'fresh.jsonl', 5, 1)`,
	} {
		if _, err := m.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// givenBack checks that the session has its own conversation back, nothing set
// aside, and only its own files followed.
func givenBack(t *testing.T, m *Machine, state control.State) {
	t.Helper()
	var (
		id    string
		aside sql.NullString
		st    string
		files int
	)
	if err := m.Store.DB.QueryRow(`SELECT agent_session_id, set_aside, state FROM sessions WHERE id = 1`).Scan(&id, &aside, &st); err != nil {
		t.Fatal(err)
	}
	m.Store.DB.QueryRow(`SELECT count(*) FROM session_files WHERE session_id = 1`).Scan(&files)
	if id != "main" || aside.Valid || control.State(st) != state || files != 1 {
		t.Fatalf("conversation %q (aside %v), %s, %d files; want main, nothing aside, %s, 1 file", id, aside, st, files, state)
	}
}

// A session in a conversation of one input's own gets its own back when it
// goes asleep, also when that agent crashes in a turn or cannot start; the
// files it followed for it go, and its turn is marked as that
// conversation's.
func TestSetAsideConversationIsGivenBack(t *testing.T) {
	t.Run("crash in a turn", func(t *testing.T) {
		m, id := newMachine(t, control.Creating)
		setAside(t, m)
		feed(t, m, id, ag(agent.Event{Seq: 1, Kind: agent.SessionReady, SessionID: "fresh"}),
			Input{Kind: Send, Key: "hataori-wake-1-1", Text: "wake"},
			Input{Kind: Exit})
		givenBack(t, m, control.Asleep)
		var aside int
		if err := m.Store.DB.QueryRow(`SELECT aside FROM turns WHERE session_id = 1`).Scan(&aside); err != nil || aside != 1 {
			t.Fatalf("the turn's conversation: %d, %v; want marked", aside, err)
		}
	})
	for name, in := range map[string]Input{
		"exit before ready": {Kind: Exit, Reason: "start: no"},
		"start timeout":     {Kind: StartTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			m, id := newMachine(t, control.Creating)
			setAside(t, m)
			feed(t, m, id, in)
			givenBack(t, m, control.Asleep)
		})
	}
	// A session whose own conversation cannot start is failed, as before.
	m, id := newMachine(t, control.Creating)
	feed(t, m, id, Input{Kind: Exit, Reason: "start: no"})
	var st string
	m.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&st)
	if control.State(st) != control.Failed {
		t.Fatalf("an ordinary session that cannot start is %s, want failed", st)
	}
}
