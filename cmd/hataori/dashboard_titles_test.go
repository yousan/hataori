package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// titlesRig starts a daemon with the fake surface and the fake agent, and
// one thread per input in scope c1; it returns the threads in order.
func titlesRig(t *testing.T, r *chatRoot, inputs ...string) (*wire.Client, []control.ThreadID) {
	t.Helper()
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	repo := bareRepo(t)
	var ids []control.ThreadID
	for i, in := range inputs {
		id, err := c.Start(ctx, control.StartRequest{Key: fmt.Sprint("k", i), Scope: "c1", Repo: repo, Input: control.Input{Text: in}})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "a thread", 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
		ids = append(ids, id)
	}
	return c, ids
}

// A thread is shown by its chat thread's name once the surface reports one,
// following a rename, and else by its first line; an issue's or a pull
// request's address is shown short, as repo#N, in the tree, the tabs and
// the All threads table, with the whole title in the tooltip; the details
// panel writes it short too, whole in its tooltip (#631).
func TestDashboardThreadTitles(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) { cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"} })
	const (
		issue  = "Do https://github.com/acme/widgets/issues/12 (fix the login)"
		pull   = "https://github.com/acme/widgets/pull/34#discussion_r1 review the docs"
		plain  = "reply a plain title"
		toName = "reply to be renamed"
		inside = "See (https://github.com/acme/widgets/issues/12#issuecomment-1). Next"
		notOne = "https://example.com/widgets/issues/12abc is no issue"
	)
	c, ids := titlesRig(t, r, issue+"\nreply ok", pull+"\nreply ok", plain, toName, inside+"\nreply ok", notOne+"\nreply ok")
	ctx := context.Background()
	b := newBrowser(t)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	// shown is each thread's title in the tree, the tab bar and the table,
	// and its tooltip in each.
	type shown struct{ Tree, TreeTip, Table, TableTip string }
	shownOf := func(id control.ThreadID) shown {
		var s shown
		b.eval(fmt.Sprintf(`(() => {
			const r = document.querySelector('[data-section=threads] .row[data-thread="%[1]d"] .title');
			const c = document.querySelector('.all-threads tr[data-thread="%[1]d"] td[data-col=title]');
			return {tree: r?.textContent ?? '', treeTip: r?.title ?? '', table: c?.textContent ?? '', tableTip: c?.title ?? ''};
		})()`, id), &s)
		return s
	}
	untilShown := func(what string, id control.ThreadID, title, full string) {
		t.Helper()
		var s shown
		want := shown{title, full, title, full}
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if s = shownOf(id); s == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the page shows %+v, want %+v", what, s, want)
			}
		}
	}
	// 1. Short forms, and a title without an address unchanged.
	untilShown("an issue's address", ids[0], "Do widgets#12 (fix the login)", issue)
	untilShown("a pull request's address first", ids[1], "widgets#34 review the docs", pull)
	untilShown("a title without an address", ids[2], plain, plain)
	untilShown("the thread before its rename", ids[3], toName, toName)
	untilShown("an address in brackets, with punctuation after it", ids[4], "See (widgets#12). Next", inside)
	untilShown("an address whose number does not stand alone", ids[5], notOne, notOne)

	// 2. The tab, the pane and the details panel show it short too, whole
	// in the tooltip.
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, ids[0]))
	b.until("the tab, short, with the whole title as its tooltip", fmt.Sprintf(`(() => {
		const tab = document.querySelector('.tabbar .tab[data-tab="thread:%[1]d"] .tab-title');
		return tab?.textContent === %[2]q && tab.title === %[3]q;
	})()`, ids[0], "Do widgets#12 (fix the login)", issue))
	b.until("the details panel, short, with the whole title as its tooltip", fmt.Sprintf(`(() => {
		const dd = document.querySelector('.details [data-field=title] dd');
		return dd?.textContent === %[1]q && dd.title === %[2]q;
	})()`, "Do widgets#12 (fix the login)", issue))

	// 3. The chat thread's own name, from the surface, and its rename.
	b.click(`.tabbar .tab[data-tab=all] .tab-main`)
	d, err := c.Thread(ctx, ids[3], 0, 0)
	if err != nil || len(d.Links) == 0 || d.Links[0].Thread == "" {
		t.Fatalf("the thread's link: %+v, %v", d.Links, err)
	}
	conv := surface.Conversation{Channel: d.Links[0].Channel, Thread: d.Links[0].Thread}
	r.driver.Rename(conv, "Login page: rework")
	untilShown("the chat thread's name", ids[3], "Login page: rework", "Login page: rework")
	eventually(t, "the name through the control API", 10*time.Second, func() bool {
		d, err := c.Thread(ctx, ids[3], 0, 0)
		return err == nil && d.Name == "Login page: rework" && d.Title == toName
	})
	r.driver.Rename(conv, "Login page: done, see https://github.com/acme/widgets/pull/40")
	untilShown("the chat thread renamed again", ids[3], "Login page: done, see widgets#40", "Login page: done, see https://github.com/acme/widgets/pull/40")
}

// A caller that may not read text gets neither a thread's first line nor
// its chat thread's name.
func TestDashboardThreadNameWithheld(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) { cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"} })
	dashboardHook = func(c *control.Caller) { c.NoText = true }
	t.Cleanup(func() { dashboardHook = nil })
	c, ids := titlesRig(t, r, "reply secret-first-line")
	ctx := context.Background()
	d, err := c.Thread(ctx, ids[0], 0, 0)
	if err != nil || len(d.Links) == 0 {
		t.Fatalf("the thread's link: %+v, %v", d.Links, err)
	}
	r.driver.Rename(surface.Conversation{Channel: d.Links[0].Channel, Thread: d.Links[0].Thread}, "secret-name")
	eventually(t, "the owner to see the name", 10*time.Second, func() bool {
		d, err := c.Thread(ctx, ids[0], 0, 0)
		return err == nil && d.Name == "secret-name"
	})
	base, cookie := dashSignIn(t, r.root)
	for _, path := range []string{"/api/threads", fmt.Sprintf("/api/thread?thread=%d", ids[0])} {
		code, body := dashGet(t, base, cookie, path)
		var v any
		if code != 200 || json.Unmarshal([]byte(body), &v) != nil {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		if json.Valid([]byte(body)) && (strings.Contains(body, "secret-name") || strings.Contains(body, "secret-first-line")) {
			t.Fatalf("%s gives a caller that may not read text the thread's text: %s", path, body)
		}
	}
}
