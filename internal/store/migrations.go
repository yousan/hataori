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
	// The names the chat services show for conversations: a scope's own
	// channel (thread ''), or a linked thread (SetName).
	{Name: "names", Up: execAll(
		`CREATE TABLE names (
			client     TEXT NOT NULL,
			channel    TEXT NOT NULL,
			thread     TEXT NOT NULL DEFAULT '',
			name       TEXT NOT NULL,
			updated_at INTEGER NOT NULL, -- unix milliseconds
			PRIMARY KEY (client, channel, thread)
		)`,
	)},
	// Who sent the input a chat front end gave the core under each key, as
	// the front end saw it: the message that gets the turn's lamp, and the
	// id to ping when the turn ends ('' for a webhook, which cannot be
	// pinged). The service tells kinds of authors only when a message
	// arrives, so they are kept here.
	{Name: "chat: senders", Up: execAll(
		`CREATE TABLE chat_senders (
			client  TEXT NOT NULL, -- the chat front end's name
			key     TEXT NOT NULL, -- the Start, Send or Answer key
			lamp    TEXT NOT NULL, -- 'message': the message below; 'prompt': the thread's prompt; 'answer': a press, pinging the turn's caller
			channel TEXT NOT NULL DEFAULT '',
			thread  TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			ping    TEXT NOT NULL DEFAULT '', -- the service's id of the sender, '' for none
			PRIMARY KEY (client, key)
		)`,
	)},
	// One row per approval a limited caller asked for (AskInterrupt): shown
	// and answered in the thread asks_in; state is open, refused,
	// interrupted or ended.
	{Name: "approvals", Up: execAll(
		`CREATE TABLE approvals (id INTEGER PRIMARY KEY, handle TEXT NOT NULL UNIQUE, asks_in INTEGER NOT NULL REFERENCES threads(id), action TEXT NOT NULL, target INTEGER NOT NULL REFERENCES threads(id), session_id INTEGER NOT NULL, turn_id INTEGER NOT NULL, reason TEXT NOT NULL, marked TEXT NOT NULL, principal TEXT NOT NULL, from_id TEXT NOT NULL, ask_key TEXT, state TEXT NOT NULL, opened_at INTEGER NOT NULL, closed_at INTEGER, UNIQUE(asks_in, ask_key))`,
	)},
	// One row per plugin that stays stopped (design, section 7.3), until a
	// run of it lasts; opened_at keys the owner's one message about it.
	{Name: "plugin outages", Up: execAll(
		`CREATE TABLE plugin_outages (
			plugin    TEXT PRIMARY KEY, -- the plugin's id
			opened_at INTEGER NOT NULL  -- unix milliseconds
		)`,
	)},
	// A thread taken in from a window a person started (design, section
	// 4.8): its workspace is that session's directory, which hataori did
	// not make and never removes.
	{Name: "threads.foreign", Up: execAll(
		`ALTER TABLE threads ADD COLUMN foreign_workspace INTEGER NOT NULL DEFAULT 0`,
	)},
	// What the plugins' message_sending handlers made of a row (design,
	// section 7.3), stored before it is posted, so that a row posted again
	// is not hooked again: '' not yet, 'passed' (with hook_text when they
	// rewrote it) or 'cancelled' (state 'cancelled', with who and why).
	{Name: "chat_outbox: message_sending", Up: execAll(
		`ALTER TABLE chat_outbox ADD COLUMN hook TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_outbox ADD COLUMN hook_text TEXT`,
		`ALTER TABLE chat_outbox ADD COLUMN hook_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_outbox ADD COLUMN hook_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chat_outbox ADD COLUMN hook_failed INTEGER NOT NULL DEFAULT 0`,
		// ThreadDetail.cancelled reads a thread's post_cancelled events.
		`CREATE INDEX events_thread_kind ON events (thread_id, kind, pos)`,
	)},
	// The person whose Stop interrupted the turn, recorded before the
	// interrupt is sent, so that a reader of the turn's end can tell a
	// stop someone asked for from a failure; '' for none.
	{Name: "turns.stopped_by", Up: execAll(
		`ALTER TABLE turns ADD COLUMN stopped_by TEXT NOT NULL DEFAULT ''`,
	)},
	// The directory of each scope's supervisor threads (design, section
	// 7.1, "Its directory and its notes"), which outlives each of them:
	// every new supervisor thread of the scope starts in it.
	{Name: "supervisor_dirs", Up: execAll(
		`CREATE TABLE supervisor_dirs (
			scope TEXT PRIMARY KEY,
			dir   TEXT NOT NULL
		)`,
	)},
	// One row per send or start that a caller with Propose proposed
	// (#650): shown and decided in the thread asks_in. Its id is the
	// proposal's number. state is open; approved, with who approved it,
	// from before its call is made until its outcome is stored; then the
	// outcome: run, refused, busy, ended, closed or failed.
	{Name: "proposals", Up: execAll(
		`CREATE TABLE proposals (
			id        INTEGER PRIMARY KEY,
			handle    TEXT NOT NULL UNIQUE,                     -- what Answer names
			asks_in   INTEGER NOT NULL REFERENCES threads(id),
			action    TEXT NOT NULL,                            -- send or start
			target    INTEGER REFERENCES threads(id),           -- a send's thread; a start's, once it ran
			question  TEXT NOT NULL DEFAULT '',                 -- a send's: the open question it answers, by its handle
			call      TEXT NOT NULL,                            -- JSON: what is run when approved
			mark      TEXT NOT NULL,                            -- the caller's mark
			principal TEXT NOT NULL,
			from_id   TEXT NOT NULL,
			ask_key   TEXT,
			state     TEXT NOT NULL,
			approved_by   TEXT NOT NULL DEFAULT '',             -- who approved it: principal, from, link (JSON) and via
			approved_from TEXT NOT NULL DEFAULT '',
			approved_link TEXT,
			approved_via  TEXT NOT NULL DEFAULT '',
			opened_at INTEGER NOT NULL,
			closed_at INTEGER,
			UNIQUE (asks_in, ask_key)
		)`,
	)},
	// A supervisor's posts that a chat front end holds back (design,
	// section 7.1, "What it posts"): the final answers of wakes under
	// report "digest", posted together at due_at; and how many times each
	// person was named during quiet hours, told when they end at end_at.
	// A row is written with the reader's cursor and removed with the
	// message that posts it, so a restart neither loses nor repeats one.
	{Name: "chat: held posts", Up: execAll(
		`CREATE TABLE chat_held (
			client TEXT NOT NULL,    -- the chat front end's name
			thread INTEGER NOT NULL, -- the supervisor's thread
			due_at INTEGER NOT NULL, -- unix milliseconds: when the digest is posted
			pos    INTEGER NOT NULL, -- the answer's event
			at     INTEGER NOT NULL, -- unix milliseconds: the answer's time
			text   TEXT NOT NULL,
			PRIMARY KEY (client, pos)
		)`,
		`CREATE TABLE chat_named (
			client TEXT NOT NULL,
			thread INTEGER NOT NULL,
			end_at INTEGER NOT NULL, -- unix milliseconds: when the quiet hours end
			name   TEXT NOT NULL,    -- the person named
			times  INTEGER NOT NULL,
			pos    INTEGER NOT NULL, -- the last event that named them
			PRIMARY KEY (client, thread, end_at, name)
		)`,
	)},
	// The user's files of a scope's supervisor (#656; design, section 7.1,
	// "Two layers"), one row per scope and file, each a hash of the file's
	// content, '' for a file not given: told, as its session was last given
	// or told it; seen, as the daemon last looked at it after a turn of the
	// supervisor's thread. With the waker: the files the wake being sent
	// names, with their hashes when it was made, which become told once it
	// was accepted; the last supervisor turn end after which the files
	// were seen, so that one read again names nothing; and what the turn
	// end after it found, until its line is posted and that is recorded.
	{Name: "supervisor_files", Up: execAll(
		`CREATE TABLE supervisor_files (
			scope TEXT NOT NULL,
			name  TEXT NOT NULL,
			told  TEXT NOT NULL,
			seen  TEXT NOT NULL,
			PRIMARY KEY (scope, name)
		)`,
		`ALTER TABLE supervisor_wakes ADD COLUMN send_files TEXT`,
		`ALTER TABLE supervisor_wakes ADD COLUMN files_pos INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE supervisor_wakes ADD COLUMN files_pending TEXT`, // JSON: what a turn end found, until its line is posted
	)},
	// The files the owner trusted for a scope's threads on one repository,
	// from a start blocked at a start-up dialog (#590), beside the config's
	// trust_files.
	{Name: "trusted_files", Up: execAll(
		`CREATE TABLE trusted_files (
			id         INTEGER PRIMARY KEY,
			scope      TEXT NOT NULL,
			repo       TEXT NOT NULL,
			file       TEXT NOT NULL,
			trusted_by TEXT NOT NULL,
			created_at INTEGER NOT NULL, -- unix milliseconds
			UNIQUE (scope, repo, file)
		)`,
	)},
	// A thread a person chose to run without its repository's settings,
	// from a start the folder-trust dialog blocked (#590): every launch of
	// it leaves the project's and the local settings files out.
	{Name: "threads.no_project_settings", Up: execAll(
		`ALTER TABLE threads ADD COLUMN no_project_settings INTEGER NOT NULL DEFAULT 0`,
	)},
	// Where each scope's supervisor waker began (Begin): a turn end at or
	// before it, from before the scope named its supervisor, waits for no
	// background task of its (#672). A scope's waker that began before
	// this column takes its stored position, since the turn ends before it
	// woke the supervisor already.
	{Name: "supervisor_wakes.since", Up: execAll(
		`ALTER TABLE supervisor_wakes ADD COLUMN since INTEGER NOT NULL DEFAULT 0`,
		`UPDATE supervisor_wakes SET since = pos`,
	)},
	// The copies the daemon wrote into docs/ of a supervisor's directory
	// (design, section 7.1, "Its directory and its notes"), each with its
	// size, SHA-256, inode and the daemon's own second link to it, which
	// holds the inode while the row names it: it writes over or removes
	// only a file there that is still one of these. pending_* is a copy
	// being put in place: a daemon that died before it recorded the copy
	// finds either.
	{Name: "supervisor_docs", Up: execAll(
		`CREATE TABLE supervisor_docs (
			dir          TEXT NOT NULL, -- the supervisor's directory
			name         TEXT NOT NULL, -- the file's name in docs/
			size         INTEGER NOT NULL,
			hash         TEXT NOT NULL, -- hex
			ino          INTEGER NOT NULL, -- the copy's inode
			keep         TEXT NOT NULL,    -- the daemon's own link to it in docs/
			pending_size INTEGER,          -- the copy being put in place, if any
			pending_hash TEXT,
			pending_ino  INTEGER,
			pending_keep TEXT,
			PRIMARY KEY (dir, name)
		)`,
	)},
	// Where a removal put the thread's workspace in trash/, so that a
	// reopen can take it back (design, section 5.1); '' for a thread whose
	// workspace is in place, or was removed before this was recorded. And
	// the owner's messages in a closed thread that the chat front end
	// holds until its Reopen button is pressed.
	{Name: "reopen", Up: execAll(
		`ALTER TABLE threads ADD COLUMN trash TEXT NOT NULL DEFAULT ''`,
		// The directory a reopen put in place and has not recorded the
		// reopen of yet: what it was (restored or remade) and its identity,
		// so that a reopen tried again after a crash knows it for its own.
		`ALTER TABLE threads ADD COLUMN reopen_placed TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE chat_reopen_held (
			client      TEXT NOT NULL, -- the chat front end's name
			channel     TEXT NOT NULL,
			message     TEXT NOT NULL, -- the surface's message id
			thread_id   INTEGER NOT NULL,
			conv_thread TEXT NOT NULL, -- the conversation it was written in
			author      TEXT NOT NULL, -- the surface's id of its author
			ping        TEXT NOT NULL, -- whom its turn's end pings; '' for nobody
			text        TEXT NOT NULL,
			received_at INTEGER NOT NULL,
			PRIMARY KEY (client, channel, message)
		)`,
	)},
	// A start the supervisor made during a person's turn (#677), under its
	// key: asked for by that person until its thread's first wake. woken is
	// then the position that wake covered, and the row goes once the
	// waker's own position has passed it.
	{Name: "supervisor_asked", Up: execAll(
		`CREATE TABLE supervisor_asked (
			scope   TEXT NOT NULL,
			key     TEXT NOT NULL,    -- the start's
			asks_in INTEGER NOT NULL, -- the supervisor's thread, where a proposal of it is shown
			person  TEXT NOT NULL,
			thread  INTEGER,          -- what the start made, once a wake carries it
			wake    TEXT,             -- the key of the wake that carries it
			woken   INTEGER,
			PRIMARY KEY (scope, key)
		)`,
	)},
	// The agent's id of the conversation a session set aside while it runs
	// one input in a conversation of its own, as a supervisor's wake does
	// (core.Core.AsFresh, #676): it is the session's id again once the
	// session is asleep, failed or closed. NULL while it runs its own.
	// A turn that ran in such a conversation, and a file followed for one,
	// are marked: the turn is no turn of the session's own conversation, and
	// the file's row goes when the session gets its own back (it is never
	// read again, and each would be one more file to poll).
	{Name: "sessions.set_aside", Up: execAll(
		`ALTER TABLE sessions ADD COLUMN set_aside TEXT`,
		`ALTER TABLE turns ADD COLUMN aside INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE session_files ADD COLUMN aside INTEGER NOT NULL DEFAULT 0`,
		// Input an agent before the session's current one left, which none
		// reports any more.
		`ALTER TABLE pending_inputs ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
	)},
	// A scope's reminders: a wake of its supervisor at a time it set, with a
	// note (design, section 7.1, #649). AUTOINCREMENT: a number names one
	// reminder for good. wake is the key of the stored wake that carries it,
	// while it is open.
	{Name: "supervisor_reminders", Up: execAll(
		`CREATE TABLE supervisor_reminders (
			id    INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			due   INTEGER NOT NULL, -- Unix milliseconds
			note  TEXT NOT NULL,
			made  INTEGER NOT NULL, -- when it was set
			state TEXT NOT NULL,    -- open, given or cancelled
			wake  TEXT
		)`,
		`CREATE INDEX supervisor_reminders_scope ON supervisor_reminders (scope, state)`,
	)},
	// Up to where a wake that went past a gate's hold gave a thread, while
	// the waker's position is before it: the wake left what the gate held
	// (#681). Such a thread's events up to there are not given again.
	{Name: "supervisor_given", Up: execAll(
		`CREATE TABLE supervisor_given (
			scope  TEXT NOT NULL,
			thread INTEGER NOT NULL,
			upto   INTEGER NOT NULL,
			PRIMARY KEY (scope, thread)
		)`,
	)},
	// From when on what is written in a link's conversation is input, in
	// unix milliseconds: for a conversation that was there before the link,
	// the time the link was made, since what was written in it before was
	// not for hataori (#571). 0 for one opened for its thread. Of the links
	// made before this step, a thread's first one was opened for it.
	{Name: "links.since", Up: execAll(
		`ALTER TABLE links ADD COLUMN since INTEGER NOT NULL DEFAULT 0`,
		`UPDATE links SET since = created_at
			WHERE id NOT IN (SELECT min(id) FROM links GROUP BY thread_id)`,
	)},
	// The thread's earlier conversation that a session's agent was not told
	// yet, since its conversation was started with no input to tell it with;
	// '' when it was, or there is none (#579). A pending input that is given
	// to the agent after it carries a copy: the session's is cleared with the
	// report of that input.
	{Name: "sessions.context", Up: execAll(
		`ALTER TABLE sessions ADD COLUMN context TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pending_inputs ADD COLUMN context TEXT NOT NULL DEFAULT ''`,
	)},
	// The configuration directory whose login is out, '' for the one of a
	// session with no directory of its own (agent.LoginDir): an outage is
	// one of an agent and a directory (#568).
	{Name: "login_outages.config_dir", Up: execAll(
		`ALTER TABLE login_outages ADD COLUMN config_dir TEXT NOT NULL DEFAULT ''`,
		`DROP INDEX login_outages_open`,
		`CREATE UNIQUE INDEX login_outages_open ON login_outages (agent, config_dir) WHERE closed_at IS NULL`,
	)},
	// What a thread is to its scope (control.ThreadKind), set at its start
	// and never changed. A thread started confined before was its scope's
	// supervisor's own: nothing else was (#867).
	{Name: "threads.kind", Up: execAll(
		`ALTER TABLE threads ADD COLUMN kind TEXT NOT NULL DEFAULT 'ordinary'`,
		`UPDATE threads SET kind = 'supervisor' WHERE confined = 1`,
	)},
	// Input of a conversation, accepted, that waits until fewer of its
	// scope's conversations run a turn (#875): a core.FirstInput. wake is
	// that nobody wrote it, told that the conversation was told it waits,
	// pos the newest event's position when it came.
	{Name: "waiting_inputs", Up: execAll(
		`CREATE TABLE waiting_inputs (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			thread_id INTEGER NOT NULL REFERENCES threads(id),
			input     TEXT NOT NULL,
			wake      INTEGER NOT NULL,
			told      INTEGER NOT NULL DEFAULT 0,
			pos       INTEGER NOT NULL
		)`,
		`CREATE INDEX waiting_inputs_thread ON waiting_inputs (thread_id, id)`,
	)},
	// Whether the link's client opened its conversation: 1, or 0 for one
	// that was there before the link (#870). Of the links made before this
	// step, a thread's first one counts as opened and no other does: who
	// opened a conversation was not recorded, and the first is the one that
	// a thread is started with. (Not by since: it is set anew for a link
	// that was not served for a time, and in the links older than it says
	// only which one is the first.) An archive not sent yet is dropped,
	// unless it is of a conversation that a link counts as opened.
	{Name: "links.opened", Up: execAll(
		`ALTER TABLE links ADD COLUMN opened INTEGER NOT NULL DEFAULT 0`,
		`UPDATE links SET opened = 1 WHERE id IN (SELECT min(id) FROM links GROUP BY thread_id)`,
		`UPDATE chat_outbox SET state = 'dropped' WHERE state = 'pending'
			AND CASE WHEN json_valid(message) THEN json_extract(message, '$.op') END = 'archive'
			AND NOT EXISTS (SELECT 1 FROM links l WHERE l.client = chat_outbox.client AND l.channel = chat_outbox.channel
				AND l.thread = chat_outbox.thread AND l.opened = 1)`,
	)},
	// The writes that supervisors' sessions make through the daemon (#868):
	// each is recorded before anything of it is on the disk, and found out
	// about afterwards, also by the next daemon (done). One of the user's
	// files (name) stays until the lines that name it were posted: told is
	// the position of the event that ended its turn, pos the newest event's
	// position when it was made.
	{Name: "supervisor_writes", Up: execAll(
		`CREATE TABLE supervisor_writes (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			scope   TEXT NOT NULL,
			thread  INTEGER NOT NULL,
			dir     TEXT NOT NULL,
			path    TEXT NOT NULL,             -- the file's, in dir
			tmp     TEXT NOT NULL,             -- the daemon's own name for the new content, beside the file
			was     TEXT NOT NULL,             -- the version it replaces; '' for a new file
			version TEXT NOT NULL,             -- that of the new content
			there   INTEGER NOT NULL,          -- how many of the directories on its way were there before, or are another's
			failed  INTEGER NOT NULL DEFAULT 0, -- its session was told that it was not made
			pos     INTEGER NOT NULL,
			done    INTEGER NOT NULL DEFAULT 0,
			name    TEXT NOT NULL DEFAULT '',  -- the user's file that it is
			told    INTEGER NOT NULL DEFAULT 0
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
