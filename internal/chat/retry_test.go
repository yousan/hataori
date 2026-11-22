package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface/fake"
)

// A driver whose Run fails at the start, as when the network is not up
// yet, is run again until it connects; a trigger posted meanwhile starts
// its thread then. While it is down, Clients says so. The failures log one
// line, and the connection one more (#598).
func TestRunTriesAgainUntilTheDriverConnects(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.RetryFirst, fe.RetryMax = 50*time.Millisecond, 100*time.Millisecond
	})
	f.driver.Fail(fake.OpRun, 3, errors.New("network is unreachable"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.fe.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	clients := func() []control.ClientState {
		t.Helper()
		cs, err := f.core.As(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpRead}).Clients(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	waitFor(t, "the first failure", func() bool { return f.driver.Runs() >= 1 })
	trigger := f.driver.Inject(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git while it could not connect")
	if f.driver.Runs() > 3 {
		t.Fatal("the driver connected before the trigger was posted")
	}
	if cs := clients(); len(cs) != 1 || cs[0].Client != "chat" || cs[0].Connected || cs[0].Since.IsZero() {
		t.Fatalf("while it cannot connect, Clients says %+v", cs)
	}

	waitFor(t, "the trigger's thread", func() bool { return len(f.runner.all()) == 1 })
	if l := f.runner.all(); l[0].Key != trigger.Ref.ID || l[0].Input.Text != "while it could not connect" {
		t.Fatalf("started %+v", l)
	}
	if n := f.driver.Runs(); n != 4 {
		t.Fatalf("the driver ran %d times, want 3 failures and 1 that connected", n)
	}
	if cs := clients(); len(cs) != 1 || !cs[0].Connected {
		t.Fatalf("once connected, Clients says %+v", cs)
	}
	failed, connected := f.count("the driver stopped; trying again"), f.count("chat front end connected")
	if failed != 1 || connected != 1 || !strings.Contains(f.line("chat front end connected"), "failures=3") {
		t.Fatalf("logged %d failure lines and %d connection lines, want 1 and 1:\n%s", failed, connected, strings.Join(f.log.lines(), "\n"))
	}

	// A connection that drops later, and that the driver cannot make
	// again, is made again the same way. Within the minute of the last
	// failure line, a connection that keeps dropping logs nothing more.
	for runs := 5; runs <= 7; runs++ {
		f.driver.Drop(errors.New("the gateway closed and may not reconnect"))
		waitFor(t, "the driver to run again", func() bool { return f.driver.Runs() == runs && f.driver.Connected() })
		waitFor(t, "Clients to say connected again", func() bool { cs := clients(); return len(cs) == 1 && cs[0].Connected })
	}
	if failed, connected := f.count("the driver stopped; trying again"), f.count("chat front end connected"); failed != 1 || connected != 1 {
		t.Fatalf("after three drops, logged %d failure lines and %d connection lines, want 1 and 1:\n%s",
			failed, connected, strings.Join(f.log.lines(), "\n"))
	}
}

// A drop that the driver makes good by itself shows in Clients until it
// is connected again; the catch-up's own retry does not count as a
// connection (#598).
func TestClientsWhileTheDriverReconnects(t *testing.T) {
	f := newFront(t, workspaces{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.fe.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the connection", func() bool { up, _ := f.fe.State(); return up })
	f.driver.Disconnect()
	if up, since := f.fe.State(); up || since.IsZero() {
		t.Fatalf("while the driver reconnects, State says %v since %v", up, since)
	}
	f.fe.catchUpAll() // as the catch-up's retry does
	if up, _ := f.fe.State(); up {
		t.Fatal("the catch-up's retry counted as a connection")
	}
	f.driver.Reconnect()
	if up, _ := f.fe.State(); !up {
		t.Fatal("connected again, State says not connected")
	}
}

// count counts the log lines that contain s.
func (f *front) count(s string) int {
	n := 0
	for _, l := range f.log.lines() {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}

// line returns the first log line that contains s.
func (f *front) line(s string) string {
	for _, l := range f.log.lines() {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}
