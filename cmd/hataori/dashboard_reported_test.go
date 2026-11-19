package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// A tmux row whose pane has a reported session shows its session id,
// directory, last event and that event's time, in the tree and in the
// details panel, which keeps its line that hataori does not judge the
// terminal's state; a row without one looks as before, and neither shows a
// state. Driven by the real server with a fixed host terminal list.
func TestDashboardReportedSession(t *testing.T) {
	at := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Millisecond)
	const (
		reported = "/dev/pts/931"
		plain    = "/dev/pts/932"
		session  = "00000000-0000-4000-8000-0000000000aa"
		dir      = "/home/alice/src/reported-dir"
	)
	hostTermsSource = fixedTerms{
		{Device: reported, Process: "claude", Activity: &at, Tmux: &control.TmuxPane{Pane: "%31", Session: "work", Window: "1", WindowName: "agent", Index: "0", Dir: dir},
			Reported: &control.ReportedSession{Window: "tmux:/tmp/tmux-1000/default,4242,%31", Agent: "claude-code", Session: session,
				Dir: dir, Event: "UserPromptSubmit", At: at}},
		{Device: plain, Process: "bash", Activity: &at, Tmux: &control.TmuxPane{Pane: "%32", Session: "work", Window: "2", WindowName: "shell", Index: "0"}},
	}
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
	link, err := c.SignInLink(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": link})
	b.unfoldHosts()
	row := func(dev string) string { return fmt.Sprintf(`.tree [data-host=%q]`, dev) }
	b.until("both panes in the tree", fmt.Sprintf(`!!document.querySelector('%s') && !!document.querySelector('%s')`, row(reported), row(plain)))

	// The tree: the reported row's own line, as the hooks reported it.
	var line struct {
		Session, Event, Dir, Time, Text string
	}
	b.eval(fmt.Sprintf(`(() => { const l = document.querySelector('%s [data-reported]');
		return l && {session: l.dataset.reported, event: l.querySelector('.reported-event').textContent,
			dir: l.querySelector('.reported-dir').textContent, time: l.querySelector('time').getAttribute('datetime'), text: l.textContent}; })()`, row(reported)), &line)
	if line.Session != session || line.Event != "UserPromptSubmit" || line.Dir != dir || line.Time != at.Format("2006-01-02T15:04:05.000Z") ||
		!strings.Contains(line.Text, session) || !strings.Contains(line.Text, "1m") {
		t.Fatalf("the reported row's line: %+v", line)
	}
	// A row without a report has its two lines only, as before.
	var plainLines []string
	b.eval(fmt.Sprintf(`[...document.querySelector('%s').children].map(e => e.className)`, row(plain)), &plainLines)
	if strings.Join(plainLines, ",") != "line1,line2" {
		t.Fatalf("the row without a report: %q", plainLines)
	}
	b.noStateFor(reported, "the reported row")
	b.noStateFor(plain, "the row without a report")

	// The details panel: the report, and the line that hataori does not
	// judge the terminal's state.
	b.click(row(reported))
	b.until("the pane's details", fmt.Sprintf(`!!document.querySelector('.details [data-host-details=%q] [data-block=reported]')`, reported))
	var details map[string]string
	b.eval(fmt.Sprintf(`Object.fromEntries([...document.querySelectorAll('.details [data-host-details=%q] [data-field]')].map(f => [f.dataset.field, (f.querySelector('dd') ?? f).textContent]))`, reported), &details)
	if details["reportedSession"] != session || details["reportedDir"] != dir || details["reportedEvent"] != "UserPromptSubmit" ||
		details["reportedAt"] != "1m" || !strings.Contains(details["note"], "does not judge its state") {
		t.Fatalf("the reported pane's details: %q", details)
	}
	b.noStateFor(reported, "the reported pane opened")
	b.click(row(plain))
	b.until("the other pane's details", fmt.Sprintf(`!!document.querySelector('.details [data-host-details=%q]')`, plain))
	var blocks int
	b.eval(fmt.Sprintf(`document.querySelectorAll('.details [data-host-details=%q] [data-block=reported]').length`, plain), &blocks)
	if blocks != 0 {
		t.Fatal("a pane without a report shows a reported block")
	}
}
