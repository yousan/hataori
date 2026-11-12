package store

import (
	"context"
	"database/sql"
)

// migrations is the schema, in order. Version n is the database after the
// first n steps. Never edit or reorder a step that has been released; add one.
var migrations = []Migration{
	{Name: "threads, workspaces, sessions, turns", Up: execAll(
		`CREATE TABLE threads (
			id         INTEGER PRIMARY KEY, -- the thread's short number
			scope      TEXT NOT NULL,
			key        TEXT NOT NULL,
			title      TEXT NOT NULL DEFAULT '',
			repo       TEXT NOT NULL DEFAULT '',
			started_by TEXT NOT NULL,
			state      TEXT NOT NULL,
			created_at INTEGER NOT NULL, -- unix milliseconds
			updated_at INTEGER NOT NULL,
			UNIQUE (scope, key)
		)`,
		`CREATE TABLE workspaces (
			id        INTEGER PRIMARY KEY,
			thread_id INTEGER NOT NULL UNIQUE REFERENCES threads (id),
			path      TEXT NOT NULL UNIQUE, -- relative to the instance root
			remote    TEXT NOT NULL,
			state     TEXT NOT NULL
		)`,
		`CREATE TABLE sessions (
			id               INTEGER PRIMARY KEY,
			thread_id        INTEGER NOT NULL REFERENCES threads (id),
			agent            TEXT NOT NULL, -- registered adapter name
			agent_session_id TEXT NOT NULL DEFAULT '',
			state            TEXT NOT NULL,
			model            TEXT NOT NULL DEFAULT '',
			effort           TEXT NOT NULL DEFAULT '',
			host_pid         INTEGER NOT NULL DEFAULT 0,
			log_offset       INTEGER NOT NULL DEFAULT 0,
			last_seq         INTEGER NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL
		)`,
		`CREATE INDEX sessions_thread ON sessions (thread_id)`,
		`CREATE TABLE turns (
			id         INTEGER PRIMARY KEY,
			session_id INTEGER NOT NULL REFERENCES sessions (id),
			started_by TEXT NOT NULL,
			trigger    TEXT NOT NULL DEFAULT '', -- key of the call that started it
			started_at INTEGER NOT NULL,
			ended_at   INTEGER,
			status     TEXT NOT NULL DEFAULT '',
			reason     TEXT NOT NULL DEFAULT '',
			background INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX turns_session ON turns (session_id)`,
	)},
	{Name: "inbox, events", Up: execAll(
		`CREATE TABLE inbox (
			key         TEXT PRIMARY KEY, -- the caller's safe-to-repeat key
			data        BLOB NOT NULL,
			accepted_at INTEGER NOT NULL
		)`,
		`CREATE TABLE events (
			pos    INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused
			scope  TEXT NOT NULL,
			thread TEXT NOT NULL,
			time   INTEGER NOT NULL,
			kind   TEXT NOT NULL,
			data   BLOB NOT NULL
		)`,
		`CREATE INDEX events_scope ON events (scope, thread, pos)`,
	)},
	{Name: "turns.principal", Up: execAll(
		`ALTER TABLE turns ADD COLUMN principal TEXT NOT NULL DEFAULT ''`,
	)},
	{Name: "login outages", Up: execAll(
		`CREATE TABLE login_outages (
			id        INTEGER PRIMARY KEY,
			agent     TEXT NOT NULL, -- registered adapter name
			opened_at INTEGER NOT NULL,
			closed_at INTEGER
		)`,
		`CREATE UNIQUE INDEX login_outages_open ON login_outages (agent) WHERE closed_at IS NULL`,
		`CREATE TABLE login_outage_threads (
			outage_id INTEGER NOT NULL REFERENCES login_outages (id),
			thread_id INTEGER NOT NULL REFERENCES threads (id),
			PRIMARY KEY (outage_id, thread_id)
		)`,
	)},
	{Name: "runner: session spec, first input, followed files", Up: execAll(
		// spec: JSON, what the codec is made from; first_input: JSON, sent
		// once the agent reports that it is ready.
		`ALTER TABLE sessions ADD COLUMN spec TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN first_input TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE session_files (
			session_id INTEGER NOT NULL REFERENCES sessions (id),
			path       TEXT NOT NULL,
			offset     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, path)
		)`,
	)},
}

func execAll(stmts ...string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return err
			}
		}
		return nil
	}
}
