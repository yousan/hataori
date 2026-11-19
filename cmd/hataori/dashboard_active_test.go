package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/wire"
)

// setThreadState puts a stopped daemon's thread, and its sessions, in
// state s: the closed and removed states have no control operation yet.
func setThreadState(t *testing.T, root string, id control.ThreadID, s control.State) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, store.File))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE threads SET state = ? WHERE id = ?`, string(s), int64(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET state = ? WHERE thread_id = ?`, string(s), int64(id)); err != nil {
		t.Fatal(err)
	}
}

// showAll chooses to show every thread, asleep ones too, with the tab
// bar's control, as a person would.
func (b *browser) showAll() {
	b.t.Helper()
	b.until("the choice of the threads shown", `!!document.querySelector('.tools .show [data-choice=all]')`)
	b.click(`.tools .show [data-choice=all]`)
}

// numbersOf is what a list of thread numbers from the page holds, sorted.
func numbersOf(ns []int64) []int64 { ns = slices.Clone(ns); slices.Sort(ns); return ns }

// By default the tree, the All threads table and the narrow list show only
// the active threads (creating, idle, running, waiting, failed), driven by
// the real server with the fake agent: one control switches to all and
// back, with the number hidden beside it, and the choice survives a
// reload; threads that wake or fall asleep come and go without a reload,
// and an open tab stays; the footer counts every thread; the filter works
// on what is shown.
func TestDashboardActiveThreadsOnly(t *testing.T) {
	root := terminalRoot(t)
	dir := t.TempDir()
	once, slow := filepath.Join(dir, "fail-once"), filepath.Join(dir, "slow-once")
	// The agent's command: with the file once it fails to start, with the
	// file slow it never reports that it started, so its thread stays
	// creating until the start times out; each file is used once.
	setConfig(t, root, func(cfg map[string]any) {
		ad := cfg["adapters"].(map[string]any)[fake.Name].(map[string]any)
		ad["command"] = []string{"sh", "-c",
			`once=$1; slow=$2; shift 2; if [ -e "$once" ]; then rm -f "$once"; exit 3; fi; if [ -e "$slow" ]; then rm -f "$slow"; exec sleep 600; fi; exec "$@"`,
			"sh", once, slow, os.Args[0], "fake"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	repo := bareRepo(t)
	var c *wire.Client
	dial := func() {
		t.Helper()
		var err error
		if c, err = wire.Dial(root); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
	}
	start := func(key, text string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: "k-" + key, Scope: "s", Thread: key, Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	info := func(id control.ThreadID) control.ThreadInfo {
		t.Helper()
		d, err := c.Thread(ctx, id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return d.ThreadInfo
	}
	settle := func(what string, id control.ThreadID, s control.State) {
		t.Helper()
		eventually(t, what, 20*time.Second, func() bool { return info(id).State == s })
	}

	// Three threads fall asleep; with the daemon stopped, one is closed and
	// one removed.
	stop := runDaemon(t, root)
	dial()
	sleepy := start("t1", "reply alpha-sleeps\nexit")
	closing := start("t2", "reply beta-closes\nexit")
	removing := start("t3", "reply gamma-removed\nexit")
	for _, id := range []control.ThreadID{sleepy, closing, removing} {
		settle("a thread to fall asleep", id, control.Asleep)
	}
	c.Close()
	stop()
	setThreadState(t, root, closing, control.Closed)
	setThreadState(t, root, removing, control.Removed)
	runDaemon(t, root)
	dial()
	if info(closing).State != control.Closed || info(removing).State != control.Removed {
		t.Fatalf("closed %s, removed %s", info(closing).State, info(removing).State)
	}

	// The active threads: failed, idle, running, waiting, creating.
	os.WriteFile(once, nil, 0o600)
	failing := start("t4", "reply never")
	settle("the failing thread", failing, control.Failed)
	idle := start("t5", "reply done")
	running := start("t6", "tool long 10m")
	waiting := start("t7", "ask Proceed with the deploy?")
	settle("the idle thread", idle, control.Idle)
	settle("the running thread", running, control.Running)
	settle("the waiting thread", waiting, control.Waiting)
	os.WriteFile(slow, nil, 0o600)
	creating := start("t8", "reply slow start")
	eventually(t, "the creating thread", 20*time.Second, func() bool {
		_, err := os.Stat(slow)
		return os.IsNotExist(err) && info(creating).State == control.Creating
	})

	num := func(id control.ThreadID) int64 { return int64(info(id).ID) }
	nums := func(ids ...control.ThreadID) []int64 {
		var ns []int64
		for _, id := range ids {
			ns = append(ns, num(id))
		}
		return numbersOf(ns)
	}
	active := nums(failing, idle, running, waiting, creating)
	every := nums(failing, idle, running, waiting, creating, sleepy, closing, removing)

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})

	// shown is what the page shows: the tree's rows, the table's rows, the
	// choice and the number hidden ("" when none is shown).
	type shown struct {
		Tree, Table []int64
		Choice      string
		Hidden      string
		Text        string
	}
	page := func() shown {
		t.Helper()
		var s shown
		b.eval(`(() => {
			const h = document.querySelector('.tools .show .hidden-count');
			return {
				tree: [...document.querySelectorAll('[data-section=threads] .row')].map(r => +r.dataset.thread),
				table: [...document.querySelectorAll('.all-threads tr[data-thread]')].map(r => +r.dataset.thread),
				choice: document.querySelector('.tools .show [aria-pressed=true]')?.dataset.choice ?? '',
				hidden: h ? h.dataset.hidden : '',
				text: h ? h.textContent : '',
			};
		})()`, &s)
		s.Tree, s.Table = numbersOf(s.Tree), numbersOf(s.Table)
		return s
	}
	untilShown := func(what string, ok func(s shown) bool) {
		t.Helper()
		var s shown
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if s = page(); ok(s) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the page shows %+v", what, s)
			}
		}
	}
	showsOnly := func(want []int64, choice string, hidden int) func(s shown) bool {
		return func(s shown) bool {
			h := ""
			if choice == "active" {
				h = fmt.Sprint(hidden)
			}
			return reflect.DeepEqual(s.Tree, want) && reflect.DeepEqual(s.Table, want) && s.Choice == choice && s.Hidden == h &&
				(choice != "active" || s.Text == fmt.Sprintf("%d hidden", hidden))
		}
	}

	// 1. By default only the active threads, and "3 hidden" beside the
	// control. Each active state has its row; no hidden state has one.
	untilShown("the active threads only", showsOnly(active, "active", 3))
	b.until("the creating thread's row", fmt.Sprintf(`document.querySelector('[data-section=threads] .row[data-thread="%d"]')?.dataset.state === 'creating'`, num(creating)))
	var states []string
	b.eval(`[...new Set([...document.querySelectorAll('[data-section=threads] .row, .all-threads tr[data-thread]')].map(r => r.dataset.state))].sort()`, &states)
	for _, s := range states {
		if s == "asleep" || s == "closed" || s == "removed" {
			t.Fatalf("a row in state %s is shown: %v", s, states)
		}
	}

	// 2. The footer counts every thread, hidden ones too, as the server
	// has them.
	base, cookie := dashSignIn(t, root)
	countsMatch := func(asleep int) bool {
		code, body := dashGet(t, base, cookie, "/api/threads")
		var ts []control.ThreadInfo
		if err := json.Unmarshal([]byte(body), &ts); code != http.StatusOK || err != nil {
			t.Fatalf("/api/threads: %d %s", code, body)
		}
		want := map[string]int{}
		for _, x := range ts {
			want[string(x.State)]++
		}
		var got map[string]int
		b.eval(`Object.fromEntries([...document.querySelectorAll('.counts [data-count]')].map(li => [li.dataset.count, +li.dataset.n]))`, &got)
		return reflect.DeepEqual(got, want) && got["asleep"] == asleep && got["closed"] == 1 && got["removed"] == 1
	}
	eventually(t, "the footer to count every thread", 20*time.Second, func() bool { return countsMatch(1) })

	// 3. The control switches to all and back; the choice survives a reload.
	reload := func() {
		t.Helper()
		b.eval(`document.body.dataset.reloading = 'yes', true`, nil)
		b.call("Page.reload", map[string]any{})
		b.until("the page after the reload", `document.body.dataset.reloading !== 'yes' && document.querySelectorAll('[data-section=threads] .row').length > 0`)
	}
	b.click(`.tools .show [data-choice=all]`)
	untilShown("all threads", showsOnly(every, "all", 0))
	reload()
	untilShown("all threads after a reload", showsOnly(every, "all", 0))
	b.click(`.tools .show [data-choice=active]`)
	untilShown("the active threads again", showsOnly(active, "active", 3))
	reload()
	untilShown("the active threads after a reload", showsOnly(active, "active", 3))

	// 4. The filter works on what is shown, and the number hidden stays.
	noneText := func() string {
		var s string
		b.eval(`document.querySelector('[data-section=threads] .none')?.textContent ?? ''`, &s)
		return s
	}
	b.filter("alpha-sleeps")
	untilShown("a filter that matches only a hidden thread", func(s shown) bool { return len(s.Tree) == 0 && s.Hidden == "3" })
	if got, want := noneText(), "No active thread matches the filter. 3 hidden threads are not searched."; got != want {
		t.Fatalf("the filter's message: %q, want %q", got, want)
	}
	b.filter("done")
	untilShown("a filter that matches an active thread", func(s shown) bool {
		return reflect.DeepEqual(s.Tree, []int64{num(idle)}) && s.Hidden == "3"
	})
	b.click(`.tools .show [data-choice=all]`)
	b.filter("alpha-sleeps")
	untilShown("the hidden thread with all", func(s shown) bool { return reflect.DeepEqual(s.Tree, []int64{num(sleepy)}) })
	b.filter("")
	b.click(`.tools .show [data-choice=active]`)
	untilShown("the active threads, unfiltered", showsOnly(active, "active", 3))

	// 5. Without a reload: a thread with an open tab falls asleep and leaves
	// the tree and the table, and keeps its tab, its pane and the status
	// bar; an asleep thread wakes and comes back; with all chosen, a thread
	// that falls asleep stays.
	b.eval(`window.__samePage = true`, nil)
	nIdle := num(idle)
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, nIdle))
	b.until("the idle thread's tab", fmt.Sprintf(`!!document.querySelector('.pane.active[data-tab="thread:%d"] .thread-pane')`, nIdle))
	// Two panes: the thread's, and the All threads table viewed before it.
	b.click(`[data-tool=split]`)
	b.until("the table beside the thread", `!!document.querySelector('.pane[data-pane="1"][data-tab=all] .all-threads')`)
	if err := c.Send(ctx, idle, "k-sleep", control.Input{Text: "exit"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	settle("the idle thread to fall asleep", idle, control.Asleep)
	activeNow := nums(failing, running, waiting, creating)
	untilShown("the thread that fell asleep gone", showsOnly(activeNow, "active", 4))
	b.until("its tab, its pane and the status bar kept", fmt.Sprintf(`(() => {
		const tab = document.querySelector('.tabbar .tab[data-tab="thread:%[1]d"]');
		return !!tab && tab.querySelector('.tab-title').textContent === 'reply done' &&
			tab.querySelector('.agent-mark').classList.contains('state-asleep') &&
			!!document.querySelector('.pane.active[data-tab="thread:%[1]d"] .thread-pane') &&
			document.querySelector('.status-item').dataset.selected == %[1]d;
	})()`, nIdle))

	if err := c.Send(ctx, sleepy, "k-wake", control.Input{Text: "reply awake"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	settle("the asleep thread to wake", sleepy, control.Idle)
	untilShown("the thread that woke", showsOnly(nums(failing, running, waiting, creating, sleepy), "active", 3))

	b.click(`.tools .show [data-choice=all]`)
	if err := c.Send(ctx, sleepy, "k-sleep-again", control.Input{Text: "exit"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	settle("the woken thread to fall asleep again", sleepy, control.Asleep)
	b.until("the asleep thread kept with all", fmt.Sprintf(`document.querySelector('[data-section=threads] .row[data-thread="%d"]')?.dataset.state === 'asleep'`, num(sleepy)))
	untilShown("every thread with all", showsOnly(every, "all", 0))
	var same bool
	b.eval(`window.__samePage === true`, &same)
	if !same {
		t.Fatal("the page was reloaded")
	}
	eventually(t, "the footer to count every thread after the changes", 20*time.Second, func() bool { return countsMatch(2) })

	// 6. The narrow list follows the same choice, with the same control.
	b.click(`.tools .show [data-choice=active]`)
	b.phone()
	b.until("the narrow list", `document.querySelector('.shell')?.dataset.layout === 'narrow'`)
	narrow := func() (rows []int64, hidden string) {
		var r struct {
			Rows   []int64
			Hidden string
		}
		b.eval(`({rows: [...document.querySelectorAll('.narrow-list .row[data-thread]')].map(r => +r.dataset.thread),
			hidden: document.querySelector('.narrow-show .hidden-count')?.dataset.hidden ?? ''})`, &r)
		return numbersOf(r.Rows), r.Hidden
	}
	eventually(t, "the narrow list of the active threads", 20*time.Second, func() bool {
		rows, hidden := narrow()
		return reflect.DeepEqual(rows, activeNow) && hidden == "4"
	})
	b.noSideScroll("the narrow list with the choice")
	b.click(`.narrow-show .show [data-choice=all]`)
	eventually(t, "the narrow list of all threads", 20*time.Second, func() bool {
		rows, hidden := narrow()
		return reflect.DeepEqual(rows, every) && hidden == ""
	})

	var csp []string
	b.eval(`window.__csp`, &csp)
	if len(csp) > 0 {
		t.Fatalf("CSP violations: %v", csp)
	}
}
