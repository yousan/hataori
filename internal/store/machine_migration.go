package store

// This migration changes only SQLite. In particular, opaque session specs,
// recorded workspace paths, host runs and the files they name stay untouched.
var machineMigration = Migration{Name: "machine identity and placement foundation", ForeignKeysOff: true, Up: execAll(
	`CREATE TABLE machines (
		key TEXT PRIMARY KEY,
		ssh TEXT NOT NULL DEFAULT '',
		root TEXT NOT NULL DEFAULT '',
		installation TEXT NOT NULL DEFAULT '',
		configured INTEGER NOT NULL DEFAULT 0 CHECK (configured IN (0,1)),
		CHECK (key != 'local' OR (ssh = '' AND root = '' AND installation = ''))
	)`,
	`INSERT INTO machines (key, configured) VALUES ('local', 1)`,
	// NULL is reserved for an ordinary thread awaiting its first placement.
	// Historical rows and current local starts receive local by default.
	`ALTER TABLE threads ADD COLUMN machine TEXT DEFAULT 'local' REFERENCES machines(key)`,
	`CREATE INDEX threads_machine ON threads(machine)`,
	`CREATE TRIGGER threads_machine_confined_insert BEFORE INSERT ON threads
		WHEN (NEW.confined != 0 OR NEW.kind != 'ordinary') AND NEW.machine IS NOT 'local'
		BEGIN SELECT RAISE(ABORT, 'confined threads must stay local'); END`,
	`CREATE TRIGGER threads_machine_confined_update BEFORE UPDATE ON threads
		WHEN (NEW.confined != 0 OR NEW.kind != 'ordinary') AND NEW.machine IS NOT 'local'
		BEGIN SELECT RAISE(ABORT, 'confined threads must stay local'); END`,
	`CREATE TRIGGER threads_machine_fixed BEFORE UPDATE OF machine ON threads
		WHEN OLD.machine IS NOT NULL AND NEW.machine IS NOT OLD.machine
		BEGIN SELECT RAISE(ABORT, 'a placed thread cannot change machine'); END`,
	`CREATE TABLE pending_placements (
		thread_id INTEGER PRIMARY KEY REFERENCES threads(id),
		machine TEXT REFERENCES machines(key), -- NULL: automatic choice
		agent TEXT NOT NULL,
		data TEXT NOT NULL, -- accepted start, opaque to the store
		created_at INTEGER NOT NULL,
		reason TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE machine_reservations (
		thread_id INTEGER PRIMARY KEY REFERENCES threads(id),
		machine TEXT NOT NULL REFERENCES machines(key),
		agent TEXT NOT NULL,
		operation TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TRIGGER machine_reservations_match BEFORE INSERT ON machine_reservations
		WHEN NEW.machine IS NOT (SELECT machine FROM threads WHERE id = NEW.thread_id)
		BEGIN SELECT RAISE(ABORT, 'reservation must match thread machine'); END`,
	`CREATE TABLE machine_health (
		machine TEXT PRIMARY KEY REFERENCES machines(key),
		observed_at INTEGER NOT NULL,
		boot TEXT NOT NULL,
		reconciled INTEGER NOT NULL DEFAULT 0,
		load REAL, -- NULL: unknown, never interpreted as zero
		free_memory_bytes INTEGER,
		free_disk_bytes INTEGER
	)`,
	`CREATE TABLE machine_agent_health (
		machine TEXT NOT NULL REFERENCES machines(key),
		agent TEXT NOT NULL,
		config_dir TEXT NOT NULL,
		observed_at INTEGER NOT NULL,
		state TEXT NOT NULL,
		PRIMARY KEY (machine, agent, config_dir)
	)`,
	`ALTER TABLE login_outages ADD COLUMN machine TEXT NOT NULL DEFAULT 'local' REFERENCES machines(key)`,
	`DROP INDEX login_outages_open`,
	`CREATE UNIQUE INDEX login_outages_open ON login_outages (machine, agent, config_dir) WHERE closed_at IS NULL`,
	`CREATE TABLE agent_limits_new (
		machine TEXT NOT NULL DEFAULT 'local' REFERENCES machines(key),
		agent TEXT NOT NULL,
		config_dir TEXT NOT NULL DEFAULT '',
		at INTEGER NOT NULL,
		every INTEGER NOT NULL,
		windows TEXT NOT NULL,
		PRIMARY KEY (machine, agent, config_dir)
	)`,
	// Older reports do not say which directory supplied them. Keep that
	// distinction as '' rather than inventing an account identity.
	`INSERT INTO agent_limits_new (agent, at, every, windows) SELECT agent, at, every, windows FROM agent_limits`,
	`DROP TABLE agent_limits`,
	`ALTER TABLE agent_limits_new RENAME TO agent_limits`,
)}
