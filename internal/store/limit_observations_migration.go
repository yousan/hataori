package store

// Only private provenance is added. Existing windows and their conservative
// timestamps remain readable; the session machine lazily seeds each window.
var limitObservationsMigration = Migration{Name: "per-window limit provenance", Up: execAll(
	`ALTER TABLE sessions ADD COLUMN limit_observations TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agent_limits ADD COLUMN observations TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE agent_limits ADD COLUMN report_at INTEGER NOT NULL DEFAULT 0`,
	`UPDATE agent_limits SET report_at=at`,
)}
