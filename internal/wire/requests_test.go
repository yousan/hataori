package wire

import (
	"context"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// A request the daemon sent on a connection before it read the
// connection's Events request, such as a plugin's plugin_started, is
// skipped: the stream starts and delivers its events.
func TestStreamSkipsRequests(t *testing.T) {
	path := fakeDaemon(t, Version, func(m message) []string {
		if m.Op != "Events" {
			return nil
		}
		return []string{
			`{"id":1,"op":"plugin_started","args":{"plugin":"p","scopes":["s"]}}`,
			`{"id":1}`,
			`{"id":2,"op":"hook","args":{"point":"before_agent_run","scope":"s","left":15000}}`,
			`{"id":1,"event":{"pos":5,"thread":1,"scope":"s","time":"2026-01-02T03:04:05Z","kind":"usage","data":{"session":1}}}`,
		}
	})
	c, err := dialPath(path, Version)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, err := c.Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-evs:
		if e.Pos != 5 {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
}
