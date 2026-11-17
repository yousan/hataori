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
