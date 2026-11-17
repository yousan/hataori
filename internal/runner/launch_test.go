package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// hosts returns the pids of the session host processes of stateDir.
func hosts(t *testing.T, stateDir string) []string {
	t.Helper()
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	var pids []string
	for _, p := range cmdlines {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.HasSuffix(b, []byte("\x00role\x00host\x00"+stateDir+"\x00")) {
			pids = append(pids, filepath.Base(filepath.Dir(p)))
		}
	}
	return pids
}

// A daemon killed while it launches a new session's host leaves, after
// it starts again, exactly one host, and the first input is delivered once.
func TestDaemonKilledDuringALaunch(t *testing.T) {
	for _, point := range []string{"before host start", "after host start"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			t.Cleanup(func() { killSessions(root) })
			runDaemon(t, root, false, "die-at", point, "reply once").waitKilled(t)
			st := openStore(t, root)
			d := runDaemon(t, root, false, "resume")
			until(t, "the turn's end", st, "turn_ended", 1)
			time.Sleep(300 * time.Millisecond) // anything late would show now
			d.kill()
			got := counts(t, st)
			if got["turn_started"] != 1 || got["input"] != 1 || got["text"] != 1 {
				t.Errorf("events %v", got)
			}
			if h := hosts(t, StateDir(root, 1)); len(h) != 1 {
				t.Errorf("%d hosts run for the session: %v", len(h), h)
			}
			var launching int
			st.DB.QueryRow(`SELECT launch FROM sessions WHERE id = 1`).Scan(&launching)
			if launching != 0 {
				t.Errorf("launch = %d after the restart", launching)
			}
		})
	}
}

// A daemon killed after a new thread's workspace is in place and before
// it recorded it: after a restart the session starts in that workspace,
// whose contents are as they were, and the first input is delivered once.
func TestDaemonKilledAfterTheWorkspaceIsPlaced(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	runDaemon(t, root, false, "die-at+dieplaced", "no such point", "reply once").waitKilled(t)
	work := filepath.Join(root, "work", "t1")
	before, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("the workspace is not in place: %v", err)
	}
	st := openStore(t, root)
	d := runDaemon(t, root, false, "resume")
	until(t, "the turn's end", st, "turn_ended", 1)
	time.Sleep(300 * time.Millisecond)
	d.kill()
	got := counts(t, st)
	if got["turn_started"] != 1 || got["input"] != 1 || got["text"] != 1 {
		t.Errorf("events %v", got)
	}
	var dir string
	st.DB.QueryRow(`SELECT json_extract(spec, '$.spec.Dir') FROM sessions WHERE id = 1`).Scan(&dir)
	if dir != work {
		t.Errorf("the session runs in %q, want %q", dir, work)
	}
	after, _ := os.ReadDir(work)
	notes, _ := os.ReadFile(filepath.Join(work, "notes.txt"))
	if len(after) != len(before) || len(after) != 1 || string(notes) != "someone's work\n" {
		t.Errorf("the workspace changed: %v, then %v; notes %q", before, after, notes)
	}
}
