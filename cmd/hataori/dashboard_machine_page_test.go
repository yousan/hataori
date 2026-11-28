package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/dashboard"
)

// machinePageAPI is an isolated scoped API fixture, not a worker or an
// agent-output capture. No daemon, tmux, SSH, login or existing root is used.
// The browser runs the committed page against the actual dashboard routes.
type machinePageAPI struct {
	control.API
	mu             sync.Mutex
	threads        []control.ThreadInfo
	streams        []chan control.Event
	pos            uint64
	cut            chan struct{}
	queries        chan struct{}
	workspaceError bool
	history        []control.Event
	historyError   bool
}

func (a *machinePageAPI) Threads(context.Context, control.Filter) ([]control.ThreadInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]control.ThreadInfo(nil), a.threads...), nil
}
func (a *machinePageAPI) Thread(ctx context.Context, id control.ThreadID, _, _ int) (control.ThreadDetail, error) {
	ts, _ := a.Threads(ctx, control.Filter{})
	for _, x := range ts {
		if x.ID == id {
			return control.ThreadDetail{ThreadInfo: x}, nil
		}
	}
	return control.ThreadDetail{}, core.ErrNotFound
}
func (*machinePageAPI) Scopes(context.Context) ([]control.ScopeInfo, error) {
	return []control.ScopeInfo{{Key: "visible", Name: "Visible scope"}}, nil
}
func (*machinePageAPI) Layout(context.Context) ([]control.ChatLayout, error)   { return nil, nil }
func (*machinePageAPI) Limits(context.Context) ([]control.AgentLimits, error)  { return nil, nil }
func (*machinePageAPI) Clients(context.Context) ([]control.ClientState, error) { return nil, nil }
func (*machinePageAPI) HostTerminals(context.Context) ([]control.HostTerminal, error) {
	return nil, control.ErrForbidden
}
func (a *machinePageAPI) ThreadEvents(_ context.Context, id control.ThreadID, before uint64, limit int) ([]control.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id != 1 {
		return nil, nil
	}
	if a.historyError {
		return nil, fmt.Errorf("fixture history unavailable")
	}
	var events []control.Event
	for _, e := range a.history {
		if before == 0 || e.Pos < before {
			events = append(events, e)
		}
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}
func (a *machinePageAPI) Workspace(ctx context.Context, id control.ThreadID) (control.WorkspaceState, error) {
	d, err := a.Thread(ctx, id, 0, 0)
	a.mu.Lock()
	failed := a.workspaceError
	a.mu.Unlock()
	if err != nil || failed || d.PlacementPending || (d.Availability != nil && d.Availability.State != "ready") {
		return control.WorkspaceState{}, fmt.Errorf("fixture workspace unavailable")
	}
	return control.WorkspaceState{Branch: "work", Added: 7, Removed: 2, Uncommitted: 3, Unpushed: 1}, nil
}
func (a *machinePageAPI) Terminal(ctx context.Context, id control.ThreadID, _ string, _ uint64) (<-chan control.TerminalChunk, error) {
	d, err := a.Thread(ctx, id, 0, 0)
	if err != nil || d.PlacementPending || d.Availability.State != "ready" {
		return nil, fmt.Errorf("fixture terminal unavailable")
	}
	out := make(chan control.TerminalChunk, 1)
	out <- control.TerminalChunk{Run: "fixture-run", Reset: true, Pos: 14, Cols: 80, Rows: 24, Data: []byte("fixture output")}
	go func() {
		defer close(out)
		select {
		case <-a.queries:
			select {
			case out <- control.TerminalChunk{Run: "fixture-run", Pos: 32, Cols: 80, Rows: 24, Data: []byte("\x1b[6n\x1b[cquery-guard")}:
			case <-ctx.Done():
				return
			case <-a.cut:
				return
			}
		case <-ctx.Done():
			return
		case <-a.cut:
			return
		}
		select {
		case <-ctx.Done():
		case <-a.cut:
		}
	}()
	return out, nil
}
func (a *machinePageAPI) Events(ctx context.Context, _ control.Filter, _ uint64) (<-chan control.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch := make(chan control.Event, 16)
	ch <- control.Event{Kind: control.CaughtUp, Pos: a.pos}
	a.streams = append(a.streams, ch)
	// The server owns cancellation. Normally channels stay open while a
	// canceled Strict Mode subscription is replaced; the reconnect test
	// explicitly closes and removes them under the same mutex as senders.
	return ch, nil
}
func (a *machinePageAPI) away() {
	a.mu.Lock()
	defer a.mu.Unlock()
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	a.threads[0].Availability = &control.MachineAvailability{State: "away", At: at.Add(time.Minute), Reason: "fixture connection lost", LastKnownState: control.Running, LastKnownAt: &at}
	a.pos++
	for _, ch := range a.streams {
		select {
		case ch <- control.Event{Kind: "machine_availability", Thread: 1, Scope: "visible", Pos: a.pos, Time: at}:
		default:
		}
	}
	close(a.cut)
}

func machinePage(t *testing.T, lang string, scripts ...string) (*machinePageAPI, *browser) {
	return machinePageFixture(t, lang, false, scripts...)
}

func machinePageFixture(t *testing.T, lang string, secure bool, scripts ...string) (*machinePageAPI, *browser) {
	t.Helper()
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	a := &machinePageAPI{cut: make(chan struct{}), queries: make(chan struct{}), threads: []control.ThreadInfo{
		{ID: 1, Scope: "visible", Machine: "worker-a", State: control.Running, Title: "Recorded machine", Created: at, Updated: at, Availability: &control.MachineAvailability{State: "ready", At: at}, Context: &control.Context{Used: 100, Size: 1000}},
		{ID: 2, Scope: "visible", PlacementPending: true, PlacementReason: "fixture capacity full", State: control.Creating, Title: "Waiting for placement", Created: at, Updated: at},
	}}
	clock := time.Now().Truncate(time.Second)
	cfg := dashboard.Config{Now: func() time.Time { return clock }, Local: &dashboard.Local{Listen: "127.0.0.1:0"}, MayAct: func(string) bool { return true }}
	way := control.WayLocal
	var flags []string
	if secure {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
			t.Fatal(err)
		}
		spki := writeNamedCert(t, root, "127.0.0.1")
		cert, err := tls.LoadX509KeyPair(filepath.Join(root, "config", "cert.pem"), filepath.Join(root, "config", "key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { opened.Close() })
		cfg.Root = opened
		cfg.Local = nil
		cfg.Web = &dashboard.Web{Listen: "127.0.0.1:0", TLS: &cert}
		way = control.WayWeb
		flags = []string{"--ignore-certificate-errors-spki-list=" + base64.StdEncoding.EncodeToString(spki)}
	}
	s, err := dashboard.Listen(cfg, func(person string) (control.API, bool) { return a, person == "owner" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// A web sign-in in the server's start second deliberately fails the
	// existing recent-auth check. Advance only this disposable fixture's
	// clock before serving and signing in, rather than weakening that check.
	clock = clock.Add(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); s.Close(); <-done })
	link, err := s.NewLink(ctx, "owner", way)
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, flags...)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__requests = []; const originalFetch = window.fetch; window.fetch = function(url, options) { window.__requests.push({url: String(url), method: options?.method ?? 'GET', body: options?.body ?? ''}); return originalFetch.apply(this, arguments); };`})
	for _, script := range scripts {
		b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": script})
	}
	b.call("Page.navigate", map[string]any{"url": strings.Replace(link, "#", "?lang="+lang+"#", 1)})
	b.until("both scoped fixture threads", `document.querySelectorAll('.tree [data-section=threads] .row').length === 2`)
	b.timeout = 20 * time.Second
	return a, b
}

func TestDashboardMachineViews(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		t.Run(lang, func(t *testing.T) {
			a, b := machinePage(t, lang)
			b.until("pending placement in tree", `!!document.querySelector('.tree [data-thread="2"] [data-placement-pending]')`)
			b.click(`.tree [data-thread="1"]`)
			b.until("ready workspace and terminal", `document.querySelector('.details [data-field=lines] dd')?.textContent.includes('7') && document.querySelector('[data-terminal="1"] .xterm-rows')?.textContent.includes('fixture output')`)
			a.away()
			for _, selector := range []string{`.tree [data-thread="1"]`, `.details [data-details="1"]`, `.status [data-selected="1"]`, `.pane-head`} {
				b.until("timestamped away and last-known state in "+selector, fmt.Sprintf(`(() => { const el = document.querySelector(%q); return el?.querySelector('[data-machine="worker-a"]') && el.querySelector('[data-availability="away"] time')?.dateTime === '2026-01-02T03:05:00Z' && el.querySelector('[data-last-known-state="running"]') && el.querySelector('[data-availability-reason]')?.textContent === 'fixture connection lost' && !el.querySelector('.state-running'); })()`, selector))
			}
			b.until("cached workspace retains counts and states its age", `document.querySelector('.details [data-workspace-unavailable]')?.textContent.includes('(') && document.querySelector('.details [data-field=lines] dd')?.textContent.includes('7') && document.querySelector('[data-output-unavailable]')?.textContent.includes('(')`)
			b.until("statistics age uses the last browser read, not the state timestamp", `(() => { const el = document.querySelector('.details [data-stats-cached]'); const at = el?.dataset.snapshotRead; return Date.parse(at) > Date.now() - 10000 && el.textContent.includes(new Date(at).toLocaleString(document.documentElement.lang, {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'})); })()`)
			b.until("away excluded from current running counts and tab", `!document.querySelector('.counts [data-count=running]') && !document.querySelector('.tab .state-running') && !document.querySelector('.tab .elapsed')`)
			b.click(`[data-tab=all] .tab-main`)
			b.until("away table row with retained cache", `document.querySelector('.all-threads [data-thread="1"] [data-availability="away"]') && document.querySelector('.all-threads [data-thread="1"] [data-workspace-unavailable]') && !document.querySelector('.all-threads .state-running')`)
			b.click(`.tree [data-thread="2"]`)
			for _, selector := range []string{`.tree [data-thread="2"]`, `.details [data-details="2"]`, `.status [data-selected="2"]`, `.pane-head`} {
				b.until("pending placement in "+selector, fmt.Sprintf(`document.querySelector(%q)?.querySelector('[data-placement-pending]')?.textContent.includes('fixture capacity full')`, selector))
			}
			b.until("pending workspace has no invented zero counts", `document.querySelector('.details [data-field=lines] dd')?.textContent === '—' && !!document.querySelector('.details [data-workspace-unavailable]')`)
			var requests []struct{ URL, Method, Body string }
			b.eval(`window.__requests`, &requests)
			for _, r := range requests {
				if strings.Contains(r.URL, "/api/terminal") && (r.Method != "GET" && r.Method != "HEAD" || r.Body != "") {
					t.Fatalf("passive terminal request sent input: %+v", r)
				}
			}
			var text string
			b.eval(`document.documentElement.lang + ' ' + document.body.textContent`, &text)
			want := "Pending placement"
			if lang == "ja" {
				want = "配置待ち"
			}
			if !strings.HasPrefix(text, lang+" ") || !strings.Contains(text, want) {
				t.Fatalf("wrong language: %s", text)
			}
		})
	}
}

// Away threads must also leave the phone's "running" section. Opening a
// thread that was already away cannot turn a refused HEAD into a no-terminal
// claim, and a cache that was never read must show unavailable, not zeros.
func TestDashboardMachineNarrow(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		t.Run(lang, func(t *testing.T) {
			a, b := machinePage(t, lang)
			a.away()
			b.phone()
			b.until("away and pending phone sections", `document.querySelector('[data-section=unavailable] [data-thread="1"] [data-availability=away]') && document.querySelector('[data-section=pending] [data-thread="2"] [data-placement-pending]') && !document.querySelector('[data-section=running] [data-thread="1"]')`)
			b.noSideScroll("machine list")
			b.click(`[data-thread="1"]`)
			b.until("unavailable phone details", `document.querySelector('[data-narrow-thread="1"] .pane-head [data-availability=away]') && document.querySelector('[data-narrow-thread="1"] [data-details="1"] [data-availability=away]') && document.querySelector('[data-workspace-unavailable]') && !document.querySelector('[data-narrow-thread="1"] .state-running')`)
			b.noSideScroll("away thread")
		})
	}
}

// The page's real Attachment state machine, with a deliberately disposable
// WebSocket transport. This proves browser queue/drop behaviour only; it is
// not evidence of worker routing, owner checks, SSH or runtime conformance.
func TestDashboardMachineAttachmentLoss(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		t.Run(lang, func(t *testing.T) {
			a, b := machinePage(t, lang, machineSocketFixture)
			b.click(`.tree [data-thread="1"]`)
			b.until("passive fixture output", `document.querySelector('[data-terminal="1"] .xterm-rows')?.textContent.includes('fixture output') && document.querySelector('[data-terminal="1"][data-typing=on]')`)
			// Deliver terminal queries only once input is enabled: delivering
			// them during startup's disabled stdin would not test the guard.
			close(a.queries)
			b.until("terminal queries were parsed while input was enabled", `document.querySelector('[data-terminal="1"] .xterm-rows')?.textContent.includes('query-guard')`)
			b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1200, "height": 800, "deviceScaleFactor": 1, "mobile": false})
			b.eval(`new Promise(done => setTimeout(done, 400))`, nil)
			b.until("viewing sent no keys or resize", `window.__sockets.length === 0 && window.__keys.length === 0 && window.__sizes.length === 0`)
			focusTerminal(b)
			b.key("x", "KeyX", 88, 0, "x")
			b.until("one unacknowledged attachment", `window.__sockets.length === 1 && document.querySelector('[data-attach=starting]')`)
			b.eval(`window.__sockets[0].cut(), true`, nil)
			b.until("lost attachment visible on loopback", `document.querySelector('[data-attach-end=lost]') && window.__keys.length === 0`)
			b.eval(`window.__holdAttach = false`, nil)
			focusTerminal(b)
			b.key("y", "KeyY", 89, 0, "y")
			b.until("only a fresh key reaches a new attachment", `window.__sockets.length === 2 && window.__keys.join('') === 'y' && document.querySelector('[data-attach=on]')`)
			a.away()
			b.until("availability ends the attachment and input", `document.querySelector('[data-attach-end=lost]') && document.querySelector('[data-terminal="1"][data-typing=off]') && window.__sockets[1].readyState === 3`)
			focusTerminal(b)
			b.key("z", "KeyZ", 90, 0, "z")
			b.until("no reconnect or replay while away", `window.__sockets.length === 2 && window.__keys.join('') === 'y'`)
		})
	}
}

// A disposable transport for frontend state-machine tests only.
const machineSocketFixture = `window.__sockets = []; window.__keys = []; window.__sizes = []; window.__holdAttach = true;
      window.WebSocket = class {
        static OPEN = 1;
        constructor(url) { this.url = url; this.readyState = 0; window.__sockets.push(this); setTimeout(() => { if (this.readyState !== 3) { this.readyState = 1; this.onopen?.(); } }, 0); }
        send(raw) { const m = JSON.parse(raw); if (m.keys) window.__keys.push(m.keys); if (m.resize) window.__sizes.push(m.resize); if (m.thread && !window.__holdAttach) setTimeout(() => this.onmessage?.({data: JSON.stringify({attached: {idle: 300}})}), 0); }
        cut() { this.readyState = 3; this.onclose?.({code: 1006, reason: ''}); }
        close() { if (this.readyState === 3) return; this.readyState = 3; this.onclose?.({code: 1000, reason: ''}); }
      };`

func TestDashboardMachineAwayWebSwitch(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		t.Run(lang, func(t *testing.T) {
			a, b := machinePageFixture(t, lang, true, machineSocketFixture)
			b.click(`.tree [data-thread="1"]`)
			b.until("web attachment switch", `document.querySelector('.pane [role=switch]') && document.querySelector('[data-terminal="1"] .xterm-screen')`)
			b.eval(`window.__holdAttach = false`, nil)
			b.click(`.pane [role=switch]`)
			b.until("web attachment on", `document.querySelector('[data-attach=on]') && window.__sockets.length === 1`)
			a.away()
			b.until("away disables initiation and retains loss explanation", `document.querySelector('.pane [role=switch]')?.disabled && document.querySelector('[data-attach-end=lost]') && window.__sockets[0].readyState === 3`)
			b.eval(`document.querySelector('.pane [role=switch]').click(); new Promise(done => setTimeout(done, 400))`, nil)
			b.until("away cannot create a replacement web attachment", `window.__sockets.length === 1 && window.__keys.length === 0`)
		})
	}
}

func TestDashboardMachineWorkspaceFailure(t *testing.T) {
	a, b := machinePage(t, "en")
	b.click(`.tree [data-thread="1"]`)
	b.until("successful workspace read", `document.querySelector('.details [data-field=lines] dd')?.textContent.includes('7')`)
	a.mu.Lock()
	a.workspaceError = true
	a.threads[0].Updated = a.threads[0].Updated.Add(time.Minute)
	a.pos++
	for _, ch := range a.streams {
		ch <- control.Event{Kind: "state", Thread: 1, Scope: "visible", Pos: a.pos, Time: a.threads[0].Updated}
	}
	a.mu.Unlock()
	b.until("failed read retains the successful snapshot with its time", `document.querySelector('.details [data-workspace-unavailable]')?.textContent.includes('cached') && document.querySelector('.details [data-field=lines] dd')?.textContent.includes('7')`)
}

func TestDashboardMachineConversationCache(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		t.Run(lang, func(t *testing.T) {
			a, b := machinePage(t, lang)
			a.mu.Lock()
			a.history = []control.Event{
				{Kind: "text", Thread: 1, Scope: "visible", Pos: 1, Time: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC), Data: []byte(`{"text":"fixture conversation output"}`)},
				{Kind: "tool_started", Thread: 1, Scope: "visible", Pos: 2, Time: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC), Data: []byte(`{"id":"fixture-tool","tool":"fixture tool"}`)},
			}
			a.mu.Unlock()
			b.click(`.tree [data-thread="1"]`)
			b.until("conversation switch", `!!document.querySelector('button[data-view=conversation]')`)
			b.click(`button[data-view=conversation]`)
			b.until("conversation history", `document.querySelector('.conv-text')?.textContent === 'fixture conversation output'`)
			a.away()
			b.until("retained conversation with receipt age", `(() => { const el = document.querySelector('.conversation [data-output-unavailable]'); return Date.parse(el?.dataset.historyObserved) > Date.now() - 10000 && el.textContent.includes('(') && document.querySelector('.conv-text')?.textContent === 'fixture conversation output'; })()`)
			b.until("tool completion remains unknown", fmt.Sprintf(`document.querySelector('.conv-tool-detail')?.textContent.includes(%q)`, map[string]string{"en": "completion not observed", "ja": "完了は未確認"}[lang]))
			b.click(`.tree [data-thread="2"]`)
			b.click(`button[data-view=conversation]`)
			b.until("empty pending conversation has no cached history", `(() => { const el = document.querySelector('[data-conversation="2"]'); return el?.querySelector('[data-output-unavailable]') && !el.querySelector('[data-output-unavailable]').dataset.historyObserved && !el.querySelector('.conv-text'); })()`)
		})
	}
}

// disconnectEvents ends only streams created by this disposable fixture.
// All fixture send/close operations hold mu, and the closed channels are
// removed before later Events or availability calls can use them.
func (a *machinePageAPI) disconnectEvents() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ch := range a.streams {
		close(ch)
	}
	a.streams = nil
}

func TestDashboardMachineConversationRecovery(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		t.Run(lang, func(t *testing.T) {
			a, b := machinePage(t, lang)
			a.mu.Lock()
			for i := 1; i <= 100; i++ {
				a.history = append(a.history, control.Event{Kind: "text", Thread: 1, Scope: "visible", Pos: uint64(i), Time: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC), Data: []byte(`{"text":"fixture recovery output"}`)})
			}
			a.history[99].Kind = "tool_started"
			a.history[99].Data = []byte(`{"id":"fixture-recovery-tool","tool":"fixture recovery tool"}`)
			a.mu.Unlock()
			b.click(`.tree [data-thread="1"]`)
			b.until("conversation switch", `!!document.querySelector('button[data-view=conversation]')`)
			b.click(`button[data-view=conversation]`)
			b.until("full history page loaded", `document.querySelectorAll('.conv-list > li').length === 100`)
			a.mu.Lock()
			a.historyError = true
			a.mu.Unlock()
			a.disconnectEvents()
			b.until("failed history catch-up retains output", `document.querySelector('.conversation [data-output-unavailable]')?.textContent.includes('(') && document.querySelectorAll('.conv-list > li').length === 100`)
			a.mu.Lock()
			a.historyError = false
			a.mu.Unlock()
			b.until("full overlapping history page recovers", fmt.Sprintf(`!document.querySelector('.conversation [data-output-unavailable]') && document.querySelector('.conv-tool-detail')?.textContent.includes(%q) && document.querySelectorAll('.conv-list > li').length === 100`, map[string]string{"en": "still running", "ja": "実行中"}[lang]))
		})
	}
}
