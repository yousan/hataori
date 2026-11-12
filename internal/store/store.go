// Package store keeps everything hataori must remember after a restart in
// one SQLite database, with a schema version and forward-only migrations.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver, so the binary builds without cgo
)

// File is the database's path relative to the instance root.
const File = "state/hataori.db"

// Store is an open database. It has one connection, so the daemon is its
// only writer and writes never contend for the lock.
type Store struct {
	DB *sql.DB
}

// Migration is one forward-only step of the schema. Each is applied in its
// own transaction together with the version it brings the database to.
type Migration struct {
	Name string
	Up   func(ctx context.Context, tx *sql.Tx) error
}

// Open opens <root>/state/hataori.db and brings it to the latest version.
func Open(ctx context.Context, root string) (*Store, error) {
	return open(ctx, filepath.Join(root, File), migrations)
}

func open(ctx context.Context, path string, steps []Migration) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.migrate(ctx, steps); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// Version returns the schema version of the database; 0 for a new one.
func (s *Store) Version(ctx context.Context) (int, error) {
	return version(ctx, s.DB)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func version(ctx context.Context, q querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&n)
	if err != nil || n == 0 {
		return 0, err
	}
	var v int
	err = q.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// migrate applies the steps after the database's version, one transaction
// each. It writes nothing when the database is newer than steps.
func (s *Store) migrate(ctx context.Context, steps []Migration) error {
	v, err := version(ctx, s.DB)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if v > len(steps) {
		return fmt.Errorf("store: database schema version %d is newer than this binary knows (%d); use a newer hataori", v, len(steps))
	}
	for i := v; i < len(steps); i++ {
		if err := s.apply(ctx, i+1, steps[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, to int, m Migration) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	defer tx.Rollback()
	// Read the version again inside the write transaction.
	v, err := version(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	if v != to-1 {
		return fmt.Errorf("store: migration %d (%s): database is at version %d", to, m.Name, v)
	}
	if err := m.Up(ctx, tx); err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, to); err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", to, m.Name, err)
	}
	return nil
}
