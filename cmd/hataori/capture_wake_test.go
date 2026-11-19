package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// TestCaptureWakeEvents writes the capture behind the supervisor's digest
// golden file (internal/supervisor/testdata/wake): the events of a scope
// in which threads ended their turns in each way, as a daemon with the
// fake agent wrote them, and each thread as the waker reads it. It runs
// only when HATAORI_CAPTURE_WAKE names the directory to write.
//
//	HATAORI_CAPTURE_WAKE=$PWD/internal/supervisor/testdata/wake go test -run TestCaptureWakeEvents ./cmd/hataori
func TestCaptureWakeEvents(t *testing.T) {
	out := os.Getenv("HATAORI_CAPTURE_WAKE")
	if out == "" {
		t.Skip("set HATAORI_CAPTURE_WAKE to the directory to write")
	}
	wt := newWakeTest(t)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	long := "reply " + strings.TrimSpace(strings.Repeat("ünïcödé text, ", 120))
	scripts := []string{
		"reply first", // and a second turn below, which fails
		long,
		"reply one\nreply two",
		"ask Which way? -- left | right",
		"fail limit",
		"background 1h\nreply started a long task",
		"reply done",
	}
	var ids []control.ThreadID
	for i, s := range scripts {
		id, _ := wt.worker(t, owner, "c1", "k"+strconv.Itoa(i), s)
		ids = append(ids, id)
	}
	if err := owner.Send(ctx, ids[0], "again", control.Input{Text: "fail api_error"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.ended(t, ids[0], 2)

	var c struct {
		Supervisor control.ThreadID                 `json:"supervisor"`
		Events     []control.Event                  `json:"events"`
		Threads    map[control.ThreadID]wakeCapture `json:"threads"`
	}
	c.Supervisor, c.Threads = sv.ID, map[control.ThreadID]wakeCapture{}
	evCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	evs, err := owner.Events(evCtx, control.Filter{Scope: "c1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		c.Events = append(c.Events, e)
	}
	cancel()
	for _, id := range ids {
		d, err := owner.Thread(ctx, id, 8192, 0)
		if err != nil {
			t.Fatal(err)
		}
		c.Threads[id] = wakeCapture{Title: d.Title, Recent: d.Recent}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// No path of this machine goes into the repository.
	s := strings.ReplaceAll(string(b), wt.repo, "/repo.git")
	s = strings.ReplaceAll(s, wt.r.root, "/root")
	for _, p := range []string{os.TempDir(), "/var/folders", "/private/", "/home/", "/Users/"} {
		if strings.Contains(s, p) {
			t.Fatalf("the capture holds a path of this machine (%s)", p)
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "capture.json"), []byte(s+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s; the supervisor's own thread is %d", filepath.Join(out, "capture.json"), sv.ID)
}

// wakeCapture is a thread as the waker reads it for a digest.
type wakeCapture struct {
	Title  string `json:"title"`
	Recent string `json:"recent"`
}
