package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/wire"
)

// serveWire serves the owner's API on the instance's control socket and
// returns the server and a client of it.
func (f *fixture) serveWire() (*wire.Server, *wire.Client) {
	f.t.Helper()
	srv, err := wire.Listen(f.root, owner(f.core), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	f.t.Cleanup(cancel)
	c, err := wire.Dial(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.Close() })
	return srv, c
}

// next returns the next chunk, or fails after a while.
func next(t *testing.T, ch <-chan control.TerminalChunk) control.TerminalChunk {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("the terminal stream ended")
		}
		return c
	case <-time.After(15 * time.Second):
		t.Fatal("no terminal output")
	}
	return control.TerminalChunk{}
}

// readOn reads continuing chunks until what was read ends with suffix.
func readOn(t *testing.T, ch <-chan control.TerminalChunk, run string, pos uint64, suffix string) ([]byte, uint64) {
	t.Helper()
	var got []byte
	for !bytes.HasSuffix(got, []byte(suffix)) {
		c := next(t, ch)
		if c.Reset || c.Run != run || c.Pos != pos+uint64(len(c.Data)) {
			t.Fatalf("chunk of run %q at %d with %d bytes (reset %v) does not continue %d of run %q", c.Run, c.Pos, len(c.Data), c.Reset, pos, run)
		}
		got, pos = append(got, c.Data...), c.Pos
	}
	return got, pos
}

func terminalLog(f *fixture) []byte {
	b, _ := os.ReadFile(filepath.Join(StateDir(f.root, 1), host.TerminalLog))
	return b
}

// A client streams a fake agent's terminal over the control socket, and
// after the connection breaks it goes on from its last position: every
// byte once, in order.
func TestTerminalStreamOverTheWire(t *testing.T) {
	f := newFixture(t, options{terminal: true})
	srv, c := f.serveWire()
	f.start("screen one\nscreen two\nreply ok")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	waitFor(t, "the output", 15*time.Second, func() bool { return bytes.Contains(terminalLog(f), []byte("two\r\n")) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Terminal(ctx, thread, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	first := next(t, ch)
	if !first.Reset || first.Run == "" || first.Cols != 80 || first.Rows != 24 || first.Pos != uint64(len(first.Data)) {
		t.Fatalf("first chunk: reset %v, run %q, %dx%d, %d bytes at %d", first.Reset, first.Run, first.Cols, first.Rows, len(first.Data), first.Pos)
	}
	if !bytes.Contains(first.Data, []byte("one\r\ntwo\r\n")) {
		t.Fatalf("first chunk %q", first.Data)
	}

	srv.DropConns() // as if the daemon restarted: the client connects again
	send(t, owner(f.core), thread, "m2", "screen three\nscreen four\nreply ok")
	rest, _ := readOn(t, ch, first.Run, first.Pos, "four\r\n")
	if got, log := append(first.Data, rest...), terminalLog(f); !bytes.Equal(got, log) {
		t.Fatalf("streamed %q\nthe terminal log holds %q", got, log)
	}
}

// An asleep session sends the end of its last run's log, marked reset,
// then nothing until it runs again; then the new run.
func TestTerminalOfAnAsleepSession(t *testing.T) {
	f := newFixture(t, options{terminal: true, idlePeriod: 300 * time.Millisecond})
	f.start("screen before\nreply hi")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the host's end", 15*time.Second, func() bool { return host.Gone(StateDir(f.root, 1)) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := owner(f.core).Terminal(ctx, thread, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	first := next(t, ch)
	if !first.Reset || first.Run != "" || first.Pos != 0 || first.Cols != 80 || first.Rows != 24 ||
		!bytes.Equal(first.Data, terminalLog(f)) || !bytes.Contains(first.Data, []byte("before\r\n")) {
		t.Fatalf("first chunk: reset %v, run %q, pos %d, %dx%d, %q", first.Reset, first.Run, first.Pos, first.Cols, first.Rows, first.Data)
	}
	select {
	case c := <-ch:
		t.Fatalf("an asleep session sent more: %+v", c)
	case <-time.After(time.Second):
	}

	send(t, owner(f.core), thread, "m2", "screen after\nreply again")
	woke := next(t, ch)
	if !woke.Reset || woke.Run == "" {
		t.Fatalf("after the wake: reset %v, run %q", woke.Reset, woke.Run)
	}
	if !bytes.Contains(woke.Data, []byte("after\r\n")) {
		readOn(t, ch, woke.Run, woke.Pos, "after\r\n")
	}
}

// A session on pipes has no terminal: not_found, not an empty stream.
func TestTerminalOnPipes(t *testing.T) {
	f := newFixture(t, options{})
	_, c := f.serveWire()
	f.start("reply hi")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	for name, api := range map[string]control.API{"in process": owner(f.core), "over the socket": c} {
		if _, err := api.Terminal(context.Background(), thread, "", 0); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// What a client sends on a Terminal stream reaches nothing: a Send written
// on it is neither answered nor carried out.
func TestTerminalStreamTakesNoInput(t *testing.T) {
	f := newFixture(t, options{terminal: true})
	f.serveWire()
	f.start("reply ok")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	inputs := counts(t, f.st)["input"]

	path, err := wire.SocketPath(f.root)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	enc.Encode(map[string]any{"version": wire.Version})
	var hello map[string]any
	if err := dec.Decode(&hello); err != nil || hello["error"] != nil {
		t.Fatalf("hello: %v %v", hello, err)
	}
	enc.Encode(map[string]any{"id": 1, "op": "Terminal", "args": map[string]any{"id": thread, "run": "", "after": 0}})
	enc.Encode(map[string]any{"id": 2, "op": "Send", "args": map[string]any{"id": thread, "key": "m9",
		"input": map[string]any{"text": "screen injected\nreply injected"}, "busy": 0}})
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var resp map[string]any
		if err := dec.Decode(&resp); err != nil {
			break // the deadline: nothing more came
		}
		if resp["id"] != float64(1) {
			t.Fatalf("an answer to a request sent on the stream: %v", resp)
		}
	}
	if counts(t, f.st)["input"] != inputs || bytes.Contains(terminalLog(f), []byte("injected")) {
		t.Fatal("a Send written on the terminal stream was carried out")
	}
}

// A stream that follows a session while it goes to sleep goes quiet: no
// empty chunk and no reset until the session runs again.
func TestTerminalStreamThroughSleep(t *testing.T) {
	f := newFixture(t, options{terminal: true, idlePeriod: 500 * time.Millisecond})
	f.start("screen before\nreply hi")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := owner(f.core).Terminal(ctx, thread, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	first := next(t, ch)
	run, pos := first.Run, first.Pos
	if !bytes.Contains(first.Data, []byte("before\r\n")) {
		_, pos = readOn(t, ch, run, pos, "before\r\n")
	}
	asleep := make(chan struct{})
	go func() {
		for sessionState(t, f.st, 1) != "asleep" || !host.Gone(StateDir(f.root, 1)) {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(time.Second)
		close(asleep)
	}()
	for quiet := true; quiet; {
		select {
		case c := <-ch:
			if c.Reset || len(c.Data) == 0 || c.Run != run || c.Pos != pos+uint64(len(c.Data)) {
				t.Fatalf("while the session went to sleep: reset %v, run %q, %d bytes at %d", c.Reset, c.Run, len(c.Data), c.Pos)
			}
			pos = c.Pos
		case <-asleep:
			quiet = false
		case <-time.After(20 * time.Second):
			t.Fatal("the session did not go to sleep")
		}
	}
	send(t, owner(f.core), thread, "m2", "screen after\nreply again")
	woke := next(t, ch)
	if !woke.Reset || woke.Run == "" || woke.Run == run {
		t.Fatalf("after the wake: reset %v, run %q (was %q)", woke.Reset, woke.Run, run)
	}
	if !bytes.Contains(woke.Data, []byte("after\r\n")) {
		readOn(t, ch, woke.Run, woke.Pos, "after\r\n")
	}
}

// When the thread gets a new session, because the agent no longer had the
// conversation, the stream goes on with the new session.
func TestTerminalFollowsANewSession(t *testing.T) {
	conv := t.TempDir()
	f := newFixture(t, options{terminal: true, idlePeriod: 300 * time.Millisecond, conversations: conv})
	f.start("screen first\nreply hello")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the host's end", 15*time.Second, func() bool { return host.Gone(StateDir(f.root, 1)) })
	files, _ := filepath.Glob(filepath.Join(conv, "*"))
	for _, p := range files {
		os.Remove(p) // the agent's own retention removed it
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := owner(f.core).Terminal(ctx, thread, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c := next(t, ch); !c.Reset || !bytes.Contains(c.Data, []byte("first\r\n")) {
		t.Fatalf("asleep: reset %v, %q", c.Reset, c.Data)
	}
	send(t, owner(f.core), thread, "m2", "screen second\nreply anew")
	waitFor(t, "the new session", 15*time.Second, f.has("session_replaced", 1))
	// The old session's agent runs briefly before it says it cannot resume,
	// so more than one run may come, each starting with a reset.
	var screen []byte
	var run string
	var pos uint64
	for !bytes.Contains(screen, []byte("second\r\n")) {
		c := next(t, ch)
		switch {
		case c.Reset:
			screen = append([]byte(nil), c.Data...)
		case c.Run != run || c.Pos != pos+uint64(len(c.Data)):
			t.Fatalf("chunk of run %q at %d with %d bytes does not continue %d of run %q", c.Run, c.Pos, len(c.Data), pos, run)
		default:
			screen = append(screen, c.Data...)
		}
		run, pos = c.Run, c.Pos
	}
	if b, _ := os.ReadFile(filepath.Join(StateDir(f.root, 2), host.TerminalLog)); !bytes.Contains(b, []byte("second\r\n")) {
		t.Fatal("the output did not come from the new session")
	}
}
