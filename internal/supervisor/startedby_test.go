package supervisor

import (
	"context"
	"testing"
)

// The supervisor thread that started a thread is the scope's confined one
// open at the thread's creation, else the newest before it; none when the
// starter is not that scope's supervisor.
func TestStartedBy(t *testing.T) {
	c := remindStore(t)
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, confined, kind, created_at, updated_at) VALUES
			(1, 'c1', 'sv1', 'p', 'idle', 1, 'supervisor', 0, 0), (2, 'c1', 'sv2', 'p', 'closed', 1, 'supervisor', 0, 0), (3, 'c2', 'sv', 'p', 'idle', 1, 'supervisor', 0, 0),
			(4, 'c1', 'w', 'p', 'failed', 0, 'ordinary', 0, 0)`,
		// #1 is closed at 20, when #2 takes over; #2 is closed at 40, and #1
		// reopened at 50.
		`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
			(10, 'c1', '', 1, 0, 'thread_created', '{}'), (11, 'c1', '', 1, 0, 'state', '{"from":"creating","to":"idle"}'),
			(12, 'c2', '', 3, 0, 'thread_created', '{}'),
			(20, 'c1', '', 1, 0, 'state', '{"from":"idle","to":"closed"}'),
			(21, 'c1', '', 2, 0, 'thread_created', '{}'), (22, 'c1', '', 2, 0, 'state', '{"from":"creating","to":"idle"}'),
			(23, 'c1', '', 4, 0, 'thread_created', '{}'),
			(40, 'c1', '', 2, 0, 'state', '{"from":"idle","to":"closed"}'),
			(50, 'c1', '', 1, 0, 'state', '{"from":"closed","to":"asleep"}')`,
	} {
		if _, err := c.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for what, tc := range map[string]struct {
		scope, from string
		pos         uint64
		want        int64
	}{
		"before any":                        {"c1", ID("c1"), 5, 0},
		"the first":                         {"c1", ID("c1"), 15, 1},
		"the second, which is open":         {"c1", ID("c1"), 30, 2},
		"none open: the newest":             {"c1", ID("c1"), 45, 2},
		"the first, reopened":               {"c1", ID("c1"), 60, 1},
		"a person":                          {"c1", "chat:someone", 30, 0},
		"another scope's supervisor":        {"c1", ID("c2"), 30, 0},
		"the other scope's own supervisor":  {"c2", ID("c2"), 30, 3},
		"a scope that has no supervisor":    {"c3", ID("c3"), 30, 0},
		"the scope's, created by no caller": {"c1", "", 30, 0},
	} {
		got, err := StartedBy(context.Background(), c.Store.DB, tc.scope, tc.from, tc.pos)
		if err != nil || int64(got) != tc.want {
			t.Errorf("%s: #%d, %v; want #%d", what, got, err, tc.want)
		}
	}
}
