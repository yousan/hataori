package store

var sessionIOMigration = Migration{Name: "machine session I/O and replay proofs", Up: execAll(
	`ALTER TABLE sessions ADD COLUMN io_error TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sessions ADD COLUMN screen_offset INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE session_io_sources (
		session_id INTEGER NOT NULL REFERENCES sessions(id),
		path TEXT NOT NULL,
		handle TEXT NOT NULL,
		generation TEXT NOT NULL,
		PRIMARY KEY(session_id,path)
	)`,
	`CREATE TABLE session_read_proofs (
		session_id INTEGER NOT NULL,
		n INTEGER NOT NULL,
		digest TEXT NOT NULL,
		handle TEXT NOT NULL,
		generation TEXT NOT NULL,
		PRIMARY KEY(session_id,n),
		FOREIGN KEY(session_id,n) REFERENCES session_reads(session_id,n) ON DELETE CASCADE
	)`,
	`CREATE TABLE session_machine_runs (
		session_id INTEGER PRIMARY KEY REFERENCES sessions(id),
		machine TEXT NOT NULL REFERENCES machines(key),
		run TEXT NOT NULL,
		workspace TEXT NOT NULL,
		agent_config TEXT NOT NULL,
		phase TEXT NOT NULL
		,previous_run TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE machine_main_identity (id INTEGER PRIMARY KEY CHECK(id=1), identity TEXT NOT NULL)`,
	`INSERT INTO machine_main_identity VALUES(1,lower(hex(randomblob(16))))`,
	`CREATE TABLE machine_run_history (
		machine TEXT NOT NULL REFERENCES machines(key),
		thread_id INTEGER NOT NULL REFERENCES threads(id),
		session_id INTEGER NOT NULL REFERENCES sessions(id),
		run TEXT NOT NULL,
		PRIMARY KEY(machine,session_id,run)
	)`,
	`CREATE TABLE session_machine_actions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id INTEGER NOT NULL REFERENCES sessions(id),
		run TEXT NOT NULL,
		operation TEXT NOT NULL,
		key TEXT NOT NULL,
		request TEXT NOT NULL,
		inputs TEXT NOT NULL DEFAULT '[]',
		state TEXT NOT NULL,
		UNIQUE(session_id,run,operation,key)
	)`,
)}
