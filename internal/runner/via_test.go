package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

// dashboard is the owner's API as a dashboard session has it: the daemon
// authenticated it, so it has no From, and its input says it came through
// the dashboard (#604).
func dashboard(f *fixture) control.API {
	return f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, Via: "dashboard"})
}

// eventPos is the position of each event of kind, in order.
func (f *fixture) eventPos(kind string) []int64 {
	f.t.Helper()
	rows, err := f.st.DB.Query(`SELECT pos FROM events WHERE kind = ? ORDER BY pos`, kind)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var p int64
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

// The input and answer events of a caller with Via name it, and those of a
// relayed caller and of the control socket do not; a Stop that interrupts
// a turn writes one stop event with who asked and the way, before the
// turn's end, and a Stop that interrupts nothing, or an interrupt no Stop
// asked for, writes none (#604).
func TestInputsAndStopsNameTheirWay(t *testing.T) {
	f := newFixture(t, options{})
	ctx := context.Background()
	_, socket := f.serveWire()
	f.start("reply relayed") // the owner, relayed from chat
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	if err := dashboard(f).Send(ctx, thread, "k-dash", control.Input{Text: "reply from the dashboard"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the dashboard's turn", 15*time.Second, f.has("turn_ended", 2))
	if err := socket.Send(ctx, thread, "k-socket", control.Input{Text: "reply from the socket"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the socket's turn", 15*time.Second, f.has("turn_ended", 3))
	in := f.eventData("input")
	if len(in) != 3 {
		t.Fatalf("%d input events: %v", len(in), in)
	}
	if _, ok := in[0]["via"]; ok || in[0]["from"] != ownerFrom {
		t.Fatalf("the relayed input: %v, want no via", in[0])
	}
	if in[1]["via"] != "dashboard" || in[1]["principal"] != "owner" || in[1]["from"] != nil {
		t.Fatalf("the dashboard's input: %v, want via dashboard, the owner and no from", in[1])
	}
	if _, ok := in[2]["via"]; ok {
		t.Fatalf("the control socket's input: %v, want no via", in[2])
	}

	// An answer.
	if err := owner(f.core).Send(ctx, thread, "k-ask", control.Input{Text: "ask which? -- red | blue"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the question", 15*time.Second, f.has("request", 1))
	d, err := owner(f.core).Thread(ctx, thread, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("the open question: %v %v", d.Requests, err)
	}
	if err := dashboard(f).Answer(ctx, d.Requests[0].ID, control.Answer{Choices: []string{d.Requests[0].Choices[1].Token}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the answer", 15*time.Second, f.has("answer", 1))
	if a := f.eventData("answer"); a[0]["via"] != "dashboard" || a[0]["principal"] != "owner" {
		t.Fatalf("the dashboard's answer: %v, want via dashboard", a[0])
	}
	waitFor(t, "the answered turn", 15*time.Second, f.has("turn_ended", 5))

	// A Stop of a turn that is not running, and one while no turn runs,
	// stop nothing and write nothing.
	if err := owner(f.core).Send(ctx, thread, "k-long", control.Input{Text: "tool wait 60s\nreply late"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the tool", 15*time.Second, f.has("tool_started", 1))
	var turn int64
	f.st.DB.QueryRow(`SELECT id FROM turns WHERE ended_at IS NULL`).Scan(&turn)
	if err := dashboard(f).Stop(ctx, thread, turn+1); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("a stop of a turn that is not running: %v", err)
	}
	if n := len(f.eventData("stop")); n != 0 {
		t.Fatalf("%d stop events after a refused stop", n)
	}
	// The dashboard's Stop: one event, before the turn's end.
	if err := dashboard(f).Stop(ctx, thread, turn); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stopped turn's end", 10*time.Second, f.has("turn_ended", 6))
	stops := f.eventData("stop")
	if len(stops) != 1 {
		t.Fatalf("%d stop events, want 1: %v", len(stops), stops)
	}
	if s := stops[0]; s["via"] != "dashboard" || s["principal"] != "owner" || s["from"] != nil || s["turn"] != float64(turn) || s["session"] == nil {
		t.Fatalf("the stop event %v, want turn %d, the owner and via dashboard", s, turn)
	}
	if sp, ends := f.eventPos("stop")[0], f.eventPos("turn_ended"); sp > ends[len(ends)-1] {
		t.Fatalf("the stop event at %d comes after the turn's end at %d", sp, ends[len(ends)-1])
	}
	if err := dashboard(f).Stop(ctx, thread, 0); err != nil {
		t.Fatalf("a stop while nothing runs: %v", err)
	}

	// A relayed Stop names where it came from; an interrupt that a Send
	// asked for is no one's stop.
	if err := owner(f.core).Send(ctx, thread, "k-long2", control.Input{Text: "tool wait 60s\nreply late"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second tool", 15*time.Second, f.has("tool_started", 2))
	if err := owner(f.core).Stop(ctx, thread, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second stopped turn's end", 10*time.Second, f.has("turn_ended", 7))
	if err := owner(f.core).Send(ctx, thread, "k-long3", control.Input{Text: "tool wait 60s\nreply late"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the third tool", 15*time.Second, f.has("tool_started", 3))
	if err := owner(f.core).Send(ctx, thread, "k-int", control.Input{Text: "reply instead"}, control.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the interrupted turn's end", 10*time.Second, f.has("turn_ended", 8))
	stops = f.eventData("stop")
	if len(stops) != 2 || stops[1]["from"] != ownerFrom || stops[1]["via"] != nil {
		t.Fatalf("stop events %v, want a second one from %s with no via, and none for the interrupting send", stops, ownerFrom)
	}
}

// An asleep thread woken by a caller with Via keeps it in its input event.
func TestWakingInputNamesItsWay(t *testing.T) {
	f := newFixture(t, options{idlePeriod: time.Second})
	ctx := context.Background()
	f.start("reply first")
	waitFor(t, "the session asleep", 20*time.Second, func() bool {
		var s string
		f.st.DB.QueryRow(`SELECT state FROM sessions`).Scan(&s)
		return s == string(control.Asleep)
	})
	if err := dashboard(f).Send(ctx, thread, "k-wake", control.Input{Text: "reply awake"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the woken turn", 20*time.Second, f.has("turn_ended", 2))
	in := f.eventData("input")
	if len(in) != 2 || in[1]["via"] != "dashboard" {
		t.Fatalf("input events %v, want the woken one with via dashboard", in)
	}
}

// A refusal of a caller with Via names it in its log line.
func TestRefusalNamesTheWay(t *testing.T) {
	f := newFixture(t, options{})
	ctx := context.Background()
	f.start("ask which? -- red | blue")
	waitFor(t, "the question", 15*time.Second, f.has("request", 1))
	d, err := owner(f.core).Thread(ctx, thread, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("the open question: %v %v", d.Requests, err)
	}
	stranger := f.core.As(control.Caller{Principal: "stranger", Scopes: []string{"s"}, May: all, Via: "dashboard"})
	calls := map[string]error{
		"Send":   stranger.Send(ctx, thread, "k", control.Input{Text: "reply no"}, control.Deliver),
		"Stop":   stranger.Stop(ctx, thread, 0),
		"Answer": stranger.Answer(ctx, d.Requests[0].ID, control.Answer{Text: "red"}),
	}
	_, calls["AttachTerminal"] = stranger.AttachTerminal(ctx, thread)
	for op, err := range calls {
		if !errors.Is(err, control.ErrForbidden) {
			t.Errorf("%s by a person who may not act: %v, want forbidden", op, err)
		}
	}
	for _, op := range []string{"Send", "Stop", "Answer", "AttachTerminal"} {
		found := false
		for _, l := range strings.Split(f.log.String(), "\n") {
			if strings.Contains(l, `msg="control call refused"`) && strings.Contains(l, "op="+op+" ") {
				found = true
				if !strings.Contains(l, "via=dashboard") {
					t.Errorf("the refusal of %s does not name the way: %s", op, l)
				}
			}
		}
		if !found {
			t.Errorf("no refusal of %s logged:\n%s", op, f.log.String())
		}
	}
}
