package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/wire"
)

// A supervisor's thread and a removed thread have no workspace to read: the
// page does not ask for it, on a load or when the thread is selected, so
// the daemon logs no warning, and the details panel says why there is none.
// A workspace that is gone although its thread was not removed is still
// asked for, and each read logs one warning.
func TestDashboardThreadsWithoutWorkspace(t *testing.T) {
	repo := bareRepo(t)
	r, _ := svRoot(t, true, func(cfg map[string]any) {
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	_, logged := runDaemonLog(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sv := supervisorReady(t, c, "c1")
	if !sv.Confined {
		t.Fatalf("the supervisor's thread is not listed as confined: %+v", sv)
	}
	start := func(key string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: key, Scope: "c1", Thread: key, Repo: repo, Input: control.Input{Text: "reply " + key}})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the thread to be idle", 20*time.Second, func() bool {
			d, err := c.Thread(ctx, id, 0, 0)
			return err == nil && d.State == control.Idle
		})
		return id
	}
	removed := start("removed")
	if err := c.CloseThread(ctx, removed); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveThread(ctx, removed); err != nil {
		t.Fatal(err)
	}
	gone := start("gone")
	// The only workspace left is the one of the thread gone; the
	// supervisor's directory is no git work tree.
	var left []string
	dirs, _ := filepath.Glob(filepath.Join(r.root, "state", "sessions", "*"))
	for _, d := range dirs {
		if spec, ok, _ := host.ReadSpec(d); ok {
			if _, err := os.Lstat(filepath.Join(spec.Dir, ".git")); err == nil {
				left = append(left, spec.Dir)
			}
		}
	}
	if len(left) != 1 {
		t.Fatalf("%d workspaces, want only the one of the last thread", len(left))
	}
	if err := os.RemoveAll(left[0]); err != nil {
		t.Fatal(err)
	}

	// warned waits until n reads of the gone workspace were logged, and
	// checks that they are all the reads that failed.
	warned := func(when string, n int) {
		t.Helper()
		want := fmt.Sprintf(`"dashboard workspace" thread=%d `, gone)
		eventually(t, "the warning "+when, 20*time.Second, func() bool { return strings.Count(logged(), want) >= n })
		var got []string
		for _, l := range strings.Split(logged(), "\n") {
			if strings.Contains(l, "dashboard workspace") {
				got = append(got, l)
			}
		}
		if len(got) != n || strings.Count(logged(), want) != n {
			t.Fatalf("%s: want %d warnings, all of thread %d:\n%s", when, n, gone, strings.Join(got, "\n"))
		}
	}
	// none: the page has asked for a workspace since it loaded, and only
	// for the one that is gone. A failed read is in no resource timing of
	// some browsers, so fetch notes what it is asked.
	none := fmt.Sprintf(`window.__asked.length > 0 && window.__asked.every(u => u === '/api/workspace?thread=%d')`, gone)
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__asked = [];
		const fetched = window.fetch;
		window.fetch = (u, ...rest) => { if (String(u).includes('/api/workspace?')) window.__asked.push(String(u)); return fetched(u, ...rest); };`})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1500, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	b.showAll()
	for _, id := range []control.ThreadID{sv.ID, removed, gone} {
		b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, id))
	}
	b.until("the reads of the first load", none)
	warned("after the first load", 1)
	warned("after the first load", 1)
	b.call("Page.reload", map[string]any{})
	b.until("the reads of the reload", none)
	warned("after a reload", 2)

	note := `document.querySelector('.details [data-field=noWorkspace]')`
	fields := `[...document.querySelectorAll('.details [data-block=workspace] [data-field]')].map(e => e.dataset.field).join(' ')`
	openThread(t, b, sv.ID)
	b.until("why a supervisor's thread has none", note+`?.dataset.why === 'supervisor'`)
	b.until("a supervisor's fields", fields+` === 'noWorkspace terminal'`)
	openThread(t, b, removed)
	b.until("why a removed thread has none", note+`?.dataset.why === 'removed'`)
	b.until("a removed thread's fields", fields+` === 'noWorkspace repo terminal'`)
	openThread(t, b, gone)
	b.until("the fields of a workspace that cannot be read", fields+` === 'repo branch lines uncommitted unpushed terminal' && !`+note)
	b.until("the reads after selecting each", none)
	warned("after selecting each", 3)
}
