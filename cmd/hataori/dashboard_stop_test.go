package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// lastStarted is the turn number of the thread's last turn_started.
func lastStarted(t *testing.T, c *wire.Client, id control.ThreadID) int64 {
	t.Helper()
	var all []control.Event
	for before := uint64(0); ; {
		evs, err := c.ThreadEvents(t.Context(), id, before, control.MaxEvents)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			break
		}
		all = append(evs, all...)
		before = evs[0].Pos
	}
	s := dataOf(all, "turn_started")
	return int64(s[len(s)-1]["turn"].(float64))
}

// stopsOf are the data of the thread's stop events.
func stopsOf(t *testing.T, c *wire.Client, id control.ThreadID) []map[string]any {
	t.Helper()
	evs, err := c.ThreadEvents(t.Context(), id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	return dataOf(evs, "stop")
}

// The acceptance criteria of #612 in the browser, against the daemon with
// the fake surface and the fake agent on pipes: a running thread's pane has
// a Stop button in both views and in the narrow layout, and a thread that
// does not run has none; pressing it asks once, cancelling sends nothing,
// and confirming ends the turn as interrupted with one stop event that
// names the owner and via dashboard; confirmed after that turn has ended,
// it stops no later turn and shows the refusal. A person who may not act
// sees no Stop button.
func TestDashboardStopButton(t *testing.T) {
	r := boxRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	x := threadOf(t, r.root)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, x.ID)
	time.Sleep(500 * time.Millisecond)
	if n := countOf(b, ".pane.active [data-stop-button]"); n != 0 {
		t.Fatalf("%d Stop buttons on a thread that does not run", n)
	}

	running := func(what string) {
		t.Helper()
		r.driver.Inject(th, fakesurface.Webhook(e2eHook), "tool wait 60s\nreply late "+what)
		b.until("the Stop button of a running turn", `!!document.querySelector('.pane.active [data-stop-button]')`)
	}
	// Cancelled: nothing sent. The confirmation takes the focus, which
	// goes back to Stop, and names the running turn.
	running("one")
	b.click(".pane.active [data-stop-button]")
	b.until("the confirmation, with the focus", `!!document.querySelector('.pane.active [data-stop-confirm]') && document.activeElement?.dataset.stopNo === ''`)
	var named int64
	b.eval(`+document.querySelector('.pane.active [data-stop-confirm]').dataset.stopConfirm`, &named)
	if started := lastStarted(t, c, x.ID); named != started {
		t.Fatalf("the confirmation names turn %d, the running turn is %d", named, started)
	}
	b.click(".pane.active [data-stop-no]")
	b.until("the Stop button again, with the focus", `!!document.querySelector('.pane.active [data-stop-button]') && !document.querySelector('.pane.active [data-stop-confirm]') && document.activeElement?.dataset.stopButton === ''`)
	time.Sleep(300 * time.Millisecond)
	if s := stopsOf(t, c, x.ID); len(s) != 0 {
		t.Fatalf("a cancelled stop sent %v", s)
	}
	// Confirmed: the turn ends as interrupted, one stop event.
	b.click(".pane.active [data-stop-button]")
	b.click(".pane.active [data-stop-yes]")
	var ended []map[string]any
	eventually(t, "the stopped turn's end", 15*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		ended = dataOf(evs, "turn_ended")
		return len(ended) == 2
	})
	if ended[1]["reason"] != "interrupted" {
		t.Fatalf("the stopped turn ended %v", ended[1])
	}
	if s := stopsOf(t, c, x.ID); len(s) != 1 || s[0]["via"] != "dashboard" || s[0]["principal"] != "owner" {
		t.Fatalf("stop events %v, want one by the owner via dashboard", s)
	}
	b.until("no Stop button once it ended", `!document.querySelector('.pane.active [data-stop-button]')`)

	// In the conversation view too, and in the narrow layout.
	running("two")
	b.eval(`(() => { const v = document.querySelector('.pane.active .view-switch [data-view=conversation]'); if (v) v.click(); return true; })()`, nil)
	b.until("the Stop button in the conversation", `!!document.querySelector('.pane.active [data-conversation] ') && !!document.querySelector('.pane.active [data-stop-button]')`)
	// Confirmed after that turn ended and the next began: refused, and the
	// next turn goes on.
	b.click(".pane.active [data-stop-button]")
	b.until("the confirmation", `!!document.querySelector('.pane.active [data-stop-confirm]')`)
	var first int64
	b.eval(`+document.querySelector('.pane.active [data-stop-confirm]').dataset.stopConfirm`, &first)
	if err := c.Stop(context.Background(), x.ID, first); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second turn's end", 15*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		return len(dataOf(evs, "turn_ended")) == 3
	})
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "tool wait 60s\nreply late three")
	eventually(t, "the next turn running", 15*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		d, err := c.Thread(context.Background(), x.ID, 0, 0)
		return err == nil && d.State == control.Running && len(dataOf(evs, "turn_started")) == 4
	})
	b.click(".pane.active [data-stop-yes]")
	b.until("the refusal", `(document.querySelector('.pane.active [data-stop-refused]')?.textContent ?? '').includes('no longer running')`)
	time.Sleep(500 * time.Millisecond)
	if d, err := c.Thread(context.Background(), x.ID, 0, 0); err != nil || d.State != control.Running {
		t.Fatalf("the next turn is %v, %v: a stop of the ended turn stopped it", d.State, err)
	}
	if s := stopsOf(t, c, x.ID); len(s) != 2 {
		t.Fatalf("%d stop events, want the page's and the socket's only", len(s))
	}
	b.until("the refusal kept while that turn runs", `!!document.querySelector('.pane.active [data-stop-refused]')`)
	// A running turn whose start is more than 40 events back: stopped
	// all the same.
	if err := c.Stop(context.Background(), x.ID, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the third stopped turn's end", 15*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		return len(dataOf(evs, "turn_ended")) == 4
	})
	b.until("no Stop button, and the refusal gone, once that turn ended", `!document.querySelector('.pane.active [data-stop-button]') && !document.querySelector('.pane.active [data-stop-refused]')`)
	var script strings.Builder
	for i := range 30 {
		fmt.Fprintf(&script, "subagent s%d\n", i)
	}
	script.WriteString("tool wait 60s\nreply late four")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), script.String())
	// Read afresh once the turn's start is more than 40 events back, as
	// after opening the page.
	eventually(t, "the long turn's tool", 20*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		n := 0
		for i := len(evs) - 1; i >= 0 && evs[i].Kind != "turn_started"; i-- {
			n++
		}
		return n > 50 && len(dataOf(evs, "tool_started")) > 0 && dataOf(evs, "tool_started")[len(dataOf(evs, "tool_started"))-1]["tool"] == "wait"
	})
	b.call("Page.reload", map[string]any{})
	openThread(t, b, x.ID)
	b.eval(`(() => { const v = document.querySelector('.pane.active .view-switch [data-view=conversation]'); if (v) v.click(); return true; })()`, nil)
	b.until("the Stop button of a long turn", `!!document.querySelector('.pane.active [data-stop-button]')`)
	b.click(".pane.active [data-stop-button]")
	b.until("the confirmation", `!!document.querySelector('.pane.active [data-stop-confirm]')`)
	b.eval(`+document.querySelector('.pane.active [data-stop-confirm]').dataset.stopConfirm`, &named)
	if started := lastStarted(t, c, x.ID); named != started {
		t.Fatalf("the confirmation of a long turn names turn %d, the running turn is %d", named, started)
	}
	b.click(".pane.active [data-stop-yes]")
	eventually(t, "the long turn stopped", 15*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		e := dataOf(evs, "turn_ended")
		return len(e) == 5 && e[4]["reason"] == "interrupted"
	})
	running("five")

	b.phone()
	b.until("the narrow layout", `document.querySelector('.shell')?.dataset.layout === 'narrow'`)
	b.eval(fmt.Sprintf(`(() => { const r = document.querySelector('.narrow-list .row[data-thread="%d"]'); if (r) r.click(); return true; })()`, x.ID), nil)
	b.until("the Stop button in the narrow layout", fmt.Sprintf(`!!document.querySelector('[data-narrow-thread="%d"] [data-stop-button]')`, x.ID))

	// A person who may not act.
	dashboardHook = func(cl *control.Caller) { cl.Principal, cl.May = "grace", control.OpRead }
	t.Cleanup(func() { dashboardHook = nil })
	b2 := newBrowser(t)
	b2.call("Page.enable", map[string]any{})
	b2.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b2, x.ID)
	time.Sleep(time.Second)
	if n := countOf(b2, "[data-stop-button], [data-stop]"); n != 0 {
		t.Fatalf("a person who may not act sees %d Stop controls", n)
	}
}
