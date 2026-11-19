package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// The status bar's remaining usage, driven by the real server and the
// fake agent: nothing while the agent has reported none; a report older
// than its agent's update interval is shown with its time; a current one
// without, with each window's share used and reset time as the server
// has them.
func TestDashboardLimits(t *testing.T) {
	root := terminalRoot(t)
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply quiet"}})
	if err != nil {
		t.Fatal(err)
	}
	base, cookie := dashSignIn(t, root)
	limits := func() []control.AgentLimits {
		t.Helper()
		code, body := dashGet(t, base, cookie, "/api/limits")
		var ls []control.AgentLimits
		if err := json.Unmarshal([]byte(body), &ls); code != http.StatusOK || err != nil {
			t.Fatalf("/api/limits: %d %s", code, body)
		}
		return ls
	}
	turns := func(n int) {
		t.Helper()
		eventually(t, fmt.Sprintf("turn %d to end", n), 20*time.Second, func() bool {
			ts, err := c.Threads(ctx, control.Filter{})
			return err == nil && len(ts) == 1 && ts[0].State == control.Idle && ts[0].Turns == n
		})
	}
	turns(1)

	b := newBrowser(t)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread", `document.querySelectorAll('[data-section=threads] .row').length === 1`)

	// No report: no entry, and nothing in the status bar.
	if ls := limits(); len(ls) != 0 {
		t.Fatalf("before any report: %+v", ls)
	}
	var shown bool
	b.eval(`!!document.querySelector('.status .limits')`, &shown)
	if shown {
		t.Fatal("the status bar shows usage no agent reported")
	}

	// A report made 10 minutes ago, by an agent that reports every 5.
	send := func(key, text string, n int) {
		t.Helper()
		if err := c.Send(ctx, id, key, control.Input{Text: text}, control.Refuse); err != nil {
			t.Fatal(err)
		}
		turns(n)
	}
	send("k2", "limits 600 300 seven_day:10080:30:1795161600\nreply old", 2)
	ls := limits()
	if len(ls) != 1 || ls[0].Agent != "fake" || ls[0].Every != 300 || time.Since(ls[0].At) < 9*time.Minute {
		t.Fatalf("the old report: %+v", ls)
	}
	var asOf string
	b.eval(fmt.Sprintf(`new Date(%q).toLocaleString('en', {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'})`,
		ls[0].At.Format(time.RFC3339Nano)), &asOf)
	b.until("the old report with its time", fmt.Sprintf(`(() => {
		const l = document.querySelector('.status .limit[data-agent=fake]');
		return !!l && l.dataset.stale === 'yes' && l.querySelector('.limit-as-of')?.textContent === %s &&
			l.querySelector('[data-window=seven_day]')?.dataset.used === '30';
	})()`, js("as of "+asOf)))

	// A current report: shown without a time, with the server's windows.
	send("k3", "limits 0 300 five_hour:300:4:1795000000 seven_day:10080:21:1795161600\nreply new", 3)
	ls = limits()
	if len(ls) != 1 || len(ls[0].Windows) != 2 {
		t.Fatalf("the new report: %+v", ls)
	}
	var want []string
	for _, w := range ls[0].Windows {
		var resets string
		b.eval(fmt.Sprintf(`new Date(%q).toLocaleString('en', {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'})`,
			w.Resets.Format(time.RFC3339)), &resets)
		label := map[int64]string{300: "5h", 10080: "7d"}[w.Minutes]
		want = append(want, fmt.Sprintf("%s %d%% used· resets %s", label, w.Used, resets))
	}
	b.until("the new report, current", fmt.Sprintf(`(() => {
		const l = document.querySelector('.status .limit[data-agent=fake]');
		if (!l || l.dataset.stale !== 'no' || l.querySelector('.limit-as-of')) return false;
		const ws = [...l.querySelectorAll('.limit-window')].map(w => w.textContent);
		return JSON.stringify(ws) === %s;
	})()`, js(js(want))))
}
