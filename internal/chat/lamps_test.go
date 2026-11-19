package chat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// lampEvents feeds turn events to the lamps of thread 1 and returns the
// rows they give, in order.
type lampEvents struct {
	t   *testing.T
	f   *front
	r   *reader
	pos uint64
}

func newLampEvents(t *testing.T) *lampEvents {
	f := newFront(t, workspaces{})
	return &lampEvents{t: t, f: f, r: &reader{lamp: map[control.ThreadID]*lampState{}, lampKnown: map[control.ThreadID]bool{}}}
}

var lampThread = surface.Conversation{Channel: channel, Thread: "t1"}

func (l *lampEvents) send(kind string, data map[string]any) []Row {
	l.t.Helper()
	l.pos++
	data["session"], data["turn"] = 1, int(l.pos)
	b, _ := json.Marshal(data)
	rows, err := l.f.fe.lamps(context.Background(), l.r, control.Event{Pos: l.pos, Thread: 1, Kind: control.EventKind(kind), Data: b},
		[]surface.Conversation{lampThread})
	if err != nil {
		l.t.Fatal(err)
	}
	return rows
}

func (l *lampEvents) sender(key string, s sender) {
	l.t.Helper()
	if err := l.f.fe.rememberSender(context.Background(), key, s); err != nil {
		l.t.Fatal(err)
	}
}

func pinged(rows []Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Message.Notify...)
	}
	return out
}

// A press that the core refused comes first: the turn of the answer it
// accepted pings who gave that answer.
func TestThePingOfAnAnswerGoesToWhoseAnswerWasTaken(t *testing.T) {
	l := newLampEvents(t)
	msg := surface.MessageRef{Conv: lampThread, ID: "m1"}
	l.sender("m1", sender{lamp: "message", msg: msg, ping: "ada"})
	l.send("turn_started", map[string]any{"by": "caller", "key": "m1", "from": "chat:ada"})
	l.send("turn_ended", map[string]any{"by": "caller", "outcome": "blocked", "background": 0})
	l.sender("answer:1:q1", sender{lamp: "answer"}) // recorded by a guest's press, refused
	l.send("turn_started", map[string]any{"by": "caller", "key": "answer:1:q1", "from": "chat:grace"})
	rows := l.send("turn_ended", map[string]any{"by": "caller", "outcome": "completion", "background": 0})
	if p := pinged(rows); len(p) != 1 || p[0] != "grace" {
		t.Fatalf("the answer's turn pinged %v, want the person whose answer was taken", p)
	}
	// A turn the agent then starts by itself, failing, pings that caller.
	l.send("turn_started", map[string]any{"by": "agent"})
	rows = l.send("turn_ended", map[string]any{"by": "agent", "outcome": "failure", "background": 0})
	if p := pinged(rows); len(p) != 1 || p[0] != "grace" {
		t.Fatalf("the agent's turn pinged %v, want the last caller", p)
	}
}

// Input this front end did not give, such as one from the control socket,
// lamps nothing and pings nobody.
func TestATurnOfInputFromElsewhereLeavesTheLampsAlone(t *testing.T) {
	l := newLampEvents(t)
	l.sender("m1", sender{lamp: "message", msg: surface.MessageRef{Conv: lampThread, ID: "m1"}, ping: "ada"})
	l.send("turn_started", map[string]any{"by": "caller", "key": "m1"})
	l.send("turn_ended", map[string]any{"by": "caller", "outcome": "completion", "background": 0})
	rows := append(l.send("turn_started", map[string]any{"by": "caller", "key": "elsewhere-1", "from": "chat:ada"}),
		l.send("turn_ended", map[string]any{"by": "caller", "outcome": "failure", "background": 0})...)
	if len(rows) != 0 {
		t.Fatalf("rows %+v, want none", rows)
	}
}
