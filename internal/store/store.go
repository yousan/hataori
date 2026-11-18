// Package store keeps everything hataori must remember after a restart in
// one SQLite database, with a schema version and forward-only migrations.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"github.com/yousan/hataori/internal/control"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // pure-Go driver, so the binary builds without cgo
)

// File is the database's path relative to the instance root.
const File = "state/hataori.db"

// Store is an open database. It has one connection, so the daemon is its
// only writer and writes never contend for the lock.
type Store struct {
	DB *sql.DB
	// CheckEvent checks every event before it is appended: control.CheckEvent
	// after Open, so that only events of the documented schema are stored.
	// A test that writes events of its own kinds sets it to nil.
	CheckEvent func(control.Event) error

	mu   sync.Mutex
	wake chan struct{} // closed when events are committed
	conn *connector
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
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	// Escape the path so that '?', '#' or '%' in a root stay part of it.
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	dsn := "file:" + esc + "?" + q.Encode()
	probe, err := sql.Open("sqlite", dsn) // only for its driver: sql.Open connects to nothing
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	c := &connector{drv: probe.Driver(), dsn: dsn}
	probe.Close()
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, wake: make(chan struct{}), CheckEvent: control.CheckEvent, conn: c}
	if err := s.migrate(ctx, steps); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database. It returns once every connection is closed,
// a connection still in use once its holder releases it, and refuses to
// open any later one: after it, nothing touches the database's files. So
// a caller ends its transactions and closes its rows and connections
// first: one it still holds keeps Close waiting. A connection whose close
// failed counts as closed, since nothing can close it again; the error is
// returned.
func (s *Store) Close() error {
	if s.conn == nil {
		return s.DB.Close()
	}
	s.conn.refuse()
	err := s.DB.Close()
	return errors.Join(err, s.conn.wait())
}

// connector opens the database's connections, counts those open, and opens
// none once refuse was called. database/sql may open a connection on a
// goroutine of its own, as when a query cancelled by its context leaves its
// connection unusable while another call waits for one, and it closes a
// connection in use only when it is released; sql.DB.Close waits for
// neither. Without the count, a connection could create the database file
// again after the store was closed and its directory removed.
type connector struct {
	drv driver.Driver
	dsn string

	mu       sync.Mutex
	closed   bool
	opening  int   // Connect calls under way
	open     int   // connections opened and not closed
	closeErr error // what closing the connections failed with
	idle     *sync.Cond
}

var errClosed = errors.New("store: closed")

func (c *connector) cond() *sync.Cond {
	if c.idle == nil {
		c.idle = sync.NewCond(&c.mu)
	}
	return c.idle
}

func (c *connector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	c.opening++
	c.mu.Unlock()
	dc, err := c.drv.Open(c.dsn)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opening--
	if err == nil {
		c.open++
		dc = &countedConn{conn: dc, c: c}
	}
	c.cond().Broadcast()
	return dc, err
}

func (c *connector) Driver() driver.Driver { return c.drv }

// refuse makes every later Connect fail without opening anything.
func (c *connector) refuse() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// wait returns once no connection is being opened and none is open, with
// the errors closing them gave.
func (c *connector) wait() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.opening > 0 || c.open > 0 {
		c.cond().Wait()
	}
	return c.closeErr
}

func (c *connector) closedOne(err error) {
	c.mu.Lock()
	c.open--
	if err != nil {
		c.closeErr = errors.Join(c.closeErr, err)
	}
	c.cond().Broadcast()
	c.mu.Unlock()
}

// countedConn is a connection that tells its connector when it is closed.
// It has the methods of the SQLite driver's connection that database/sql
// uses, and passes each on.
type countedConn struct {
	conn driver.Conn
	c    *connector
	once sync.Once
}

func (k *countedConn) Close() error {
	err := k.conn.Close()
	k.once.Do(func() { k.c.closedOne(err) })
	return err
}

func (k *countedConn) Prepare(q string) (driver.Stmt, error) { return k.conn.Prepare(q) }

func (k *countedConn) Begin() (driver.Tx, error) { return k.conn.Begin() }

func (k *countedConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return k.conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}

func (k *countedConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return k.conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

func (k *countedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return k.conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (k *countedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return k.conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (k *countedConn) Ping(ctx context.Context) error { return k.conn.(driver.Pinger).Ping(ctx) }

func (k *countedConn) ResetSession(ctx context.Context) error {
	return k.conn.(driver.SessionResetter).ResetSession(ctx)
}

func (k *countedConn) IsValid() bool { return k.conn.(driver.Validator).IsValid() }

// The driver's connection has these; countedConn passes them on, so it
// must not claim any the driver's connection lacks.
var (
	_ driver.ConnBeginTx        = (*countedConn)(nil)
	_ driver.ConnPrepareContext = (*countedConn)(nil)
	_ driver.ExecerContext      = (*countedConn)(nil)
	_ driver.QueryerContext     = (*countedConn)(nil)
	_ driver.Pinger             = (*countedConn)(nil)
	_ driver.SessionResetter    = (*countedConn)(nil)
	_ driver.Validator          = (*countedConn)(nil)
)

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
	// Only now, once the database is known to be ours to change.
	if _, err := s.DB.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("store: set WAL mode: %w", err)
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
