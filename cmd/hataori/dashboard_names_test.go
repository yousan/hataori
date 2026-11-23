package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// namesRig is the daemon with the fake surface and the fake agent, a
// config with people, and two threads: one in a scope the config names,
// with every column of the All threads table, a title with an issue's
// address, and a row from before callers were resolved to people, which
// records the bare chat id of its author; one asleep in a scope named by
// its channel only, started by an id no person lists. Two idle shells of a
// fake list of host terminals are hidden, as the threads asleep are.
type namesRig struct {
	r            *chatRoot
	c            *wire.Client
	full, asleep control.ThreadID
	base, link   string
}

// namesTitle is the full thread's title: an issue's address, which the
// tree and the tabs write short as "widgets#631".
const (
	namesTitle = "Do https://github.com/example/widgets/issues/631 and make the title column wide enough to read"
	namesShort = "Do widgets#631 and make the title column wide enough to read"
)

func newNamesRig(t *testing.T) namesRig {
	t.Helper()
	terms := &listedTerms{}
	for i := range 2 {
		terms.list = append(terms.list, control.HostTerminal{Device: fmt.Sprintf("/dev/pts/%d", 961+i), Process: "bash", Activity: ago(72 * time.Hour),
			Tmux: &control.TmuxPane{Pane: fmt.Sprintf("%%%d", 61+i), Session: "work", Window: fmt.Sprint(i), WindowName: "w", Index: "0"}})
	}
	hostTermsSource = terms
	t.Cleanup(func() { hostTermsSource = nil })
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["host_terminals"] = true
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "bob", "ids": []string{fakeSurface + ":" + e2eHook}},
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "name": "Builds and releases", "people": []string{"bob"}},
			{"key": "c2", "client": fakeSurface, "enabled": true, "people": []string{"bob"}},
		}
	})
	r.driver.SetName(surface.Conversation{Channel: "c2"}, "#deploys")
	stop := runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The full thread, ordered in the chat by bob; its chat thread is
	// renamed to the title with the issue's address.
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" usage 1200 300 40000 5000 45000 200000 a-model-with-a-name\nreply "+strings.Repeat("the last line of the agent goes on ", 6))
	r.driver.Rename(th, namesTitle)
	var full control.ThreadID
	eventually(t, "the full thread idle, with its name", 30*time.Second, func() bool {
		ts, err := c.Threads(ctx, control.Filter{})
		if err != nil || len(ts) != 1 {
			return false
		}
		full = ts[0].ID
		return ts[0].State == control.Idle && ts[0].Name == namesTitle && ts[0].StartedBy == "bob"
	})
	asleep, err := c.Start(ctx, control.StartRequest{Key: "k-asleep", Scope: "c2", Repo: bareRepo(t), Input: control.Input{Text: "exit"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second thread asleep", 30*time.Second, func() bool {
		d, err := c.Thread(ctx, asleep, 0, 0)
		return err == nil && d.State == control.Asleep
	})
	eventually(t, "the second scope named by its channel", 10*time.Second, func() bool {
		ss, err := c.Scopes(ctx)
		for _, s := range ss {
			if s.Key == "c2" {
				return err == nil && s.Name == "#deploys"
			}
		}
		return false
	})
	// The rows as a daemon from before callers were resolved to people
	// wrote them: the author's chat id, without its client. The daemon
	// is stopped meanwhile, as it holds the store.
	c.Close()
	stop()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File))
	if err != nil {
		t.Fatal(err)
	}
	for id, by := range map[control.ThreadID]string{full: e2eHook, asleep: "stranger-9"} {
		if _, err := db.Exec(`UPDATE threads SET started_by = ? WHERE id = ?`, by, int64(id)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	runDaemon(t, r.root)
	r.connected(t)
	if c, err = wire.Dial(r.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	eventually(t, "the full thread idle again", 30*time.Second, func() bool {
		d, err := c.Thread(ctx, full, 0, 0)
		return err == nil && d.State == control.Idle
	})
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return namesRig{r: r, c: c, full: full, asleep: asleep, base: u.Scheme + "://" + u.Host, link: link}
}

// The acceptance criteria of #631 in the browser, against the daemon with
// the fake surface: the All threads table's Title column, at 1600 px wide
// with every column shown, shows at least 30 characters of a title and is
// no narrower than the other text columns; the counts of hidden threads
// and hidden idle shells next to Active / All are apart, with a separator
// between them, in English and Japanese; the details panel names the scope
// as the tree does, the person who started the thread by their name (an
// id no person lists as it is), and the title written short as the tree
// and the tabs write it, whole in its tooltip.
func TestDashboardNamesAndWidths(t *testing.T) {
	rig := newNamesRig(t)
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1600, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.call("Page.navigate", map[string]any{"url": rig.link})

	t.Run("title column", func(t *testing.T) {
		b.t = t
		b.until("the full thread's row with every column", fmt.Sprintf(`(() => {
			const tb = document.querySelector('.pane.active .all-threads');
			return !!tb && tb.dataset.columns === 'state number title agent age context tokens branch changes unpushed last' &&
				!!tb.querySelector('tr[data-thread="%d"] td[data-col=title]') && !!document.querySelector('.tree') && !!document.querySelector('.details');
		})()`, rig.full))
		var w struct {
			Window       float64
			Text         string
			Fits         float64 // how many characters of the title show before the cell's edge
			Title, Other float64 // the Title cell's width, and the widest other text column's
			OtherCol     string
			Cols         string // every cell's width, for the failure's message
		}
		b.eval(fmt.Sprintf(`(() => {
			const row = document.querySelector('.pane.active .all-threads tr[data-thread="%d"]');
			const td = row.querySelector('td[data-col=title]');
			const cs = getComputedStyle(td);
			const edge = td.getBoundingClientRect().right - parseFloat(cs.paddingRight);
			const text = td.firstChild;
			let fits = 0;
			const r = document.createRange();
			for (let i = 1; i <= text.length; i++) {
				r.setStart(text, 0); r.setEnd(text, i);
				if (r.getBoundingClientRect().right > edge + 0.5) break;
				fits = i;
			}
			let other = 0, otherCol = '';
			for (const c of ['agent', 'branch', 'last']) {
				const w = row.querySelector('td[data-col=' + c + ']').getBoundingClientRect().width;
				if (w > other) { other = w; otherCol = c; }
			}
			const cols = [...row.children].map(c => c.dataset.col + ' ' + Math.round(c.getBoundingClientRect().width)).join(', ');
			return {window: innerWidth, text: td.textContent, fits, title: td.getBoundingClientRect().width, other, otherCol, cols};
		})()`, rig.full), &w)
		if w.Window != 1600 || w.Text != namesShort {
			t.Fatalf("the window is %v px and the title %q: the check would test another layout", w.Window, w.Text)
		}
		if w.Fits < 30 {
			t.Errorf("a title shows %v characters before it is cut, want at least 30: %+v", w.Fits, w)
		}
		if w.Title < w.Other {
			t.Errorf("the Title column is %v px, narrower than the %s column's %v px", w.Title, w.OtherCol, w.Other)
		}
	})

	t.Run("hidden counts", func(t *testing.T) {
		b.t = t
		for _, lang := range []struct{ code, sep, threads, shells string }{
			{"en", "·", "1 hidden", "2 idle shells hidden"},
			{"ja", "・", "1 件を非表示", "使われていないシェル 2 件を非表示"},
		} {
			b.eval(`window.__namesTestPrevious = true`, nil)
			b.call("Page.navigate", map[string]any{"url": rig.base + "/?lang=" + lang.code})
			b.until("the page in "+lang.code, `!window.__namesTestPrevious && document.documentElement.lang === `+js(lang.code))
			b.until("both counts in "+lang.code, `document.querySelector('.tools .show .hidden-count')?.dataset.hidden === '1' && document.querySelector('.tools .show .hidden-terminals')?.dataset.hiddenTerminals === '2'`)
			var got struct {
				Text, Threads, Shells string
				Gap                   float64
				Between               string
			}
			b.eval(`(() => {
				const show = document.querySelector('.tools .show');
				const a = show.querySelector('.hidden-count'), z = show.querySelector('.hidden-terminals');
				let between = '';
				for (let n = a.nextSibling; n && n !== z; n = n.nextSibling) between += n.textContent;
				return {text: show.textContent, threads: a.textContent, shells: z.textContent,
					gap: z.getBoundingClientRect().left - a.getBoundingClientRect().right, between};
			})()`, &got)
			if got.Threads != lang.threads || got.Shells != lang.shells {
				t.Fatalf("%s: the counts say %q and %q, want %q and %q", lang.code, got.Threads, got.Shells, lang.threads, lang.shells)
			}
			if strings.TrimSpace(got.Between) != lang.sep {
				t.Errorf("%s: between the counts is %q, want the separator %q (the group reads %q)", lang.code, got.Between, lang.sep, got.Text)
			}
			if got.Gap < 8 {
				t.Errorf("%s: the counts are %v px apart: they run together", lang.code, got.Gap)
			}
		}
		b.eval(`localStorage.removeItem('hataori.lang')`, nil)
	})

	t.Run("details", func(t *testing.T) {
		b.t = t
		b.eval(`window.__namesTestPrevious = true`, nil)
		b.call("Page.navigate", map[string]any{"url": rig.base + "/?lang=en"})
		b.until("the page again", `!window.__namesTestPrevious && document.readyState === 'complete'`)
		b.showAll()
		type field struct{ Text, Tip string }
		fields := func(id control.ThreadID) map[string]field {
			t.Helper()
			openThread(t, b, id)
			b.until("its details", fmt.Sprintf(`!!document.querySelector('.details [data-details="%d"]')`, id))
			var f map[string]field
			b.eval(`Object.fromEntries([...document.querySelectorAll('.details [data-block=thread] [data-field]')].map(e =>
				[e.dataset.field, {text: e.querySelector('dd').textContent, tip: e.querySelector('dd').title}]))`, &f)
			return f
		}
		f := fields(rig.full)
		if f["scope"].Text != "Builds and releases" {
			t.Errorf("the scope the config names shows as %q, want its name", f["scope"].Text)
		}
		if f["startedBy"].Text != "bob" {
			t.Errorf("a thread whose row records bob's chat id shows started by %q, want bob", f["startedBy"].Text)
		}
		if f["title"].Text != namesShort || f["title"].Tip != namesTitle {
			t.Errorf("the title shows %+v, want %q with the whole title in its tooltip", f["title"], namesShort)
		}
		var tab string
		b.eval(fmt.Sprintf(`document.querySelector('.tabbar .tab[data-tab="thread:%d"] .tab-title')?.textContent ?? ''`, rig.full), &tab)
		if !strings.Contains(tab, namesShort) {
			t.Errorf("the tab reads %q: the details' short title would match no tab", tab)
		}
		f = fields(rig.asleep)
		if f["scope"].Text != "#deploys" {
			t.Errorf("a scope named by its channel only shows as %q, want the channel's name", f["scope"].Text)
		}
		if f["startedBy"].Text != "stranger-9" {
			t.Errorf("an id no person lists shows as %q, want the id", f["startedBy"].Text)
		}
	})
}
