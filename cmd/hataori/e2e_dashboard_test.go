package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// A page catches up without gaps or repeats after a cut mid-turn and a
// daemon restart.
func TestDashboardFollowsTurnsAcrossReconnectAndRestart(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": heldPort(t)})
	stopDaemon := runDaemon(t, root)
	base, cookie := dashSignIn(t, root)
	ctx := context.Background()

	ch, closeStream := dashStream(t, base, cookie, 0)
	if m := next(t, ch, 15*time.Second); m.kind != "caught_up" || m.id != 0 {
		t.Fatalf("empty stream: %q", m.raw)
	}
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "tool sleep 3s\nreply one"}})
	c.Close()
	if err != nil {
		t.Fatal(err)
	}

	type message struct{ id, data string }
	var seen []message
	var last uint64
	starts, ends := 0, 0
	readUntil := func(kind control.EventKind) {
		t.Helper()
		for {
			m := next(t, ch, 30*time.Second)
			if m.kind == "caught_up" {
				continue
			}
			if m.kind != "" || m.id == 0 || m.id != m.event.Pos || m.event.Thread != id {
				t.Fatalf("unexpected event: %q", m.raw)
			}
			seen = append(seen, message{m.sid, m.data})
			last = m.id
			switch m.event.Kind {
			case "turn_started":
				starts++
				if starts != ends+1 {
					t.Fatalf("turn started out of order: %d starts, %d ends", starts, ends)
				}
			case "turn_ended":
				ends++
				if starts != ends {
					t.Fatalf("turn ended out of order: %d starts, %d ends", starts, ends)
				}
			}
			if m.event.Kind == kind {
				return
			}
		}
	}
	waitForLogged := func(kind string) {
		t.Helper()
		eventually(t, "an event after the last position", 30*time.Second, func() bool {
			code, out, _ := cli(t, "events", "--root", root)
			if code != 0 {
				return false
			}
			for _, event := range lines(t, out) {
				pos, ok := event["pos"].(float64)
				if ok && uint64(pos) > last && event["kind"] == kind && event["thread"] == float64(id) {
					return true
				}
			}
			return false
		})
	}
	readUntil("turn_started")
	code, body := dashGet(t, base, cookie, "/api/threads")
	var threads []control.ThreadInfo
	if code != http.StatusOK {
		t.Fatalf("threads: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &threads); err != nil {
		t.Fatalf("threads: %v: %s", err, body)
	}
	found := false
	for _, thread := range threads {
		if thread.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("thread %v missing from %s", id, body)
	}

	closeStream()
	waitForLogged("turn_ended")
	ch, closeStream = dashStream(t, base, cookie, last)
	readUntil("turn_ended")
	closeStream()
	stopDaemon()
	runDaemon(t, root)
	c, err = wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, id, "k2", control.Input{Text: "reply two"}, control.Deliver); err != nil {
		c.Close()
		t.Fatal(err)
	}
	c.Close()
	waitForLogged("turn_started")
	ch, closeStream = dashStream(t, base, cookie, last)
	defer closeStream()
	readUntil("turn_ended")
	if starts != 2 || ends != 2 {
		t.Fatalf("got %d starts and %d ends, want two of each", starts, ends)
	}

	fresh, closeFresh := dashStream(t, base, cookie, 0)
	defer closeFresh()
	var want []message
	for {
		m := next(t, fresh, 30*time.Second)
		if m.kind == "caught_up" {
			break
		}
		if m.id <= last {
			want = append(want, message{m.sid, m.data})
		}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("events across reconnects:\n%v\nwant:\n%v", seen, want)
	}
}
