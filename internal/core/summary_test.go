package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

func (f *fixture) info(t *testing.T, api control.API, id control.ThreadID) control.ThreadInfo {
	t.Helper()
	ts, err := api.Threads(context.Background(), control.Filter{Thread: id})
	if err != nil || len(ts) != 1 {
		t.Fatalf("threads: %v, %v", ts, err)
	}
	return ts[0]
}

// Model and effort are the ones asked for until the agent reports its own.
func TestThreadInfoModelIsAskedUntilReported(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, err := f.owner.Start(ctx, control.StartRequest{Key: "k", Scope: "s", Thread: "t", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	got := f.info(t, f.owner, id)
	if got.Agent != "fake" || got.Model != "m-default" || got.Effort != "e-default" || got.Turns != 0 {
		t.Fatalf("before the agent reports: %+v", got)
	}
	sid, _, _ := f.core.current(ctx, f.core.Store.DB, id)
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady, Model: "reported-model", Effort: "reported-effort"})
	got = f.info(t, f.owner, id)
	if got.Model != "reported-model" || got.Effort != "reported-effort" {
		t.Fatalf("after the agent reports: %+v", got)
	}
}

// Turns counts the turns, background is the last turn_ended event's, and
// last is the last line of output, cut, and absent for a caller that may
// not read text.
func TestThreadInfoTurnsBackgroundAndLast(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	if got := f.info(t, f.owner, id); got.Turns != 1 {
		t.Fatalf("turns = %d, want 1", got.Turns)
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Text, Text: "first line\nsecond line\n\n  "})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed, Background: 2})
	got := f.info(t, f.owner, id)
	if got.Last != "second line" {
		t.Fatalf("last = %q", got.Last)
	}
	for i := range 25 { // blank messages do not hide the last line
		f.agentEvent(t, sid, uint64(10+i), agent.Event{Kind: agent.Text, Text: " \n"})
	}
	if got := f.info(t, f.owner, id); got.Last != "second line" {
		t.Fatalf("last after blank messages = %q", got.Last)
	}
	ended, err := f.core.Store.Last(ctx, id, 1, func(e control.Event) bool { return e.Kind == "turn_ended" })
	if err != nil || len(ended) != 1 {
		t.Fatalf("turn_ended: %v, %v", ended, err)
	}
	var e struct{ Background int }
	json.Unmarshal(ended[0].Data, &e)
	if e.Background != 2 || got.Background != e.Background {
		t.Fatalf("background = %d, the event's %d", got.Background, e.Background)
	}

	if err := f.owner.Send(ctx, id, "second", control.Input{Text: "again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 40, agent.Event{Kind: agent.TurnStarted})
	long := strings.Repeat("é", control.LastMax) // two bytes each
	f.agentEvent(t, sid, 41, agent.Event{Kind: agent.Text, Text: "x" + long})
	f.agentEvent(t, sid, 42, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	got = f.info(t, f.owner, id)
	if got.Turns != 2 || got.Background != 0 {
		t.Fatalf("after the second turn: %+v", got)
	}
	if len(got.Last) > control.LastMax || len(got.Last) < control.LastMax-1 || !utf8.ValidString(got.Last) || !strings.HasPrefix(got.Last, "xé") {
		t.Fatalf("last = %q (%d bytes)", got.Last, len(got.Last))
	}

	quiet := f.core.As(control.Caller{Principal: "plugin", Scopes: []string{"s"}, May: control.OpRead, NoText: true})
	if f.info(t, f.owner, id).Title == "" {
		t.Fatal("the owner gets no title")
	}
	if got := f.info(t, quiet, id); got.Last != "" || got.Title != "" || got.Turns != 2 || got.Model == "" {
		t.Fatalf("a caller that may not read text: %+v", got)
	}
	d, err := quiet.Thread(ctx, id, 100, 10)
	if err != nil || d.Last != "" || d.Title != "" || d.Recent != "" {
		t.Fatalf("a caller that may not read text: %+v, %v", d, err)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), `"title"`) {
		t.Fatalf("the title is encoded for a caller that may not read text: %s", b)
	}
	for _, e := range d.Events {
		if e.Kind == "text" && strings.Contains(string(e.Data), `"text"`) {
			t.Fatalf("a caller that may not read text got %s", e.Data)
		}
	}
}

// Thread with events returns the thread's last events, newest last, as
// Events delivers them; without it, none.
func TestThreadRecentEvents(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	f.running(t, "t2") // its events are not t1's
	for i := range 4 {
		f.agentEvent(t, sid, uint64(2+i), agent.Event{Kind: agent.Text, Text: "part"})
	}

	d, err := f.owner.Thread(ctx, id, 0, 0)
	if err != nil || d.Events != nil {
		t.Fatalf("without events: %v, %v", d.Events, err)
	}
	evs, err := f.owner.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var all []control.Event
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		all = append(all, e)
	}
	if len(all) < 6 {
		t.Fatalf("only %d events", len(all))
	}
	d, err = f.owner.Thread(ctx, id, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if want := all[len(all)-5:]; !reflect.DeepEqual(d.Events, want) {
		t.Fatalf("events:\n got %v\nwant %v", d.Events, want)
	}
	if d, _ := f.owner.Thread(ctx, id, 0, 1000); len(d.Events) != len(all) {
		t.Fatalf("%d events, want all %d", len(d.Events), len(all))
	}
}

// The terminal comes from the runner for a thread that may have a process.
// Hosts are asked at once, within one bound: hosts that do not answer
// delay a list by that bound, not by one bound each.
func TestThreadTerminalsAreAskedAtOnceWithinABound(t *testing.T) {
	f := newFixture(t, fakeOwner{})
	var ids []control.ThreadID
	var answering int64
	for i, key := range []string{"t1", "t2", "t3", "t4"} {
		id, sid := f.running(t, key)
		ids = append(ids, id)
		if i == 0 {
			answering = sid
		}
	}
	f.runner.device = func(ctx context.Context, sid int64) string {
		if sid == answering {
			return "/dev/pts/1"
		}
		<-ctx.Done() // a host that does not answer
		return ""
	}
	start := time.Now()
	ts, err := f.owner.Threads(context.Background(), control.Filter{})
	if err != nil || len(ts) != len(ids) {
		t.Fatalf("threads: %v, %v", ts, err)
	}
	if took := time.Since(start); took > terminalsTimeout+terminalsTimeout/2 {
		t.Fatalf("Threads took %v with three hosts not answering; the bound is %v", took, terminalsTimeout)
	}
	for _, ti := range ts {
		want := ""
		if ti.ID == ids[0] {
			want = "/dev/pts/1"
		}
		if ti.Terminal != want {
			t.Fatalf("thread %v: terminal %q, want %q", ti.ID, ti.Terminal, want)
		}
	}
	d, err := f.owner.Thread(context.Background(), ids[0], 0, 0)
	if err != nil || d.Terminal != "/dev/pts/1" {
		t.Fatalf("Thread: terminal %q (%v)", d.Terminal, err)
	}
}
