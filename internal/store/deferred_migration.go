package store

var deferredMigration = Migration{Name: "durable deferred management input", Up: execAll(
	`CREATE TABLE deferred_sends (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL,
		issuer INTEGER NOT NULL REFERENCES threads(id),
		key TEXT NOT NULL,
		target INTEGER NOT NULL REFERENCES threads(id),
		session INTEGER NOT NULL REFERENCES sessions(id),
		state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		data TEXT NOT NULL,
		proposal INTEGER NOT NULL DEFAULT 0,
		input_ref TEXT NOT NULL DEFAULT '',
		handoff TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE(issuer,key)
	)`,
	`CREATE UNIQUE INDEX deferred_target_pending ON deferred_sends(target)
		WHERE state IN ('proposed','queued','accepted','handoff')`,
	`CREATE INDEX deferred_input ON deferred_sends(session,input_ref)`,
)}
