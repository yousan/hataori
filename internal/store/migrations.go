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
	{Name: "requests", Up: execAll(
		// One row per question or approval the agent asked. state is open,
		// answered, withdrawn (by the agent) or closed (the agent's process
		// ended while it was open).
		`CREATE TABLE requests (
			id         INTEGER PRIMARY KEY,
			session_id INTEGER NOT NULL REFERENCES sessions (id),
			request    TEXT NOT NULL, -- the agent's request id, unique in its session
			handle     TEXT NOT NULL UNIQUE, -- the control API's RequestID
			approval   INTEGER NOT NULL DEFAULT 0,
			text       TEXT NOT NULL DEFAULT '',
			choices    TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
			state      TEXT NOT NULL,
			opened_at  INTEGER NOT NULL,
			closed_at  INTEGER,
			UNIQUE (session_id, request)
		)`,
	)},
	{Name: "sessions.log_start", Up: execAll(
		// Where the lines of the session's current agent begin in its
		// events log; earlier lines are those of an agent put to sleep.
		`ALTER TABLE sessions ADD COLUMN log_start INTEGER NOT NULL DEFAULT 0`,
	)},
	{Name: "pending input", Up: execAll(
		// launch: the core started or woke the session and the runner has
		// not launched its agent yet; first_input holds what it needs.
		`ALTER TABLE sessions ADD COLUMN launch INTEGER NOT NULL DEFAULT 0`,
		// Input or an answer the core accepted for a session's agent and
		// the runner has not handed over yet. request is the agent's id of
		// the request an answer is for, '' for input. data: JSON, the
		// control.Input or control.Answer.
		`CREATE TABLE pending_inputs (
			id         INTEGER PRIMARY KEY,
			session_id INTEGER NOT NULL REFERENCES sessions (id),
			request    TEXT NOT NULL DEFAULT '',
			data       TEXT NOT NULL
		)`,
	)},
	{Name: "chat: outbox", Up: execAll(
		// One row per message the chat front end posts, in the order it
		// was written. state is pending, sent or dropped (a decoration that
		// failed too often).
		`CREATE TABLE chat_outbox (
			id         INTEGER PRIMARY KEY,
			client     TEXT NOT NULL, -- the chat front end's name
			event      INTEGER NOT NULL, -- position of the event it shows
			part       INTEGER NOT NULL,
			channel    TEXT NOT NULL,
			thread     TEXT NOT NULL DEFAULT '',
			message    TEXT NOT NULL, -- JSON
			decoration INTEGER NOT NULL DEFAULT 0,
			state      TEXT NOT NULL DEFAULT 'pending',
			failures   INTEGER NOT NULL DEFAULT 0,
			ref        TEXT NOT NULL DEFAULT '', -- the surface's message id, once sent
			created_at INTEGER NOT NULL,
			sent_at    INTEGER,
			UNIQUE (client, event, part)
		)`,
		`CREATE INDEX chat_outbox_pending ON chat_outbox (client, channel, thread, id) WHERE state = 'pending'`,
	)},
	{Name: "chat: triggers", Up: execAll(
		// The text of each trigger message as it first arrived, so that a
		// message delivered again after an edit starts what the first did.
		`CREATE TABLE chat_triggers (
			client      TEXT NOT NULL, -- the chat front end's name
			channel     TEXT NOT NULL,
			message     TEXT NOT NULL, -- the surface's message id
			text        TEXT NOT NULL,
			received_at INTEGER NOT NULL,
			PRIMARY KEY (client, channel, message)
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
