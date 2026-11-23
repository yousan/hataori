package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
)

// TestRealClaudeCodeWakeUsage measures, against the real Claude Code CLI,
// how much each digest wake of a supervisor uses, through a run of them,
// and runs only when asked for (docs/testing.md), as TestRealClaudeCode
// does:
//
//	HATAORI_REAL_CLAUDE=1 go test -run TestRealClaudeCodeWakeUsage -v -timeout 60m ./cmd/hataori
//
// Each wake runs in a conversation of its own (#676), so its usage (the
// input, the output and what is read from and written to the cache, as the
// CLI reports them for the turn) stays about the same however many came
// before; the check is that no
// wake uses more than a fifth above the first. It logs the least too: a
// wake far below the first is one whose first request the CLI made before
// hataori's MCP server was connected, without its tools.
// Between the wakes, a person talks to the supervisor now and then, which
// its own conversation, not a wake's, reads again.
func TestRealClaudeCodeWakeUsage(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run the check against the real CLI")
	}
	settingsBefore := userSettings(t)
	if v, err := exec.Command("claude", "--version").Output(); err == nil {
		t.Logf("CLI %s", strings.TrimSpace(string(v)))
	}
	bin := t.TempDir()
	phase1 := filepath.Join(bin, "claude-for-the-check")
	wrapper := filepath.Join(bin, "claude-for-the-supervisor-check")
	if err := os.WriteFile(phase1, []byte(realWrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte(strings.ReplaceAll(realSupervisorWrapper, "%s", phase1)), 0o700); err != nil {
		t.Fatal(err)
	}
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["login_relay"] = false
		cfg["owner"] = "boss"
		cfg["model"] = "haiku" // the scope's, and so the supervisor's
		cfg["people"] = []map[string]any{
			{"name": "boss", "ids": []string{remoteSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{remoteSurface + ":" + e2eHook}},
			{"name": "sv1", "ids": []string{supervisor.ID("c1")}},
		}
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": remoteSurface, "enabled": true,
			"people": []string{"dispatch"}, "supervisor": map[string]any{"quiet": "1s", "turns_per_hour": 0, "model": "haiku"}}}
		cfg["adapters"] = map[string]any{claudecode.Name: map[string]any{"command": wrapper}}
	})
	p.deadline = time.Now().Add(50 * time.Minute)
	p.start(t)
	repo := bareRepo(t)
	const wakes = 46 // as many as the day #676 measured had turns

	var sv, worker surface.Conversation
	waitSv := func(n int) {
		t.Helper()
		eventually(t, "the supervisor's turn", p.within(6*time.Minute), func() bool { return len(p.turnsEnded(t, sv)) >= n })
	}
	sv = p.thread(t, 0)
	waitSv(1)
	turns := 1 // the first message's
	p.in(c1, "!go repo:"+repo+" Reply with just the word: ok")
	worker = p.thread(t, 1)
	waitSv(turns + 1)
	turns++
	for i := 2; i <= wakes; i++ {
		if i%5 == 0 {
			p.in(sv, "Reply with just the word: hello")
			turns++
			waitSv(turns)
		}
		p.in(worker, "Reply with just the word: ok")
		eventually(t, "the worker's turn", p.within(4*time.Minute), func() bool { return len(p.turnsEnded(t, worker)) >= i })
		turns++
		waitSv(turns)
	}

	// What each wake of the supervisor used: the usage of its turn.
	d := p.db(t)
	var svID int64
	if err := d.QueryRow(`SELECT thread_id FROM links WHERE client = ? AND channel = ? AND thread = ?`,
		remoteSurface, sv.Channel, sv.Thread).Scan(&svID); err != nil {
		t.Fatal(err)
	}
	// The CLI records the last wake's usage only after its turn's end.
	eventually(t, "the last wake's usage", p.within(time.Minute), func() bool {
		var last, counted int64
		d.QueryRow(`SELECT max(CAST(json_extract(data, '$.turn') AS INTEGER)) FROM events WHERE thread_id = ? AND kind = 'turn_started'`, svID).Scan(&last)
		d.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'usage' AND json_extract(data, '$.turn') = ?`, svID, last).Scan(&counted)
		return counted > 0
	})
	rows, err := d.Query(`SELECT json_extract(s.data, '$.key'), max(coalesce(json_extract(u.data, '$.input'), 0) +
		coalesce(json_extract(u.data, '$.output'), 0) + coalesce(json_extract(u.data, '$.cache_read'), 0) +
		coalesce(json_extract(u.data, '$.cache_write'), 0))
		FROM events s JOIN events u ON u.thread_id = s.thread_id AND u.kind = 'usage'
			AND json_extract(u.data, '$.turn') = json_extract(s.data, '$.turn')
		WHERE s.thread_id = ? AND s.kind = 'turn_started' GROUP BY s.pos ORDER BY s.pos`, svID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var used []int64
	for rows.Next() {
		var key string
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			t.Fatal(err)
		}
		kind := "a person's turn"
		if strings.HasPrefix(key, supervisor.WakeKeyPrefix) {
			kind = "a wake"
			used = append(used, n)
		}
		t.Logf("%s: %d", kind, n)
	}
	if len(used) != wakes {
		t.Fatalf("%d wakes with usage, want each of %d", len(used), wakes)
	}
	most, least := used[0], used[0]
	for _, n := range used {
		most, least = max(most, n), min(least, n)
	}
	over := func(n int64) float64 { return float64(n) / float64(used[0]) }
	t.Logf("%d wakes: first %d, last %d (%.2f of the first), most %d (%.2f), least %d (%.2f)",
		len(used), used[0], used[len(used)-1], over(used[len(used)-1]), most, over(most), least, over(least))
	if float64(most) > 1.2*float64(used[0]) {
		t.Errorf("a wake uses %.2f of the first", float64(most)/float64(used[0]))
	}
	if after := userSettings(t); after != settingsBefore {
		t.Fatalf("the user's settings file changed: %s, now %s", settingsBefore, after)
	}
}
