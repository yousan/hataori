package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #453: a Stop button on a quiet turn's
// progress line.

// quickLines makes the daemon's front ends show a progress line after a
// short quiet stretch, and returns the front end it last made.
func quickLines(t *testing.T) func() *chat.FrontEnd {
	var fe *chat.FrontEnd
	frontEndHook = func(f *chat.FrontEnd) {
		f.QuietAfter, f.EditEvery, f.Tick = 300*time.Millisecond, time.Hour, 20*time.Millisecond
		fe = f
	}
	t.Cleanup(func() { frontEndHook = nil })
	return func() *chat.FrontEnd { return fe }
}

// stopLine waits for the progress line in th with its Stop choice.
func stopLine(t *testing.T, d *fakesurface.Driver, th surface.Conversation) (fakesurface.Call, string) {
	t.Helper()
	var line fakesurface.Call
	eventually(t, "the progress line with its Stop button", 15*time.Second, func() bool {
		for _, c := range d.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 1 && c.Message.Choices[0].Label == "Stop" {
				line = c
				return true
			}
		}
		return false
	})
	return line, line.Message.Choices[0].Token
}

// turnEnds waits up to within for the thread's n-th turn end.
func (r *chatRoot) turnEnds(t *testing.T, th surface.Conversation, n int, within time.Duration) map[string]any {
	t.Helper()
	var e []map[string]any
	eventually(t, "the turn's end", within, func() bool { e = r.events(t, "turn_ended", th); return len(e) >= n })
	return e[n-1]
}

// stillRunning fails if the thread ended a turn beyond its first n.
func (r *chatRoot) stillRunning(t *testing.T, th surface.Conversation, n int) {
	t.Helper()
	time.Sleep(time.Second)
	if e := r.events(t, "turn_ended", th); len(e) != n {
		t.Fatalf("turn ends %v, want %d", e, n)
	}
}

func TestStopButtonStopsAQuietTurn(t *testing.T) {
	quickLines(t)
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
	th := r.thread(t, 0)
	line, token := stopLine(t, r.driver, th)
	start := time.Now()
	if err := r.driver.Press(line.Ref, e2eOwner, token); err != nil {
		t.Fatal(err)
	}
	if e := r.turnEnds(t, th, 1, 10*time.Second); e["reason"] != "interrupted" {
		t.Fatalf("the turn ended %v", e)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the turn ended %v after the press", took)
	}
	// The line lost its button, then went with the turn.
	eventually(t, "the line deleted", 15*time.Second, func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpDelete && c.Ref == line.Ref {
				return true
			}
		}
		return false
	})
	var edits []fakesurface.Call
	for _, c := range r.driver.Calls(th) {
		if c.Op == fakesurface.OpEdit && c.Ref == line.Ref {
			edits = append(edits, c)
		}
	}
	if len(edits) == 0 || len(edits[len(edits)-1].Message.Choices) != 0 || !strings.Contains(edits[len(edits)-1].Message.Text, "Stopping") {
		t.Fatalf("the line's edits %+v, want the last one without the button", edits)
	}
}

func TestStopButtonOfAGuestOrAnOutsiderStopsNothing(t *testing.T) {
	quickLines(t)
	r := linkRoot(t, false) // ada the owner, grace in the scope but not the hook
	startDaemonLog := startDaemonLog(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Person(e2eOwner), "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
	th := r.thread(t, 0)
	line, token := stopLine(t, r.driver, th)
	for _, who := range []string{"x-9", "z-7"} {
		before := strings.Count(startDaemonLog(), "id="+who)
		calls := len(r.driver.Calls(th))
		if err := r.driver.Press(line.Ref, who, token); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the refusal in the log", 5*time.Second, func() bool { return strings.Count(startDaemonLog(), "id="+who) > before })
		r.stillRunning(t, th, 0)
		if n := strings.Count(startDaemonLog(), "id="+who) - before; n != 1 {
			t.Fatalf("%s: %d log lines with the id, want 1", who, n)
		}
		if n := len(r.driver.Calls(th)); n != calls {
			t.Fatalf("%s's press changed the thread: %+v", who, r.driver.Calls(th)[calls:])
		}
	}
}

func TestStopButtonOfAnEndedTurnStopsNothing(t *testing.T) {
	fe := quickLines(t)
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" tool wait 1500ms\nreply first")
	th := r.thread(t, 0)
	line, token := stopLine(t, r.driver, th)
	r.posted(t, th, "first")
	r.turnEnds(t, th, 1, 15*time.Second)
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "tool wait 60s\nreply never")
	eventually(t, "the second turn", 15*time.Second, func() bool { return len(r.events(t, "tool_started", th)) == 2 })
	// The first turn's button, as a client with an old view still has it.
	fe().Choice(surface.Selection{Ref: line.Ref, Author: fakesurface.Person(e2eOwner), Token: token, Tokens: []string{token}})
	r.stillRunning(t, th, 1)
}

func TestStopButtonShownBeforeARestartStopsItsTurn(t *testing.T) {
	quickLines(t)
	r := newChatRoot(t, nil)
	stop, _ := runDaemonLog(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
	th := r.thread(t, 0)
	line, token := stopLine(t, r.driver, th)
	stop()
	runDaemonLog(t, r.root)
	r.connected(t)
	if err := r.driver.Press(line.Ref, e2eOwner, token); err != nil {
		t.Fatal(err)
	}
	if e := r.turnEnds(t, th, 1, 10*time.Second); e["reason"] != "interrupted" {
		t.Fatalf("the turn ended %v", e)
	}
}
