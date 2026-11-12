package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// turn runs one turn on a session and ends it with status and reason.
func turn(t *testing.T, m *Machine, id int64, key string, status agent.EndStatus, reason agent.Reason) {
	t.Helper()
	feed(t, m, id, Input{Kind: Send, Key: key}, ag(agent.Event{Kind: agent.TurnEnded, Status: status, Reason: reason}))
}

func loginEvents(t *testing.T, m *Machine) []logged {
	var out []logged
	for _, e := range events(t, m) {
		if e.Kind == KindLoginLost || e.Kind == KindLoginRestored {
			out = append(out, e)
		}
	}
	return out
}

// addSession adds a second thread with an idle session of the same agent.
func addSession(t *testing.T, m *Machine) int64 {
	t.Helper()
	if _, err := m.Store.DB.Exec(`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (2, 's', 'k2', 'p', 'idle', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.DB.Exec(`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (2, 2, 'fake', 'idle', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	return 2
}

func TestLoginOutage(t *testing.T) {
	m, a := newMachine(t, control.Idle)
	b := addSession(t, m)

	turn(t, m, a, "a1", agent.Failed, agent.Limit)
	if n := len(loginEvents(t, m)); n != 0 {
		t.Fatalf("a failure for another reason wrote %d login events", n)
	}

	turn(t, m, a, "a2", agent.Failed, agent.Login)
	turn(t, m, b, "b1", agent.Failed, agent.Login)
	turn(t, m, a, "a3", agent.Failed, agent.Login)
	got := loginEvents(t, m)
	if len(got) != 1 || got[0].Kind != KindLoginLost {
		t.Fatalf("login events after three login failures: %v", got)
	}

	turn(t, m, b, "b2", agent.Completed, "")
	got = loginEvents(t, m)
	if len(got) != 2 || got[1].Kind != KindLoginRestored {
		t.Fatalf("login events after a completed turn: %v", got)
	}
	threads, _ := got[1].Data["threads"].([]any)
	if len(threads) != 2 {
		t.Fatalf("restored event names %v, want both threads", got[1].Data["threads"])
	}

	turn(t, m, a, "a4", agent.Completed, "")
	if n := len(loginEvents(t, m)); n != 2 {
		t.Fatalf("a completed turn without an outage wrote a login event")
	}
	turn(t, m, a, "a5", agent.Failed, agent.Login)
	got = loginEvents(t, m)
	if len(got) != 3 || got[2].Kind != KindLoginLost || got[2].Data["outage"] == got[0].Data["outage"] {
		t.Fatalf("a new outage after a closed one: %v", got)
	}
}

func TestLoginOutageSurvivesRestart(t *testing.T) {
	m, a := newMachine(t, control.Idle)
	b := addSession(t, m)
	turn(t, m, a, "a1", agent.Failed, agent.Login)

	// Restart: a new store and machine on the same database.
	var seqno int
	var name, file string
	if err := m.Store.DB.QueryRow(`PRAGMA database_list`).Scan(&seqno, &name, &file); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(file))
	m.Store.Close()
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m = &Machine{Store: st}

	turn(t, m, b, "b1", agent.Failed, agent.Login)
	if got := loginEvents(t, m); len(got) != 1 {
		t.Fatalf("after a restart: %v", got)
	}
	var open int
	st.DB.QueryRow(`SELECT count(*) FROM login_outages WHERE closed_at IS NULL`).Scan(&open)
	if open != 1 {
		t.Fatalf("%d open outages", open)
	}
}
