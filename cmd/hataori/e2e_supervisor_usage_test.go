package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/store"
)

// A wake ends with a line of the usage windows of each agent that a thread
// of the scope used in the last 24 hours and that reported them, with the
// report's age when it is older than the agent's interval. With no report
// there is none, and a person's message has none (#798).
func TestWakeSaysTheAgentsUsageWindows(t *testing.T) {
	record := t.TempDir()
	wt := newWakeTest(t, recordIn(record))
	stop, owner, sv := wt.start(t)
	defer stop()
	wake := func(n int, scope, key, script string) string {
		t.Helper()
		_, pos := wt.worker(t, owner, scope, key, script)
		wt.read(t, pos)
		wt.quiet(t)
		return wt.woken(t, owner, sv.ID, n)[n-1]
	}
	// usage is what a wake has from its usage lines on; all, its text.
	usage := func(all string) string {
		if i := strings.Index(all, "\nUsage windows of "); i >= 0 {
			return all[i+1:]
		}
		return ""
	}
	none := wake(1, "c1", "a", "reply same")
	if usage(none) != "" {
		t.Fatalf("no agent reported, and the wake has a line:\n%s", none)
	}

	five, seven := wakeBase.Add(3*time.Hour), wakeBase.Add(80*time.Hour)
	at := func(x time.Time) string { return x.Format("2006-01-02 15:04 MST") }
	windows := fmt.Sprintf("five_hour:300:41:%d seven_day:10080:56:%d", five.Unix(), seven.Unix())
	line := fmt.Sprintf("Usage windows of fake: five_hour 41%% used, resets %s; seven_day 56%% used, resets %s.", at(five), at(seven))
	// The report is 10 minutes old when it comes, for an agent that reports
	// every 5. The wake's clock runs ahead of the real time.
	old := wake(2, "c1", "b", "limits 600 300 "+windows+"\nreply same")
	var ms int64
	if err := wt.db.QueryRow(`SELECT at FROM agent_limits WHERE agent = 'fake'`).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	got := usage(old)
	age, ok := strings.CutPrefix(got, line+" Reported ")
	age, ok2 := strings.CutSuffix(age, " ago.\n")
	if !ok || !ok2 {
		t.Fatalf("the wake's end: %q, want %q with the report's age", got, line)
	}
	// Its age in whole minutes, at the moment the wake was made.
	minutes := 0
	for f := strings.Fields(age); len(f) >= 2; f = f[2:] {
		var n int
		fmt.Sscan(f[0], &n)
		minutes += n * map[string]int{"hour": 60, "hours": 60, "minute": 1, "minutes": 1}[f[1]]
	}
	if now := wt.clock().Now().Sub(time.UnixMilli(ms)); time.Duration(minutes)*time.Minute > now || now-time.Duration(minutes)*time.Minute > 2*time.Minute {
		t.Errorf("a report %s old is shown as %q", now, age)
	}

	// A current report has no age.
	with := wake(3, "c1", "c", "limits 0 86400 "+windows+"\nreply same")
	if got, want := usage(with), line+"\n"; got != want {
		t.Fatalf("the wake's end: %q, want %q\n%s", got, want, with)
	}
	// Otherwise it is as the wake without a report.
	if a, b := strings.SplitN(none, "\n", 2)[0], strings.SplitN(with, "\n", 2)[0]; a != b {
		t.Errorf("the wakes begin with %q and %q", a, b)
	}

	// Agents that reported too: one whose turn in this scope is over for 25
	// hours, one for 23 hours, one that only another scope used, and one
	// nobody used.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(wt.r.root, store.File)+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	used := func(scope, key, agent string, ago time.Duration) {
		t.Helper()
		id, _ := wt.worker(t, owner, scope, key, "reply same")
		if _, err := db.Exec(`UPDATE turns SET started_at = ?1, ended_at = ?1 WHERE session_id IN (SELECT id FROM sessions WHERE thread_id = ?2)`,
			wakeBase.Add(-ago).UnixMilli(), int64(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE sessions SET agent = ? WHERE thread_id = ?`, agent, int64(id)); err != nil {
			t.Fatal(err)
		}
	}
	used("c1", "d", "over", 25*time.Hour)
	used("c1", "e", "recent", 23*time.Hour)
	used("c2", "f", "theirs", time.Hour)
	ws, _ := json.Marshal([]map[string]any{{"name": "primary", "used": 7}})
	for _, a := range []string{"over", "recent", "theirs", "unused"} {
		if _, err := db.Exec(`INSERT INTO agent_limits (agent, at, every, windows) VALUES (?, ?, 86400000, ?)`, a, wakeBase.UnixMilli(), ws); err != nil {
			t.Fatal(err)
		}
	}
	got = usage(wake(4, "c1", "g", "limits 0 86400 "+windows+"\nreply same"))
	if want := line + "\nUsage windows of recent: primary 7% used.\n"; got != want {
		t.Errorf("the wake's end: %q, want %q", got, want)
	}

	sendAndRead(t, owner, sv.ID, "p", "reply person")
	for _, s := range starts(t, record) {
		for _, in := range s.Inputs {
			if !strings.HasPrefix(in, "hataori: ") && strings.Contains(in, "Usage windows of") {
				t.Errorf("a person's message with the line:\n%s", in)
			}
		}
	}
}
