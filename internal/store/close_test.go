package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// slowDriver's Open waits until release is closed, as a connection that
// database/sql opens on its own goroutine may still be opening.
type slowDriver struct {
	inside  chan struct{} // closed once Open has started
	release chan struct{}
	opens   atomic.Int32
	done    atomic.Bool // the Open that was released has returned
}

func (d *slowDriver) Open(string) (driver.Conn, error) {
	if d.opens.Add(1) == 1 {
		close(d.inside)
		<-d.release
		d.done.Store(true)
	}
	return nil, errors.New("slow driver: no connection")
}

// Closing waits for a connection that is being opened, and no connection
// is opened after it.
func TestCloseWaitsForAConnectionBeingOpened(t *testing.T) {
	d := &slowDriver{inside: make(chan struct{}), release: make(chan struct{})}
	c := &connector{drv: d}
	go c.Connect(context.Background())
	<-d.inside
	closed := make(chan bool)
	go func() { c.close(); closed <- d.done.Load() }()
	select {
	case <-closed:
		t.Fatal("close returned while a connection was still being opened")
	case <-time.After(200 * time.Millisecond): // the open is held this long
	}
	close(d.release)
	if openDone := <-closed; !openDone {
		t.Fatal("close returned before the open did")
	}
	if _, err := c.Connect(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("Connect after close: %v, want errClosed", err)
	}
	if n := d.opens.Load(); n != 1 {
		t.Fatalf("the driver opened %d times, want 1", n)
	}
}

// A closed store's connector opens no connection, so nothing creates the
// database file again.
func TestClosedStoreOpensNothing(t *testing.T) {
	root := newRoot(t)
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.conn.Connect(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("Connect after Close: %v, want errClosed", err)
	}
}
