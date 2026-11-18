package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// hold is how long a test waits to see that a call does not return while
// what it must wait for is held.
const hold = 200 * time.Millisecond

// returnsNot fails the test if done delivers within hold.
func returnsNot(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatal(what)
	case <-time.After(hold):
	}
}

type nopConn struct{ driver.Conn }

func (nopConn) Close() error { return nil }

// slowDriver's first Open waits until release is closed, as a connection
// that database/sql opens on a goroutine of its own may still be opening.
type slowDriver struct {
	inside  chan struct{} // closed once the first Open has started
	release chan struct{}
	opens   atomic.Int32
}

func (d *slowDriver) Open(string) (driver.Conn, error) {
	if d.opens.Add(1) == 1 {
		close(d.inside)
		<-d.release
	}
	return nopConn{}, nil
}

// Closing waits for a connection that is being opened and then for it to
// be closed, and opens none after.
func TestCloseWaitsForAConnectionBeingOpened(t *testing.T) {
	d := &slowDriver{inside: make(chan struct{}), release: make(chan struct{})}
	c := &connector{drv: d}
	got := make(chan driver.Conn)
	go func() { k, _ := c.Connect(context.Background()); got <- k }()
	<-d.inside
	closing, closed := make(chan struct{}), make(chan struct{})
	go func() { c.refuse(); close(closing); c.wait(); close(closed) }()
	<-closing
	returnsNot(t, closed, "closing returned while a connection was being opened")
	close(d.release)
	k := <-got
	returnsNot(t, closed, "closing returned while a connection it opened was open")
	k.Close()
	<-closed
	if _, err := c.Connect(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("Connect after close: %v, want errClosed", err)
	}
	if n := d.opens.Load(); n != 1 {
		t.Fatalf("the driver opened %d times, want 1", n)
	}
}

// Close waits for a connection in use: a transaction still open holds it
// until it ends.
func TestCloseWaitsForAConnectionInUse(t *testing.T) {
	s, err := Open(context.Background(), newRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	closing, closed := make(chan struct{}), make(chan struct{})
	go func() { close(closing); s.Close(); close(closed) }()
	<-closing
	returnsNot(t, closed, "Close returned while a transaction held its connection")
	tx.Rollback()
	<-closed
	if _, err := s.conn.Connect(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("Connect after Close: %v, want errClosed", err)
	}
}

type failingClose struct{ driver.Conn }

func (failingClose) Close() error { return errors.New("close failed") }

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return failingClose{}, nil }

// A connection whose close fails counts as closed, and the error comes
// back from closing.
func TestCloseReturnsAFailedConnectionClose(t *testing.T) {
	c := &connector{drv: failingDriver{}}
	k, err := c.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	c.refuse()
	if err := c.wait(); err == nil || err.Error() != "close failed" {
		t.Fatalf("wait: %v, want the close's error", err)
	}
}

// countedConn claims exactly the optional interfaces of the SQLite
// driver's connection, so database/sql uses it as it would the driver's.
// The driver's Exec and Query (driver.Execer, driver.Queryer) are left out:
// database/sql uses ExecContext and QueryContext when a connection has them.
func TestCountedConnHasTheDriversInterfaces(t *testing.T) {
	probe, err := sql.Open("sqlite", "file:"+newRoot(t)+"/x.db")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	raw, err := probe.Driver().Open("file:" + newRoot(t) + "/x.db")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	k := &countedConn{conn: raw, c: &connector{}}
	checks := map[string]func(driver.Conn) bool{
		"ConnBeginTx":        func(c driver.Conn) bool { _, ok := c.(driver.ConnBeginTx); return ok },
		"ConnPrepareContext": func(c driver.Conn) bool { _, ok := c.(driver.ConnPrepareContext); return ok },
		"ExecerContext":      func(c driver.Conn) bool { _, ok := c.(driver.ExecerContext); return ok },
		"QueryerContext":     func(c driver.Conn) bool { _, ok := c.(driver.QueryerContext); return ok },
		"Pinger":             func(c driver.Conn) bool { _, ok := c.(driver.Pinger); return ok },
		"SessionResetter":    func(c driver.Conn) bool { _, ok := c.(driver.SessionResetter); return ok },
		"Validator":          func(c driver.Conn) bool { _, ok := c.(driver.Validator); return ok },
		"NamedValueChecker":  func(c driver.Conn) bool { _, ok := c.(driver.NamedValueChecker); return ok },
	}
	for name, has := range checks {
		if has(raw) != has(k) {
			t.Errorf("%s: the driver's connection %t, countedConn %t", name, has(raw), has(k))
		}
	}
}
