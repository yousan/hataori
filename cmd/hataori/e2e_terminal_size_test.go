package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// A session's terminal has the size the config gives, 80×24 without one:
// a thread started without terminal_size is 80×24; after the config sets
// 120×40 and the daemon starts again, the same thread resumed is 120×40,
// and so is a new one; without the key again, it is back to 80×24. The
// thread's tab, open on the dashboard across the restarts, fits each size
// into its pane.
func TestSessionTerminalSize(t *testing.T) {
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["dashboard"] = map[string]any{"listen": freePort(t)} })
	stop := runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) // the client of the daemon running last
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	repo := bareRepo(t)
	info := func(id control.ThreadID) control.ThreadInfo {
		t.Helper()
		d, err := c.Thread(ctx, id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return d.ThreadInfo
	}
	// settle waits for the thread to be idle after turns turns, with a
	// terminal, and returns the terminal's size as the kernel has it.
	settle := func(what string, id control.ThreadID, turns int) string {
		t.Helper()
		var x control.ThreadInfo
		eventually(t, what, 20*time.Second, func() bool {
			x = info(id)
			return x.State == control.Idle && x.Turns == turns && x.Terminal != ""
		})
		return ptySize(t, x.Terminal)
	}

	// Without terminal_size: 80×24.
	first, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: repo, Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := settle("the first thread", first, 1); got != "80x24" {
		t.Fatalf("without terminal_size the terminal is %s, not 80x24", got)
	}
	n := int64(first)

	// The thread's tab on the dashboard, fitted at 80×24.
	b := newBrowser(t)
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1400, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, n))
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, n))
	view := fmt.Sprintf(`.pane[data-pane="0"] [data-terminal="%d"]`, n)
	b.untilFilled("the thread at 80x24", view, 80, 24)

	// restart puts the thread to sleep, then starts the daemon again with
	// the config changed by f.
	sleeps := 0
	restart := func(f func(cfg map[string]any)) {
		t.Helper()
		sleeps++
		if err := c.Send(ctx, first, fmt.Sprint("k-exit-", sleeps), control.Input{Text: "exit"}, control.Refuse); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the thread to fall asleep", 20*time.Second, func() bool { return info(first).State == control.Asleep })
		c.Close()
		stop()
		setConfig(t, root, f)
		stop = runDaemon(t, root)
		if c, err = wire.Dial(root); err != nil {
			t.Fatal(err)
		}
	}
	restart(func(cfg map[string]any) { cfg["terminal_size"] = map[string]any{"cols": 120, "rows": 40} })

	// Resumed: 120×40, and the open tab fits the new size.
	if err := c.Send(ctx, first, "k-wake", control.Input{Text: "reply two"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	if got := settle("the resumed thread", first, 3); got != "120x40" {
		t.Fatalf("the resumed session's terminal is %s, not 120x40", got)
	}
	b.untilFilled("the thread at 120x40", view, 120, 40)

	// New: 120×40.
	second, err := c.Start(ctx, control.StartRequest{Key: "k2", Scope: "s", Thread: "t2", Repo: repo, Input: control.Input{Text: "reply new"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := settle("the new thread", second, 1); got != "120x40" {
		t.Fatalf("a new session's terminal is %s, not 120x40", got)
	}

	// Without the key again: the thread resumed is 80×24, and its tab fits
	// the smaller grid again even though the pane keeps its size: the
	// "no process" line, which comes and goes with the session, is held
	// out of the layout here.
	restart(func(cfg map[string]any) { delete(cfg, "terminal_size") })
	b.eval(`(() => { const s = document.createElement('style'); s.textContent = '.terminal-view .no-process { position: absolute; visibility: hidden; }'; document.head.append(s); return true; })()`, nil)
	b.until("the asleep thread's last screen at 120x40", fmt.Sprintf(`document.querySelector('%s .xterm')?.dataset.cols === '120'`, view))
	if err := c.Send(ctx, first, "k-back", control.Input{Text: "reply three"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	if got := settle("the thread resumed without terminal_size", first, 5); got != "80x24" {
		t.Fatalf("without terminal_size the resumed terminal is %s, not 80x24", got)
	}
	b.untilFilled("the thread back at 80x24", view, 80, 24)
}
