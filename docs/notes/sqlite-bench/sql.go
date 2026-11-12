//go:build modernc || ncruces || mattn

package main

import "database/sql"

// sqlStore drives any database/sql SQLite driver.
type sqlStore struct{ db *sql.DB }

func openSQL(name, dsn string) (store, error) {
	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, err
	}
	// hataori has one writer; one connection also makes the read-back pragmas
	// come from the same connection that the DSN configured.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &sqlStore{db: db}, nil
}

func (s *sqlStore) pragma(name string) (v string, err error) {
	err = s.db.QueryRow("PRAGMA " + name).Scan(&v)
	return v, err
}

func (s *sqlStore) createTable() error {
	_, err := s.db.Exec("CREATE TABLE events (seq INTEGER PRIMARY KEY, body TEXT NOT NULL)")
	return err
}

func (s *sqlStore) insert(i int, body string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO events (seq, body) VALUES (?, ?)", i, body); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *sqlStore) readAll() (int, error) {
	rows, err := s.db.Query("SELECT seq, body FROM events ORDER BY seq")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var seq int
		var body string
		if err := rows.Scan(&seq, &body); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func (s *sqlStore) close() error { return s.db.Close() }
