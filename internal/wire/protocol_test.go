package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Major versions must be equal; minor versions may differ.
func TestVersions(t *testing.T) {
	root := t.TempDir()
	_, owner := newCore(t, root)
	srv := serve(t, root, owner)
	c, err := dialPath(srv.Path(), "2.9")
	if err != nil {
		t.Fatalf("a newer minor version is refused: %v", err)
	}
	if _, err := c.Threads(context.Background(), control.Filter{}); err != nil {
		t.Fatalf("a call with a newer minor version: %v", err)
	}
	c.Close()
	for _, v := range []string{"3.0", "1.10"} {
		_, err := dialPath(srv.Path(), v)
		if err == nil || !strings.Contains(err.Error(), "client speaks "+v) || !strings.Contains(err.Error(), "daemon speaks "+Version) {
			t.Errorf("client %s: %v", v, err)
		}
	}
	// And the client refuses a daemon of another major version.
	path := fakeDaemon(t, "3.3", nil)
	if _, err := dialPath(path, Version); err == nil || !strings.Contains(err.Error(), "3.3") || !strings.Contains(err.Error(), Version) {
		t.Errorf("daemon 3.3: %v", err)
	}
}

// fakeDaemon listens on a socket of its own, says hello with version, and
// answers each line with the function's lines (none when nil).
func fakeDaemon(t *testing.T, version string, answer func(req message) []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				r.ReadBytes('\n')
				b, _ := json.Marshal(hello{Version: version})
				c.Write(append(b, '\n'))
				for {
					l, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var m message
					json.Unmarshal(l, &m)
					if answer != nil {
						for _, a := range answer(m) {
							c.Write([]byte(a + "\n"))
						}
					}
				}
			}()
		}
	}()
	return path
}

// A field or an event kind the client does not know is ignored.
func TestUnknownFieldsAndKinds(t *testing.T) {
	path := fakeDaemon(t, "2.4", func(m message) []string {
		switch m.Op {
		case "Threads":
			return []string{`{"id":1,"result":[{"id":{"scope":"s","key":"k","shard":7},"number":1,"state":"idle","colour":"red"}],"later":{"x":1}}`}
		case "Events":
			return []string{
				`{"id":1}`,
				`{"id":1,"event":{"pos":1,"thread":{"scope":"s","key":"k"},"time":"2026-01-02T03:04:05Z","kind":"from_the_future","data":{"x":[1]},"extra":true}}`,
				`{"id":1,"event":{"pos":2,"thread":{"scope":"s","key":"k"},"time":"2026-01-02T03:04:05Z","kind":"usage","data":{"session":1}}}`,
			}
		}
		return nil
	})
	c, err := dialPath(path, Version)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ts, err := c.Threads(context.Background(), control.Filter{})
	if err != nil || len(ts) != 1 || ts[0].ID.Key != "k" || ts[0].State != control.Idle {
		t.Fatalf("threads: %+v, %v", ts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, err := c.Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for len(kinds) < 1 {
		select {
		case e := <-evs:
			kinds = append(kinds, string(e.Kind))
		case <-time.After(5 * time.Second):
			t.Fatalf("events: %v", kinds)
		}
	}
	if kinds[0] != "usage" { // the unknown kind before it is skipped
		t.Fatalf("events: %v", kinds)
	}
}

// The daemon sends a connected client a request and waits for its answer.
func TestDaemonAsksTheClient(t *testing.T) {
	root := t.TempDir()
	_, owner := newCore(t, root)
	srv := serve(t, root, owner)
	c, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var peers []*Peer
	for deadline := time.Now().Add(5 * time.Second); len(peers) == 0; time.Sleep(5 * time.Millisecond) {
		if peers = srv.Peers(); time.Now().After(deadline) {
			t.Fatal("no peer")
		}
	}
	ctx := context.Background()
	if err := peers[0].Request(ctx, "NewThread", map[string]string{"scope": "s"}, nil); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("a client without a handler: %v", err)
	}
	c.SetHandler(func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		if op != "NewThread" {
			return nil, ErrNotSupported
		}
		// The handler may call the daemon on the same connection.
		if _, err := c.Threads(ctx, control.Filter{}); err != nil {
			return nil, err
		}
		return map[string]string{"key": "t-new"}, nil
	})
	var got struct{ Key string }
	if err := peers[0].Request(ctx, "NewThread", map[string]string{"scope": "s"}, &got); err != nil || got.Key != "t-new" {
		t.Fatalf("answer %+v, %v", got, err)
	}
	if err := peers[0].Request(ctx, "Unheard", nil, nil); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("an operation the client does not know: %v", err)
	}
	// And the other way: an operation the daemon does not know.
	if err := c.call(ctx, "Unheard", nil, nil); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("an operation the daemon does not know: %v", err)
	}
}
