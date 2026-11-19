package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// listedTerms is a host terminal list a test sets and changes: what the
// daemon lists the host's terminals from (hostTermsSource).
type listedTerms struct {
	mu   sync.Mutex
	list []control.HostTerminal
}

func (l *listedTerms) List(context.Context, []int) ([]control.HostTerminal, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list), nil
}

func (l *listedTerms) Screen(_ context.Context, pane string, _ []int) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range l.list {
		if h.Tmux != nil && h.Tmux.Pane == pane {
			return "screen of " + pane + "\n", true, nil
		}
	}
	return "", false, nil
}

func (l *listedTerms) View(ctx context.Context, pane string, owned []int) (control.HostView, bool, error) {
	screen, ok, err := l.Screen(ctx, pane, owned)
	return control.HostView{Screen: screen}, ok, err
}

// change changes the terminal on device.
func (l *listedTerms) change(device string, f func(h *control.HostTerminal)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.list {
		if l.list[i].Device == device {
			f(&l.list[i])
		}
	}
}

func ago(d time.Duration) *time.Time { at := time.Now().Add(-d); return &at }

// Idle shell panes on the host are hidden with asleep threads, behind the
// same switch, and counted with them; a pane that becomes active again
// shows at the next refresh, without a reload, and a pane with an open tab
// keeps it; a terminal outside tmux is never hidden. Driven by the real
// server with the fake surface, the fake agent and a fake host terminal
// list.
func TestDashboardIdleShellPanes(t *testing.T) {
	pane := func(device, id, process string, activity *time.Time) control.HostTerminal {
		return control.HostTerminal{Device: device, Process: process, Activity: activity,
			Tmux: &control.TmuxPane{Pane: id, Session: "work", Window: id[1:], WindowName: "w" + id[1:], Index: "0"}}
	}
	const (
		loginZsh = "/dev/pts/901" // a login shell, idle for three days: hidden
		bash     = "/dev/pts/902" // a shell, used an hour ago: shown
		editor   = "/dev/pts/903" // an editor, idle for three days: shown
		fish     = "/dev/pts/904" // a shell, idle for 30 hours: hidden
		outside  = "/dev/pts/905" // a shell outside tmux, idle for three days: shown
		unknown  = "/dev/pts/906" // a shell whose activity is not known: shown
	)
	terms := &listedTerms{list: []control.HostTerminal{
		pane(loginZsh, "%1", "-zsh", ago(72*time.Hour)),
		pane(bash, "%2", "bash", ago(time.Hour)),
		pane(editor, "%3", "vim", ago(72*time.Hour)),
		pane(fish, "%4", "fish", ago(30*time.Hour)),
		{Device: outside, Process: "bash", Activity: ago(72 * time.Hour)},
		pane(unknown, "%6", "sh", nil),
	}}
	// One idle pane for each other shell of the list: all hidden.
	var others []string
	for i, sh := range []string{"sh", "dash", "ksh", "mksh", "tcsh", "csh", "nu"} {
		dev := fmt.Sprintf("/dev/pts/%d", 911+i)
		terms.list = append(terms.list, pane(dev, fmt.Sprintf("%%%d", 11+i), sh, ago(25*time.Hour)))
		others = append(others, dev)
	}
	all := func(devs ...string) []string { return slices.Concat(devs, others) }
	hostTermsSource = terms
	t.Cleanup(func() { hostTermsSource = nil })
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["host_terminals"] = true
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	runDaemon(t, r.root)
	r.connected(t)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	// One asleep thread, hidden too.
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "c1", Repo: bareRepo(t), Input: control.Input{Text: "reply asleep\nexit"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the asleep thread", 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Asleep })

	b := newBrowser(t)
	// The browser's clock is two days ahead of the host's: the panes' times
	// are the host's, and so must be the time they are measured from.
	b.call("Page.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `(() => {
		const ahead = 48 * 3600 * 1000, D = Date;
		function Shifted(...a) {
			if (!new.target) return new D(D.now() + ahead).toString();
			return a.length ? new D(...a) : new D(D.now() + ahead);
		}
		Shifted.prototype = D.prototype;
		Object.setPrototypeOf(Shifted, D);
		Shifted.now = () => D.now() + ahead;
		window.Date = Shifted;
	})()`})
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.unfoldHosts()
	b.eval(`window.__samePage = true`, nil)
	// shown is what the tree and the switch show: the host rows' devices,
	// sorted, and the number hidden ("" when none is shown).
	type shown struct {
		Rows   []string
		Hidden string
	}
	untilShown := func(what, scope string, rows []string, hidden string) {
		t.Helper()
		var s shown
		want := slices.Sorted(slices.Values(rows))
		for deadline := time.Now().Add(25 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			b.eval(fmt.Sprintf(`(() => {
				const h = document.querySelector('%[1]s .show .hidden-count');
				return {rows: [...document.querySelectorAll('%[2]s [data-host]')].map(r => r.dataset.host).sort(), hidden: h ? h.dataset.hidden : ''};
			})()`, map[bool]string{true: ".narrow-list", false: ".tools"}[scope == ".narrow-list"], scope), &s)
			if slices.Equal(s.Rows, want) && s.Hidden == hidden {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the page shows %+v, want %v and %q hidden", what, s, want, hidden)
			}
		}
	}
	tree := ".tree [data-node=host]"

	// 1. By default: the idle shell panes are hidden, and counted with the
	// asleep thread; the terminal outside tmux and the pane of unknown
	// activity are shown.
	b.until("the browser's clock ahead", `Date.now() - performance.timeOrigin - performance.now() > 47 * 3600 * 1000`)
	untilShown("the idle shell panes hidden", tree, []string{bash, editor, outside, unknown}, "10")

	// 2. The switch shows them, and hides them again.
	b.click(`.tools .show [data-choice=all]`)
	untilShown("all", tree, all(loginZsh, bash, editor, fish, outside, unknown), "")
	b.click(`.tools .show [data-choice=active]`)
	untilShown("active again", tree, []string{bash, editor, outside, unknown}, "10")

	// 3. Without a reload: a pane with an open tab becomes idle and leaves
	// the tree, keeping its tab and its screen; an idle pane becomes active
	// again and comes back; another runs a command that is not a shell and
	// comes back too.
	b.click(fmt.Sprintf(`%s [data-host=%q]`, tree, bash))
	tab := fmt.Sprintf(`.tabbar .tab[data-tab="host:%s#%%2"]`, bash)
	b.until("the pane's tab with its screen", fmt.Sprintf(`!!document.querySelector('%s') && [...document.querySelectorAll('.pane.active [data-host-screen="%%2"] .xterm-rows > div')].some(d => d.textContent.includes('screen of %%2'))`, tab))
	terms.change(bash, func(h *control.HostTerminal) { h.Activity = ago(48 * time.Hour) })
	untilShown("the pane that became idle", tree, []string{editor, outside, unknown}, "11")
	b.until("its tab and its screen kept", fmt.Sprintf(`!!document.querySelector('%s') && [...document.querySelectorAll('.pane.active [data-host-screen="%%2"] .xterm-rows > div')].some(d => d.textContent.includes('screen of %%2'))`, tab))
	terms.change(loginZsh, func(h *control.HostTerminal) { h.Activity = ago(time.Minute) })
	terms.change(fish, func(h *control.HostTerminal) { h.Process = "make" })
	untilShown("the panes active again", tree, []string{loginZsh, editor, fish, outside, unknown}, "9")
	var same bool
	b.eval(`window.__samePage === true`, &same)
	if !same {
		t.Fatal("the page was reloaded")
	}

	// 4. The narrow list hides and counts the same.
	b.phone()
	b.until("the narrow list", `document.querySelector('.shell')?.dataset.layout === 'narrow'`)
	untilShown("the narrow list", ".narrow-list", []string{loginZsh, editor, fish, outside, unknown}, "9")
	b.click(`.narrow-list .show [data-choice=all]`)
	untilShown("the narrow list, all", ".narrow-list", all(loginZsh, bash, editor, fish, outside, unknown), "")
}
