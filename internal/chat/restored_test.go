package chat

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// A second client's last show_in entry is gone: its reader cannot advance
// over the scope's events. Restoring the entry must not replay that output.
func TestRestoredCrossClientLinkDoesNotReplayRevokedOutput(t *testing.T) {
	ws, _, _, files := withDirs(t)
	f := newFront(t, workspaces{}, files)
	id := f.runningThread(t)
	f.core.Scopes["other"] = core.Scope{Enabled: true, Owner: "mirror"}
	own := f.conv(id)
	shown := surface.Conversation{Channel: "shared", Thread: "shown"}
	if _, err := f.fe.Outbox.Store.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, created_at)
		VALUES (?, 'mirror', ?, ?, 0)`, int64(id), shown.Channel, shown.Thread); err != nil {
		t.Fatal(err)
	}
	d := fake.New()
	mirror := func(served bool) *front {
		fe := &FrontEnd{Name: "mirror", Driver: d, API: f.fe.API, People: testPeople(), Log: f.fe.Log,
			Scopes: map[string]Scope{"other": {Enabled: true}},
			Outbox: &Outbox{Store: f.fe.Outbox.Store, Client: "mirror", Driver: d, Log: f.fe.Log, Retry: time.Millisecond}}
		if served {
			fe.ShowIn = map[string][]string{shown.Channel: {channel}}
		}
		files(fe)
		if err := fe.settleLinks(context.Background()); err != nil {
			t.Fatal(err)
		}
		return &front{fe: fe, driver: d}
	}
	m := mirror(true)
	stopOut := run(t, m.fe.Outbox)
	stop := m.showing(t)
	f.event(t, id, "text", map[string]any{"text": "before revocation", "final": true})
	waitFor(t, "the first mirrored answer", func() bool { return posts(d, shown, "before revocation") == 1 })
	stop()
	stopOut()

	m = mirror(false)
	stop = m.showing(t)
	f.event(t, id, "input", map[string]any{"text": "revoked input", "principal": "owner", "via": "web", "turn": 9})
	hidden := f.event(t, id, "text", map[string]any{"text": "revoked answer", "final": true})
	f.event(t, id, "request", map[string]any{"request": "q", "approval": false, "text": "revoked question", "choices": []string{"yes"}})
	f.event(t, id, "file_out", map[string]any{"file": write(t, filepath.Join(ws, "revoked.txt"), 10), "caption": "revoked file"})
	stop()

	// A new front end uses the same stored links and cursor, like a daemon
	// restart. The owner's front end has not read its backlog yet.
	m = mirror(true)
	stopOut = run(t, m.fe.Outbox)
	stop = m.showing(t)
	f.showing(t)
	f.event(t, id, "text", map[string]any{"text": "after restoration", "final": true})
	waitFor(t, "the new answer in both clients", func() bool {
		return posts(d, shown, "after restoration") == 1 && posts(f.driver, own, "after restoration") == 1
	})
	f.settled(t, own)
	if got := strings.Join(postsIn(d, shown), "\n"); strings.Contains(got, "revoked") {
		t.Errorf("revoked output was replayed: %s", got)
	}
	if posts(f.driver, own, "revoked answer") != 1 {
		t.Errorf("the continuously served client lost its backlog: %q", postsIn(f.driver, own))
	}
	if posts(f.driver, own, "revoked file") != 1 {
		t.Errorf("the continuously served client lost its file: %q", postsIn(f.driver, own))
	}
	stop()
	stopOut()

	// Delayed rows, including progress keyed by an old event, must obey
	// the boundary too, even after another restart with the entry present.
	m = mirror(true)
	stopOut = run(t, m.fe.Outbox)
	defer stopOut()
	if err := m.fe.Outbox.Write(context.Background(),
		row(hidden, linePart, shown, "revoked progress"),
		row(hidden, 77, shown, "revoked delayed output"),
		row(0, -99, shown, "new notice")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a new notice", func() bool { return posts(d, shown, "new notice") == 1 })
	var pending int
	waitFor(t, "the mirror's outbox", func() bool {
		err := f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM chat_outbox WHERE client = 'mirror' AND state = 'pending'`).Scan(&pending)
		return err == nil && pending == 0
	})
	if got := strings.Join(postsIn(d, shown), "\n"); strings.Contains(got, "revoked") {
		t.Errorf("revoked output survived a restart: %s", got)
	}
}
