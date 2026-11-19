package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
)

// recentFixture holds the text events of the wake capture
// (internal/supervisor/testdata/wake, from #271), stored as a daemon
// stored them, each under a thread of its own in the fixture. It returns
// the fixture's thread for each of the capture's.
func recentFixture(t *testing.T) (*fixture, map[control.ThreadID]control.ThreadID) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "supervisor", "testdata", "wake", "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Events []control.Event `json:"events"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fakeOwner{})
	ids := map[control.ThreadID]control.ThreadID{}
	for _, e := range c.Events {
		if e.Kind != "text" {
			continue
		}
		id, ok := ids[e.Thread]
		if !ok {
			id, _ = f.running(t, fmt.Sprintf("t%d", e.Thread))
			ids[e.Thread] = id
		}
		if _, err := f.core.Store.DB.Exec(`INSERT INTO events (scope, thread, thread_id, time, kind, data) VALUES ('s', '', ?, ?, ?, ?)`,
			int64(id), e.Time.UnixMilli(), string(e.Kind), []byte(e.Data)); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) == 0 {
		t.Fatal("no text events in the capture")
	}
	return f, ids
}

// Two messages of one turn (thread 4: "one", "two") and two turns (thread
// 2: "first", then "done") come back separated (#420).
func TestRecentSeparatesMessagesAndTurns(t *testing.T) {
	f, ids := recentFixture(t)
	for id, want := range map[control.ThreadID]string{
		4: "one\n\ntwo",
		2: "first\n\ndone",
		6: "done",
	} {
		got, err := f.core.recentText(context.Background(), ids[id], 8192)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("thread %d: recent %q, want %q", id, got, want)
		}
	}
}

// The cap counts the bytes of the result, separators included, and cuts at
// the start of a character. A cut inside a separator leaves none of it.
func TestRecentCap(t *testing.T) {
	f, ids := recentFixture(t)
	ctx := context.Background()
	full := "first\n\ndone"
	for limit := 1; limit <= len(full)+1; limit++ {
		got, err := f.core.recentText(ctx, ids[2], limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.TrimLeft(full[max(0, len(full)-limit):], "\n"); got != want {
			t.Errorf("thread 2, cap %d: %q, want %q", limit, got, want)
		}
	}
	// Thread 3's one message is over 1 KB of two-byte characters.
	whole, _ := f.core.recentText(ctx, ids[3], 1<<20)
	for limit := 1000; limit < 1010; limit++ {
		got, err := f.core.recentText(ctx, ids[3], limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > limit || len(got) < limit-utf8.UTFMax+1 || !utf8.ValidString(got) || !strings.HasSuffix(whole, got) {
			t.Errorf("thread 3, cap %d: %d bytes, valid %t, a suffix %t", limit, len(got), utf8.ValidString(got), strings.HasSuffix(whole, got))
		}
	}
}
