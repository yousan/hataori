package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// An attachment that closes keeps the connection until the host has read
// what was written to it, also while the host writes output before it
// reads: otherwise that write fails, and the host drops the connection with
// the keys it had not read (#691).
func TestAttachmentClosesWhenTheHostHasRead(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closing := make(chan struct{})
	type result struct {
		keys string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		var r result
		defer func() { got <- r }()
		c, err := ln.Accept()
		if err != nil {
			r.err = err
			return
		}
		defer c.Close()
		dec, enc := json.NewDecoder(c), json.NewEncoder(c)
		var in request
		if r.err = dec.Decode(&in); r.err != nil {
			return
		}
		if r.err = enc.Encode(response{OK: true, Cols: 80, Rows: 24}); r.err != nil {
			return
		}
		<-closing
		time.Sleep(100 * time.Millisecond) // the close has begun
		for range 100 {                    // more than one read of the other side takes
			if r.err = enc.Encode(response{OK: true, Data: bytes.Repeat([]byte("x"), 4096)}); r.err != nil {
				return
			}
		}
		for {
			in = request{}
			if dec.Decode(&in) != nil {
				return
			}
			r.keys += string(in.Data)
		}
	}()
	a, err := Attach(sock)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reply ", "quick", "\r"} {
		if err := a.Input([]byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	close(closing)
	a.Close()
	if r := <-got; r.err != nil || r.keys != "reply quick\r" {
		t.Fatalf("the host read %q before the end, %v", r.keys, r.err)
	}
}

// A host that reads no more, as when its command takes no input, holds a
// closing attachment for closeWait, no longer.
func TestAttachmentCloseIsBounded(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		json.NewEncoder(c).Encode(response{OK: true, Cols: 80, Rows: 24})
		<-t.Context().Done()
		c.Close()
	}()
	a, err := Attach(sock)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Now()
	a.Close()
	if d := time.Since(from); d < closeWait || d > closeWait+3*time.Second {
		t.Fatalf("closed after %v, want %v", d, closeWait)
	}
}

// Keys written to an attachment that is closed right after reach the
// command, each time, while its answers are on their way (#691).
func TestAttachedKeysBeforeTheCloseArrive(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("echo"), Env: []string{}, Terminal: true})
	for i := range 50 {
		a, err := Attach(SocketPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				if _, err := a.Next(); err != nil {
					return
				}
			}
		}()
		line := fmt.Sprintf("line %d.", i)
		for _, k := range []string{"\x7f", "\x7f", line, "\r"} {
			if err := a.Input([]byte(k)); err != nil {
				t.Fatal(err)
			}
		}
		a.Close()
		waitFor(t, line, func() bool { return bytes.Contains(read(t, dir, TerminalLog), []byte("got "+line)) })
	}
}
