//go:build ignore

// Run with the previous version of internal/store, as described in README.md.
// It uses synthetic records only and never opens an existing instance.
package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yousan/hataori/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		panic("expected output .db.gz path")
	}
	root, err := os.MkdirTemp("", "machine-local-fixture-")
	check(err)
	defer os.RemoveAll(root)
	check(os.Mkdir(filepath.Join(root, "state"), 0700))
	s, err := store.Open(context.Background(), root)
	check(err)
	v, err := s.Version(context.Background())
	check(err)
	fmt.Println("previous schema version:", v)
	for _, query := range []string{
		`INSERT INTO startup_steps (name, done_at) VALUES ('thread links', 1)`,
		`INSERT INTO threads (id, scope, key, title, repo, started_by, state, created_at, updated_at, workspace, confined, kind)
		 VALUES (11, 'room', 'legacy', 'Unpushed work', 'https://example.test/team/repo', 'ada', 'running', 1, 2, 'workspaces/legacy/room/thread', 0, 'ordinary'),
		 (12, 'room', 'own', 'Supervisor', '', 'ada', 'idle', 1, 2, 'confined/legacy', 1, 'supervisor'),
		 (13, 'room', 'conversation', 'Conversation', '', 'ada', 'idle', 1, 2, 'confined/legacy', 1, 'conversation'),
		 (14, 'room', 'lookup', 'Lookup', '', 'ada', 'asleep', 1, 2, 'confined/legacy', 1, 'lookup')`,
		`INSERT INTO sessions (id, thread_id, agent, agent_session_id, state, created_at, updated_at, spec, first_input, host_pid, last_seq, log_offset, log_start)
		 VALUES (41, 11, 'fake', 'old-conversation', 'running', 1, 2,
		 '{"agent":"fake","spec":{"Dir":"workspaces/legacy/room/thread","StateDir":"state/sessions/41","ConfigDir":"agent-home"},"run":"prior-host-run"}',
		 '{"dir":"workspaces/legacy/room/thread","text":"keep this input"}', 12345, 17, 2048, 512),
		 (42, 12, 'fake', 'supervisor-conversation', 'idle', 1, 2,
		 '{"agent":"fake","spec":{"Dir":"confined/legacy","StateDir":"state/sessions/42","Confine":{"MCPServers":["bound-server"]}}}', '', 12346, 3, 1024, 0)`,
		`INSERT INTO session_files (session_id, path, offset, start, aside) VALUES (41, 'state/sessions/41/events.ndjson', 900, 100, 0)`,
		`INSERT INTO pending_inputs (session_id, data) VALUES (41, '{"text":"queued"}')`,
		`INSERT INTO turns (id, session_id, started_by, started_at, principal, background) VALUES (51, 41, 'ada', 1, 'ada', 2)`,
		`INSERT INTO workspaces (thread_id, path, remote, state) VALUES (11, 'workspaces/legacy/room/thread', 'https://example.test/team/repo', 'ready')`,
		`INSERT INTO links (thread_id, client, channel, thread, made_by, created_at) VALUES (11, 'chat', 'room', 'legacy', 'ada', 1)`,
		`INSERT INTO agent_limits (agent, at, every, windows) VALUES ('fake', 2000, 60000, '[{"name":"hour","used":17}]')`,
		`INSERT INTO login_outages (id, agent, config_dir, opened_at) VALUES (61, 'fake', 'agent-home', 1000)`,
		`INSERT INTO login_outage_threads (outage_id, thread_id) VALUES (61, 11)`,
	} {
		_, err := s.DB.Exec(query)
		check(err)
	}
	check(s.Close())
	b, err := os.ReadFile(filepath.Join(root, store.File))
	check(err)
	f, err := os.Create(os.Args[1])
	check(err)
	z := gzip.NewWriter(f)
	_, err = z.Write(b)
	check(err)
	check(z.Close())
	check(f.Close())
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
