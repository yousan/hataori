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
		// launch: 1 when the core started or woke the session and the
		// runner has not launched its agent yet, 2 while the runner starts
		// its host, 0 once the host is recorded as started; first_input
		// holds what a launch needs.
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
	{Name: "chat: event reader", Up: execAll(
		// The position after which each chat front end reads events again.
		`CREATE TABLE chat_cursors (
			client TEXT PRIMARY KEY,
			pos    INTEGER NOT NULL
		)`,
	)},
	{Name: "chat: login outages", Up: execAll(
		// The owner's direct message about each login outage: the threads
		// it stopped and the message's reference. Never the sign-in link
		// or a code. done: the message shows the end of the outage.
		`CREATE TABLE chat_logins (
			client     TEXT NOT NULL, -- the chat front end's name
			outage     INTEGER NOT NULL,
			agent      TEXT NOT NULL,
			threads    TEXT NOT NULL, -- JSON array of thread ids
			dm_channel TEXT NOT NULL DEFAULT '',
			dm_thread  TEXT NOT NULL DEFAULT '',
			dm_ref     TEXT NOT NULL DEFAULT '',
			closed     INTEGER NOT NULL DEFAULT 0,
			done       INTEGER NOT NULL DEFAULT 0,
			ended      TEXT NOT NULL DEFAULT '', -- how the last relay flow ended (expired, failed), for after a restart
			PRIMARY KEY (client, outage)
		)`,
	)},
	{Name: "pending_inputs.written", Up: execAll(
		// written: the input was written to the agent, and its report not
		// read yet; after a restart the codec is told, and writes nothing.
		`ALTER TABLE pending_inputs ADD COLUMN written INTEGER NOT NULL DEFAULT 0`,
	)},
	{Name: "chat: inbound cursors", Up: execAll(
		// Per channel and thread, the cursor (surface.Inbound.Cursor) of
		// the last message the front end has handled, with every message
		// before it. Catch-up asks the driver for the messages after it.
		`CREATE TABLE chat_inbound (
			client  TEXT NOT NULL, -- the chat front end's name
			channel TEXT NOT NULL,
			thread  TEXT NOT NULL DEFAULT '',
			cursor  TEXT NOT NULL, -- '' until a message was handled
			since   INTEGER NOT NULL DEFAULT 0, -- unix ms: with cursor '', catch up on messages from then
			PRIMARY KEY (client, channel, thread)
		)`,
	)},
	{Name: "chat_triggers.thread", Up: execAll(
		// thread: the chat thread opened from the trigger message, '' until
		// one is, so that the trigger delivered again opens no second one.
		`ALTER TABLE chat_triggers ADD COLUMN thread TEXT NOT NULL DEFAULT ''`,
		// A trigger that started a thread before this step keeps it: the
		// core accepted its start under 'start:' scope NUL key, with the
		// thread's id as data, and a chat scope is its channel.
		`UPDATE chat_triggers SET thread = COALESCE((SELECT json_extract(CAST(data AS TEXT), '$.key') FROM inbox
			WHERE inbox.key = 'start:' || chat_triggers.channel || char(0) || chat_triggers.message), '')`,
	)},
	// The "<client>:<id>" the input that started a turn came from, beside
	// the person in principal (design, section 6.2).
	{Name: "turns.from_id", Up: execAll(
		`ALTER TABLE turns ADD COLUMN from_id TEXT NOT NULL DEFAULT ''`,
	)},
	{Name: "token counts", Up: execAll(
		// tokens: the highest total of tokens reported in the turn (input, output,
		// cache reads and writes), which the agent reports as the turn's so far.
		`ALTER TABLE turns ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0`,
		// tokens_turn: the last reported turn total, NULL until a report.
		// tokens_today: the tokens counted in the day tokens_day (YYYY-MM-DD).
		// context_used, context_size: the last context use reported.
		`ALTER TABLE threads ADD COLUMN tokens_turn INTEGER`,
		`ALTER TABLE threads ADD COLUMN tokens_day TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ADD COLUMN tokens_today INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE threads ADD COLUMN context_used INTEGER`,
		`ALTER TABLE threads ADD COLUMN context_size INTEGER`,
	)},
	{Name: "session_reads", Up: execAll(
		// What the runner gave a session's codec, in order: one row per
		// range of complete lines read from the host's log (path '') or a
		// followed file, written in the same commit as their events. A
		// codec rebuilt later is given the same ranges in the same order.
		`CREATE TABLE session_reads (
			session_id INTEGER NOT NULL REFERENCES sessions (id),
			n          INTEGER NOT NULL, -- the read's place, from 1, in its session
			path       TEXT NOT NULL,
			start      INTEGER NOT NULL, -- offsets in that file, the end excluded
			end        INTEGER NOT NULL,
			PRIMARY KEY (session_id, n)
		)`,
		// Where the current agent's lines begin in a followed file, as
		// sessions.log_start for the log: a woken agent's codec does not read
		// what its file held before.
		`ALTER TABLE session_files ADD COLUMN start INTEGER NOT NULL DEFAULT 0`,
	)},
	{Name: "agent limits", Up: execAll(
		// The newest remaining usage each agent reported, from any of its
		// sessions: the windows as JSON, when it was reported and how often
		// its sessions report it.
		`CREATE TABLE agent_limits (
			agent   TEXT PRIMARY KEY,
			at      INTEGER NOT NULL, -- Unix ms
			every   INTEGER NOT NULL, -- ms
			windows TEXT NOT NULL
		)`,
	)},
	// Thread numbers (design, section 6.2): a thread is named by its id, and
	// a chat conversation is a link to it. The rows of existing threads are
	// filled once at daemon start, by LinkThreads, since that needs the
	// config.
	{Name: "thread numbers: links", ForeignKeysOff: true, Up: execAll(
		// threads again, without UNIQUE (scope, key) and with the
		// workspace's path, so that the path is read, never computed.
		`CREATE TABLE threads_new (
			id           INTEGER PRIMARY KEY, -- the thread's number
			scope        TEXT NOT NULL,
			key          TEXT NOT NULL, -- no longer an identity
			title        TEXT NOT NULL DEFAULT '',
			repo         TEXT NOT NULL DEFAULT '',
			started_by   TEXT NOT NULL,
			state        TEXT NOT NULL,
			created_at   INTEGER NOT NULL, -- unix milliseconds
			updated_at   INTEGER NOT NULL,
			tokens_turn  INTEGER,
			tokens_day   TEXT NOT NULL DEFAULT '',
			tokens_today INTEGER NOT NULL DEFAULT 0,
			context_used INTEGER,
			context_size INTEGER,
			workspace    TEXT NOT NULL DEFAULT '' -- as the sessions recorded it; '' until one is made
		)`,
		`INSERT INTO threads_new (id, scope, key, title, repo, started_by, state, created_at, updated_at,
			tokens_turn, tokens_day, tokens_today, context_used, context_size)
		SELECT id, scope, key, title, repo, started_by, state, created_at, updated_at,
			tokens_turn, tokens_day, tokens_today, context_used, context_size FROM threads`,
		`DROP TABLE threads`,
		`ALTER TABLE threads_new RENAME TO threads`,
		`CREATE INDEX threads_scope ON threads (scope, key)`,
		// A chat conversation that shows a thread: the client's name, and
		// the channel and thread on that service. A thread has any number.
		`CREATE TABLE links (
			id         INTEGER PRIMARY KEY,
			thread_id  INTEGER NOT NULL REFERENCES threads (id),
			client     TEXT NOT NULL, -- the chat front end's name
			channel    TEXT NOT NULL,
			thread     TEXT NOT NULL DEFAULT '',
			made_by    TEXT NOT NULL DEFAULT '', -- the person who made it
			created_at INTEGER NOT NULL, -- unix milliseconds
			UNIQUE (client, channel, thread)
		)`,
		`CREATE INDEX links_thread ON links (thread_id)`,
		// The thread's number beside (scope, thread), which stay for reads
		// by scope. NULL for an event of no thread.
		`ALTER TABLE events ADD COLUMN thread_id INTEGER REFERENCES threads (id)`,
		`CREATE INDEX events_thread ON events (thread_id, pos)`,
		// The link a post goes to; NULL for a post outside a linked
		// conversation.
		`ALTER TABLE chat_outbox ADD COLUMN link INTEGER REFERENCES links (id)`,
		// Steps that run once at daemon start, after the migrations, by name.
		`CREATE TABLE startup_steps (
			name    TEXT PRIMARY KEY,
			done_at INTEGER NOT NULL -- unix milliseconds
		)`,
	)},
	{Name: "chat: notices", Up: execAll(
		// A message the front end answered with a notice of its own, such
		// as one in a closed thread. Its rowid keys the notice's outbox
		// row, so that the message delivered again writes it once.
		`CREATE TABLE chat_notices (
			client      TEXT NOT NULL, -- the chat front end's name
			channel     TEXT NOT NULL,
			message     TEXT NOT NULL, -- the surface's message id
			received_at INTEGER NOT NULL,
			PRIMARY KEY (client, channel, message)
		)`,
	)},
	// The chat front end's list of the threads a login outage stopped holds
	// their numbers; before, each was {"scope", "key"}.
	{Name: "chat_logins.threads by number", Up: execAll(
		`UPDATE chat_logins SET threads = coalesce((SELECT json_group_array(t.id) FROM json_each(chat_logins.threads) j
			JOIN threads t ON t.scope = json_extract(j.value, '$.scope') AND t.key = json_extract(j.value, '$.key')), '[]')
		WHERE json_type(threads, '$[0]') = 'object'`,
	)},
	// One outbox row per link (design, section 5.2): a thread shown in two
	// conversations of one front end gets the same event's messages in
	// each. link is 0 for a message outside a linked conversation, such
	// as a direct message, so that the key holds for those too.
	{Name: "chat_outbox: rows per link", Up: execAll(
		`CREATE TABLE chat_outbox_new (
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
			link       INTEGER NOT NULL DEFAULT 0, -- links.id of the conversation; 0 for none
			UNIQUE (client, event, link, part)
		)`,
		`INSERT INTO chat_outbox_new (id, client, event, part, channel, thread, message, decoration, state, failures, ref,
			created_at, sent_at, link)
		SELECT id, client, event, part, channel, thread, message, decoration, state, failures, ref, created_at, sent_at,
			coalesce(link, (SELECT l.id FROM links l WHERE l.client = chat_outbox.client AND l.channel = chat_outbox.channel
				AND l.thread = chat_outbox.thread), 0)
		FROM chat_outbox`,
		`DROP TABLE chat_outbox`,
		`ALTER TABLE chat_outbox_new RENAME TO chat_outbox`,
		`CREATE INDEX chat_outbox_pending ON chat_outbox (client, channel, thread, id) WHERE state = 'pending'`,
	)},
	// A thread started confined (design, section 7.1): every launch of its
	// sessions is confined, in a directory of its own with no repository.
	{Name: "threads.confined", Up: execAll(
		`ALTER TABLE threads ADD COLUMN confined INTEGER NOT NULL DEFAULT 0`,
	)},
	// Where each scope's supervisor waker is in the event log (design,
	// section 7.1), and the wake it was sending, if any: a daemon that died
	// during the send sends the same text under the same key again.
	{Name: "supervisor_wakes", Up: execAll(
		`CREATE TABLE supervisor_wakes (
			scope    TEXT PRIMARY KEY,
			pos      INTEGER NOT NULL, -- the events up to here are handled
			send_to  INTEGER, -- the position the wake being sent covers; NULL for none
			send_key TEXT,
			send_text TEXT
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
