package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// Lamps and pings (design, sections 4.2 and 4.4). The message that started
// a turn shows "working" while it runs, and "done", "error" or "waiting"
// when it ends; the sender is pinged once when it ends. Both come from the
// turn_started and turn_ended events, so they cannot disagree with them,
// and both are decorations: dropped after a few failures, never holding a
// reply back.
//
// A turn the agent started by itself lamps the message of the last turn a
// caller started, and pings that sender only when it ends blocked or
// failed (#248); so does every turn in a supervisor's thread (#639). A message held until the running turn ends gets its own
// lamp when its turn starts. A turn typed at the terminal gets neither.

// The parts of a turn event's lamp and ping rows, far from the parts of
// what the event shows otherwise.
const (
	lampPart   = 1 << 29
	formerPart = lampPart + 1 // the former message's lamp, when a new one takes over
	pingPart   = lampPart + 2
)

// sender is what the front end recorded about the input under one key.
type sender struct {
	lamp string             // "message", "prompt" or "none"
	msg  surface.MessageRef // for "message"
	ping string             // the service's id of the sender; "" for none
}

// lit is a message with a lamp: a message the service holds, or the
// thread's prompt, which a row of this front end posted.
type lit struct {
	conv surface.Conversation
	on   *surface.MessageRef
	row  *RowKey
}

func (l *lit) same(o *lit) bool {
	switch {
	case l == nil || o == nil:
		return l == o
	case l.on != nil && o.on != nil:
		return *l.on == *o.on
	case l.row != nil && o.row != nil:
		return *l.row == *o.row
	}
	return false
}

// lampState is a thread's lamps as its events tell them.
type lampState struct {
	last     *lit           // the message of the last turn a caller started
	lastPing string         // who that turn pinged: its caller
	shown    surface.Status // what last shows
	turn     *lit           // the open turn's message; nil for none
	ping     string         // who the open turn pings at its end
	byself   bool           // the agent started the open turn
}

// rememberSender records who gave input under key, before it is given.
func (f *FrontEnd) rememberSender(ctx context.Context, key string, s sender) error {
	_, err := f.Outbox.Store.DB.ExecContext(ctx, `INSERT INTO chat_senders (client, key, lamp, channel, thread, message, ping)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		f.Name, key, s.lamp, s.msg.Conv.Channel, s.msg.Conv.Thread, s.msg.ID, s.ping)
	return err
}

// pingOf is the id to ping for author: a person's; nobody for a webhook.
func pingOf(a surface.Author) string {
	if a.Kind == surface.Person {
		return a.ID
	}
	return ""
}

func (f *FrontEnd) senderOf(ctx context.Context, key string) (sender, bool, error) {
	var s sender
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT lamp, channel, thread, message, ping FROM chat_senders WHERE client = ? AND key = ?`,
		f.Name, key).Scan(&s.lamp, &s.msg.Conv.Channel, &s.msg.Conv.Thread, &s.msg.ID, &s.ping)
	if errors.Is(err, sql.ErrNoRows) {
		return sender{}, false, nil
	}
	return s, err == nil, err
}

// lamps returns the lamp and ping rows of e, a turn's start or end, and
// keeps the thread's lamp state. convs are the thread's conversations here.
func (f *FrontEnd) lamps(ctx context.Context, r *reader, e control.Event, convs []surface.Conversation) ([]Row, error) {
	if e.Kind != "turn_started" && e.Kind != "turn_ended" {
		return nil, nil
	}
	var d struct {
		By, Key, Outcome, From, Reason string
		Turn                           int64
	}
	json.Unmarshal(e.Data, &d)
	st := r.lamp[e.Thread]
	if st == nil {
		st = &lampState{}
		r.lamp[e.Thread] = st
	}
	lamp := func(l *lit, part int, s surface.Status) Row {
		row := Row{Event: e.Pos, Part: part, Conv: l.conv, Decoration: true, Op: Lamp, Status: s}
		if l.on != nil {
			on := *l.on
			row.On = &on
		} else {
			row.Target = l.row
		}
		return row
	}
	if e.Kind == "turn_started" {
		st.turn, st.ping, st.byself = nil, "", false
		var rows []Row
		switch d.By {
		case "caller":
			s, ok, err := f.senderOf(ctx, d.Key)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, nil // input this front end did not give: no lamp of its own, none of ours
			}
			ping := s.ping
			if s.lamp == "answer" {
				// A press: whoever's answer the core accepted, the turn's
				// own from; a press is always a person's.
				ping = ""
				if client, id, ok := strings.Cut(d.From, ":"); ok && client == f.Name {
					ping = id
				}
			}
			l, err := f.litOf(ctx, r, e.Thread, s, convs)
			if err != nil {
				return nil, err
			}
			if l == nil {
				l = st.last // an answer by a button: the question's turn's message
			}
			st.lastPing = ping
			if l == nil {
				st.ping = ping
				return nil, nil
			}
			if st.last != nil && !st.last.same(l) && st.shown == surface.Waiting {
				// A typed answer took over from the message that waited.
				rows = append(rows, lamp(st.last, formerPart, surface.Done))
			}
			st.last, st.turn, st.ping = l, l, ping
		case "agent":
			if st.last == nil {
				return nil, nil
			}
			st.turn, st.ping, st.byself = st.last, st.lastPing, true
		default:
			return nil, nil // typed at the terminal
		}
		st.shown = surface.Working
		return append(rows, lamp(st.turn, lampPart, surface.Working)), nil
	}
	// turn_ended
	l, ping, byself := st.turn, st.ping, st.byself
	st.turn, st.ping, st.byself = nil, "", false
	var rows []Row
	stopped, err := f.stoppedBy(ctx, d.Outcome, d.Reason, d.Turn)
	if err != nil {
		return nil, err
	}
	s := surface.Done
	switch {
	case stopped != "":
		// A person asked for it: no lamp, which takes the working one
		// away, and no ping; the thread says who stopped it.
		s, ping = surface.NoStatus, ""
	case d.Outcome == "failure":
		s = surface.Failed
	case d.Outcome == "blocked":
		s = surface.Waiting
	}
	if l != nil {
		st.shown = s
		rows = append(rows, lamp(l, lampPart, s))
	}
	// A completed turn pings nobody when the agent started it, or in a
	// supervisor's thread, where the reply is already the answer (#639).
	t, err := f.svThread(ctx, r, e.Thread)
	if err != nil {
		return nil, err
	}
	sv := t.sv
	if sv && f.quietNow(t, f.now()) {
		ping = "" // no post of the thread mentions anyone (#657)
	}
	if ping != "" && (!(byself || sv) || d.Outcome == "failure" || d.Outcome == "blocked") {
		rows = append(rows, f.pingRow(ctx, r, e, l, ping, d.Outcome, convs))
	}
	return rows, nil
}

// litOf returns the message a turn of input s lamps, from what the front
// end recorded when it gave that input; nil when it lamps no message of its
// own, as an answer by a button.
func (f *FrontEnd) litOf(ctx context.Context, r *reader, thread control.ThreadID, s sender, convs []surface.Conversation) (*lit, error) {
	switch s.lamp {
	case "message":
		on := s.msg
		return &lit{conv: on.Conv, on: &on}, nil
	case "prompt":
		first, err := r.firstPos(ctx, thread)
		if err != nil {
			return nil, err
		}
		return &lit{conv: convs[0], row: &RowKey{Event: first, Part: 0}}, nil
	}
	return nil, nil
}

// pingRow is the one ping at a turn's end: a short line that names how it
// ended, in the thread, as a reply to the sender's message when that is in
// the thread, else to the thread's prompt.
func (f *FrontEnd) pingRow(ctx context.Context, r *reader, e control.Event, l *lit, ping, outcome string, convs []surface.Conversation) Row {
	text := "Done."
	switch outcome {
	case "failure":
		text = "The turn failed."
	case "blocked":
		text = "Waiting for your answer."
	}
	conv := convs[0]
	row := Row{Event: e.Pos, Part: pingPart, Decoration: true, Message: surface.Message{Text: text, Notify: []string{ping}}}
	switch {
	case l != nil && l.on != nil && l.on.Conv.Thread != "":
		conv = l.on.Conv
		on := *l.on
		row.Message.ReplyTo = &on
	default:
		if first, err := r.firstPos(ctx, e.Thread); err == nil {
			row.ReplyToRow = &RowKey{Event: first, Part: 0}
		}
	}
	row.Conv = conv
	return row
}

// lampRows is lamps for an event of the show loop. A thread seen for the
// first time since the start gets its lamp state from its earlier turn
// events first; the rows those give were written before.
func (f *FrontEnd) lampRows(ctx context.Context, r *reader, e control.Event, convs []surface.Conversation) ([]Row, error) {
	if e.Kind != "turn_started" && e.Kind != "turn_ended" {
		return nil, nil
	}
	if !r.lampKnown[e.Thread] {
		var earlier []control.Event
		if err := r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
			if ev.Kind == "turn_started" || ev.Kind == "turn_ended" {
				earlier = append(earlier, ev)
			}
		}); err != nil {
			return nil, err
		}
		for _, ev := range earlier {
			if _, err := f.lamps(ctx, r, ev, convs); err != nil {
				return nil, err
			}
		}
		r.lampKnown[e.Thread] = true
	}
	return f.lamps(ctx, r, e, convs)
}
