package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// namesRig is the daemon with the fake surface and the fake agent, a
// config with people, and two threads: one in a scope the config names,
// with every column of the All threads table, a title with an issue's
// address, and a row that records "owner", as an old-form config named its
// owner, while its start records bob's id; one asleep in a scope named by
// its channel only, started by an id no person lists; and more asleep there,
// one for each other form an older hataori recorded who started a thread
// in. Two idle shells of a fake list of host terminals are hidden, as the
// threads asleep are.
type namesRig struct {
	r            *chatRoot
	c            *wire.Client
	full, asleep control.ThreadID
	legacy       []legacyStarter
	base, link   string
}

// legacyStarter is a thread whose row records who started it as recorded,
// and the name the page should show for it.
type legacyStarter struct {
	id             control.ThreadID
	recorded, want string
}

// legacyStarters are the other forms of started_by, in threads started on
// the control socket, whose start records no id: the bare chat id of
// before callers were resolved to people, an old-form config's name of a
// poster ("<client>/<id>"), a full id, an old-form poster no person lists,
// and "owner", which says nothing of who that owner was. The ids are made
// up.
var legacyStarters = []legacyStarter{
	{recorded: e2eHook, want: "bob"},
	{recorded: people.OldPoster(fakeSurface, e2eHook), want: "bob"},
	{recorded: people.Join(fakeSurface, e2eHook), want: "bob"},
	{recorded: people.OldPoster(fakeSurface, "nobody-3"), want: people.OldPoster(fakeSurface, "nobody-3")},
	{recorded: people.OldOwner, want: people.OldOwner},
}

// namesTitle is the full thread's title: an issue's address, which the
// tree and the tabs write short as "widgets#631".
const (
	namesTitle = "Do https://github.com/example/widgets/issues/631 and make the title column wide enough to read"
	namesShort = "Do widgets#631 and make the title column wide enough to read"
	// The model and the branch are longer than the 12 characters a text
	// column keeps.
	namesModel  = "a-model-with-a-long-name"
	namesBranch = "feature/a-branch-with-a-long-name"
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
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" model:"+namesModel+" usage 1200 300 40000 5000 45000 200000\nreply "+strings.Repeat("the last line of the agent goes on ", 6))
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
	// Its workspace, the only one so far, on a branch whose name is longer
	// than the 12 characters a text column keeps.
	dirs, _ := filepath.Glob(filepath.Join(r.root, "state", "sessions", "*"))
	if len(dirs) != 1 {
		t.Fatalf("%d sessions, want the full thread's", len(dirs))
	}
	spec, ok, err := host.ReadSpec(dirs[0])
	if err != nil || !ok {
		t.Fatalf("the full thread's session: %v", err)
	}
	gitIn(t, spec.Dir, "checkout", "-q", "-b", namesBranch)
	asleep, err := c.Start(ctx, control.StartRequest{Key: "k-asleep", Scope: "c2", Repo: bareRepo(t), Input: control.Input{Text: "exit"}})
	if err != nil {
		t.Fatal(err)
	}
	legacy := slices.Clone(legacyStarters)
	for i := range legacy {
		if legacy[i].id, err = c.Start(ctx, control.StartRequest{Key: fmt.Sprintf("k-legacy-%d", i), Scope: "c2", Repo: bareRepo(t), Input: control.Input{Text: "exit"}}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the threads of the second scope asleep", 30*time.Second, func() bool {
		ids := []control.ThreadID{asleep}
		for _, l := range legacy {
			ids = append(ids, l.id)
		}
		for _, id := range ids {
			if d, err := c.Thread(ctx, id, 0, 0); err != nil || d.State != control.Asleep {
				return false
			}
		}
		return true
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
	// The rows as older daemons wrote them. The daemon is stopped
	// meanwhile, as it holds the store.
	c.Close()
	stop()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File))
	if err != nil {
		t.Fatal(err)
	}
	recorded := map[control.ThreadID]string{full: people.OldOwner, asleep: "stranger-9"}
	for _, l := range legacy {
		recorded[l.id] = l.recorded
	}
	for id, by := range recorded {
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
	return namesRig{r: r, c: c, full: full, asleep: asleep, legacy: legacy, base: u.Scheme + "://" + u.Host, link: link}
}

// The acceptance criteria of #631 in the browser, against the daemon with
// the fake surface: at 1600 px wide with every column shown and the tree,
// Details on or off, the All threads table's agent, branch and last output
// columns each show about 12 characters and the title gets what is left, at
// least 30 characters and no narrower than the other text columns, as the
// details panel takes no width while no thread is in front; the counts of hidden threads
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

	t.Run("text columns", func(t *testing.T) {
		b.t = t
		b.until("the full thread's row with every column", fmt.Sprintf(`(() => {
			const tb = document.querySelector('.pane.active .all-threads');
			return !!tb && tb.dataset.columns === 'state number title agent age context tokens branch changes unpushed last' &&
				!!tb.querySelector('tr[data-thread="%d"] td[data-col=title]') && !!document.querySelector('.tree');
		})()`, rig.full))
		// cell is one text cell: its text, how many of its characters show
		// before its edge, and its width.
		type cell struct {
			Text  string
			Fits  int
			Width float64
		}
		var w struct {
			Window, Pane, Scroll float64
			Details              bool
			Cells                map[string]cell
			Cols                 string // every cell's width, for a failure's message
		}
		measure := func() {
			t.Helper()
			b.eval(fmt.Sprintf(`(() => {
				const p = document.querySelector('.pane.active');
				const row = p.querySelector('.all-threads tr[data-thread="%d"]');
				const cells = {};
				for (const c of ['title', 'agent', 'branch', 'last']) {
					const td = row.querySelector('td[data-col=' + c + ']');
					// A cut value's ellipsis is painted over its end, inside the
					// edge: what shows is what ends before the ellipsis.
					const right = td.getBoundingClientRect().right - parseFloat(getComputedStyle(td).paddingRight);
					const text = td.firstChild;
					const r = document.createRange();
					const upTo = (edge) => {
						let n = 0;
						for (let i = 1; i <= text.length; i++) {
							r.setStart(text, 0); r.setEnd(text, i);
							if (r.getBoundingClientRect().right > edge + 0.5) break;
							n = i;
						}
						return n;
					};
					let fits = upTo(right);
					if (fits < text.length) {
						const e = document.createElement('span');
						e.textContent = '…';
						td.append(e);
						const ellipsis = e.getBoundingClientRect().width;
						e.remove();
						fits = upTo(right - ellipsis);
					}
					cells[c] = {text: td.textContent, fits, width: td.getBoundingClientRect().width};
				}
				const cols = [...row.children].map(c => c.dataset.col + ' ' + Math.round(c.getBoundingClientRect().width)).join(', ');
				return {window: innerWidth, pane: p.clientWidth, scroll: p.scrollWidth, details: !!document.querySelector('.details'), cells, cols};
			})()`, rig.full), &w)
			if w.Window != 1600 || w.Cells["title"].Text != namesShort {
				t.Fatalf("the window is %v px and the title %q: the check would test another layout", w.Window, w.Cells["title"].Text)
			}
		}
		// keeps checks that each text column but the title shows at least 12
		// characters (all of a shorter value), and that the table does not
		// scroll sideways.
		keeps := func(layout string) {
			t.Helper()
			for _, c := range []string{"agent", "branch", "last"} {
				v := w.Cells[c]
				if want := min(12, len([]rune(v.Text))); v.Fits < want {
					t.Errorf("%s: the %s column shows %d characters of %q, want at least %d (%s)", layout, c, v.Fits, v.Text, want, w.Cols)
				}
			}
			if w.Scroll > w.Pane {
				t.Errorf("%s: the table scrolls sideways: %v px in %v (%s)", layout, w.Scroll, w.Pane, w.Cols)
			}
		}
		// fits checks the table as it is laid out now: agent, branch and
		// last output keep about 12 characters, and the title gets what is
		// left, at least 30 characters and no narrower than any of them.
		fits := func(layout string) {
			t.Helper()
			measure()
			if w.Cells["agent"].Text != "fake · "+namesModel || w.Cells["branch"].Text != namesBranch {
				t.Fatalf("the agent and model read %q and the branch %q: the check would test short values", w.Cells["agent"].Text, w.Cells["branch"].Text)
			}
			t.Logf("%s: %s", layout, w.Cols)
			keeps(layout)
			for _, c := range []string{"agent", "branch", "last"} {
				if v := w.Cells[c]; v.Fits > 16 {
					t.Errorf("%s: the %s column shows %d characters, more than about 12, taking the title's room (%s)", layout, c, v.Fits, w.Cols)
				}
			}
			if v := w.Cells["title"]; v.Fits < 30 {
				t.Errorf("%s: a title shows %d characters before it is cut, want at least 30 (%s)", layout, v.Fits, w.Cols)
			}
			for _, c := range []string{"agent", "branch", "last"} {
				if w.Cells["title"].Width < w.Cells[c].Width {
					t.Errorf("%s: the Title column is %v px, narrower than the %s column's %v px", layout, w.Cells["title"].Width, c, w.Cells[c].Width)
				}
			}
		}
		const detailsOn = `document.querySelector('.tools [data-tool=details]')?.getAttribute('aria-pressed') === 'true'`
		// The default view: Details on, the All threads tab in front. The
		// panel would only say to open a thread: it takes no width.
		b.until("Details on", detailsOn)
		time.Sleep(300 * time.Millisecond) // the layout as it settles
		fits("Details on, the All threads tab in front")
		if w.Details {
			t.Errorf("Details on, the All threads tab in front: the details panel takes width, though it has no thread to show")
		}
		// A thread in front brings the panel back; the All threads tab in
		// front again takes it away, and Details stays on.
		openThread(t, b, rig.full)
		b.until("the panel beside the thread", detailsOn+fmt.Sprintf(` && !!document.querySelector('.details [data-details="%d"]')`, rig.full))
		b.click(`.tabbar .tab[data-tab=all] .tab-main`)
		b.until("no panel beside the All threads tab again", detailsOn+` && !!document.querySelector('.pane.active .all-threads') && !document.querySelector('.details')`)
		fits("Details on, the All threads tab in front again")
		// Details off: the same.
		b.click(`.tools [data-tool=details]`)
		b.until("Details off", `document.querySelector('.tools [data-tool=details]')?.getAttribute('aria-pressed') === 'false' && !document.querySelector('.details')`)
		fits("Details off")
		b.click(`.tools [data-tool=details]`)
		b.until("Details on again", detailsOn)
	})

	t.Run("hidden counts", func(t *testing.T) {
		b.t = t
		hidden := 1 + len(rig.legacy) // the threads asleep
		for _, lang := range []struct{ code, sep, threads, shells string }{
			{"en", "·", fmt.Sprintf("%d hidden", hidden), "2 idle shells hidden"},
			{"ja", "・", fmt.Sprintf("%d 件を非表示", hidden), "使われていないシェル 2 件を非表示"},
		} {
			b.eval(`window.__namesTestPrevious = true`, nil)
			b.call("Page.navigate", map[string]any{"url": rig.base + "/?lang=" + lang.code})
			b.until("the page in "+lang.code, `!window.__namesTestPrevious && document.documentElement.lang === `+js(lang.code))
			b.until("both counts in "+lang.code, `document.querySelector('.tools .show .hidden-count')?.dataset.hidden === '`+fmt.Sprint(hidden)+`' && document.querySelector('.tools .show [data-hidden-terminals]')?.dataset.hiddenTerminals === '2'`)
			var got struct{ Text, Title, Action string }
			b.eval(`(() => {
				const show = document.querySelector('.tools .show');
				return {text: show.querySelector('.hidden-count').textContent,
					title: show.querySelector('.hidden-count').title,
					action: show.querySelector('button').title};
			})()`, &got)
			want := lang.threads + lang.sep + lang.shells
			if got.Text != fmt.Sprint(hidden) || got.Title != want || !strings.Contains(got.Action, want) {
				t.Fatalf("%s: compact count %+v, want %d with tooltip %q", lang.code, got, hidden, want)
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
			t.Errorf("a thread whose row records %q and whose start records bob's id shows started by %q, want bob", people.OldOwner, f["startedBy"].Text)
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
		for _, l := range rig.legacy {
			if got := fields(l.id)["startedBy"].Text; got != l.want {
				t.Errorf("a thread whose row records %q shows started by %q, want %q", l.recorded, got, l.want)
			}
		}
	})
}
