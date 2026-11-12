//go:build zombiezen

package main

import (
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// zombiezen.com/go/sqlite has its own API, not database/sql.
type zStore struct{ c *sqlite.Conn }

func init() {
	driverName = "zombiezen.com/go/sqlite"
	open = func(path string, busyMS int, sync string) (store, error) {
		// OpenWAL is this library's flag: it runs "PRAGMA journal_mode=wal" on open.
		c, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenWAL)
		if err != nil {
			return nil, err
		}
		c.SetBusyTimeout(time.Duration(busyMS) * time.Millisecond)
		if err := sqlitex.ExecuteTransient(c, "PRAGMA synchronous="+sync, nil); err != nil {
			c.Close()
			return nil, err
		}
		return &zStore{c: c}, nil
	}
}

func (s *zStore) pragma(name string) (v string, err error) {
	err = sqlitex.ExecuteTransient(s.c, "PRAGMA "+name, &sqlitex.ExecOptions{
		ResultFunc: func(st *sqlite.Stmt) error { v = st.ColumnText(0); return nil },
	})
	return v, err
}

func (s *zStore) createTable() error {
	return sqlitex.ExecuteTransient(s.c, "CREATE TABLE events (seq INTEGER PRIMARY KEY, body TEXT NOT NULL)", nil)
}

func (s *zStore) insert(i int, body string) (err error) {
	end, err := sqlitex.ImmediateTransaction(s.c)
	if err != nil {
		return err
	}
	defer end(&err)
	return sqlitex.Execute(s.c, "INSERT INTO events (seq, body) VALUES (?, ?)", &sqlitex.ExecOptions{
		Args: []any{i, body},
	})
}

func (s *zStore) readAll() (n int, err error) {
	err = sqlitex.Execute(s.c, "SELECT seq, body FROM events ORDER BY seq", &sqlitex.ExecOptions{
		ResultFunc: func(st *sqlite.Stmt) error {
			_ = st.ColumnInt(0)
			_ = st.ColumnText(1)
			n++
			return nil
		},
	})
	return n, err
}

func (s *zStore) close() error { return s.c.Close() }
