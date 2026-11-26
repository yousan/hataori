package chat

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
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
	if err := m.fe.Outbox.Write(context.Background(), row(hidden, 75, shown, "revoked queued output")); err != nil {
		t.Fatal(err)
	}
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

// A digest's newest event cannot authorize its older constituents. The
// same applies to the per-person counts at the end of quiet hours.
func TestRestoredLinkFiltersHeldConstituents(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "old only", true: "old and new"}[mixed], func(t *testing.T) {
			f := newFront(t, workspaces{}, func(fe *FrontEnd) {
				fe.ShowIn = map[string][]string{"c9": {channel}}
			})
			id := f.runningThread(t)
			own, shown := f.conv(id), f.shownIn(t, id, "c9")
			at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			end := at.Add(time.Hour)
			hold := func(pos uint64, at time.Time, text string, names ...string) {
				t.Helper()
				r := &reader{sv: map[control.ThreadID]svThread{id: {sv: true, scope: channel}},
					held: []heldPost{{thread: id, pos: pos, at: at, text: text}}}
				for _, n := range names {
					r.named = append(r.named, namedPost{thread: id, pos: pos, end: end, name: n})
				}
				if err := f.fe.Outbox.Store.Update(context.Background(), func(tx *store.Tx) error {
					return f.fe.holdTx(context.Background(), tx, r)
				}); err != nil {
					t.Fatal(err)
				}
			}
			old := f.event(t, id, "text", map[string]any{"text": "old report", "final": true})
			hold(old, at, "old report", "owner", "hook")
			f.fe.ShowIn = nil
			if err := f.fe.settleLinks(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.fe.ShowIn = map[string][]string{"c9": {channel}}
			if err := f.fe.settleLinks(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mixed {
				newPos := f.event(t, id, "text", map[string]any{"text": "new report", "final": true})
				hold(newPos, at.Add(time.Minute), "new report", "owner", "grace")
				// A repeated read does not double a naming event's count.
				hold(newPos, at.Add(time.Minute), "new report", "owner", "grace")
			}
			if err := f.fe.flushHeld(context.Background(), end.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			f.settled(t, own, shown)
			primary := strings.Join(postsIn(f.driver, own), "\n")
			secondary := strings.Join(postsIn(f.driver, shown), "\n")
			if !strings.Contains(primary, "old report") || !strings.Contains(primary, "@hook once") {
				t.Errorf("continuous link lost its held output: %s", primary)
			}
			if strings.Contains(secondary, "old report") || strings.Contains(secondary, "@hook") || strings.Contains(secondary, "2 times") {
				t.Errorf("restored link leaked old held output: %s", secondary)
			}
			if mixed {
				if !strings.Contains(primary, "2 reports held") || !strings.Contains(primary, "@owner 2 times") {
					t.Errorf("continuous link lost its rollups: %s", primary)
				}
				for _, want := range []string{"1 report held", "new report", "@owner once", "@grace once"} {
					if !strings.Contains(secondary, want) {
						t.Errorf("restored link missing %q: %s", want, secondary)
					}
				}
			}
			for _, c := range f.driver.Calls(shown) {
				if c.Op == fake.OpPost && len(c.Message.Notify) != 0 {
					t.Errorf("secondary link mentioned people: %+v", c.Message)
				}
			}
			for _, table := range []string{"chat_held", "chat_named"} {
				var n int
				if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
					t.Errorf("%s left %d rows: %v", table, n, err)
				}
			}
		})
	}
}
