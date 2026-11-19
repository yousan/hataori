package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// reader is what the front end remembers about threads while it reads
// events. It is rebuilt from the threads' events when it is missing.
type reader struct {
	api   control.API
	first map[control.ThreadID]uint64               // position of each thread's first event
	turn  map[control.ThreadID]string               // key of the call that started the open turn
	by    map[control.ThreadID]string               // who started the open turn: caller, agent or terminal
	via   map[control.ThreadID]*control.LinkRef     // the link the open turn's first input came through; nil for none
	quiet map[control.ThreadID]*quiet               // open turns, for the progress line
	gone  map[control.ThreadID]*RowKey              // the last delete of a progress line
	known map[control.ThreadID]bool                 // threads whose earlier events were read
	subs  map[control.ThreadID]map[string]*subagent // running subagents, by id
	// outbox and ctx are the front end's, for looking up rows.
	outbox *Outbox
	ctx    context.Context
	// quietAfter is FrontEnd.QuietAfter.
	quietAfter time.Duration
	// conv is the conversation a thread is shown in here (FrontEnd.convOf),
	// looked up before the thread's events are read.
	conv func(control.ThreadID) surface.Conversation
}

// show reads the events of the front end's scopes, from where it stopped
// before, and writes what to post into the outbox, until ctx ends.
func (f *FrontEnd) show(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the subscription when reading stops on an error
	scopes := f.scopeList()
	r := &reader{
		api:   f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: scopes, May: control.OpRead}),
		first: map[control.ThreadID]uint64{},
		turn:  map[control.ThreadID]string{},
		by:    map[control.ThreadID]string{},
		via:   map[control.ThreadID]*control.LinkRef{},
		quiet: map[control.ThreadID]*quiet{},
		gone:  map[control.ThreadID]*RowKey{},
		known: map[control.ThreadID]bool{},
		subs:  map[control.ThreadID]map[string]*subagent{},

		outbox: f.Outbox,
		ctx:    ctx,

		quietAfter: f.quietAfter(),
		conv: func(id control.ThreadID) surface.Conversation {
			c, _, _ := f.convOf(ctx, id)
			return c
		},
	}
	var after uint64
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT pos FROM chat_cursors WHERE client = ?`, f.Name).Scan(&after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("chat: reading the cursor: %w", err)
	}
	// The open turns, for their progress lines, as their events up to the
	// cursor tell; the events after it are read below.
	threads, err := r.api.Threads(ctx, control.Filter{})
	if err != nil {
		return err
	}
	for _, t := range threads {
		if len(f.linkConvs(t)) > 0 && t.State == control.Running {
			if _, _, err := f.convOf(ctx, t.ID); err != nil {
				return err
			}
			if err := r.catchUp(ctx, t.ID, after+1); err != nil {
				return err
			}
		}
	}
	evs, err := r.api.Events(ctx, control.Filter{}, after)
	if err != nil {
		return err
	}
	defer stopEvents(cancel, evs)
	every := f.Tick
	if every <= 0 {
		every = time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	caughtUp := false // lines are due only once the stored events are read
	for {
		var e control.Event
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if !caughtUp {
				continue
			}
			if rows := f.tick(ctx, r, f.now()); len(rows) > 0 {
				if err := f.Outbox.Write(ctx, rows...); err != nil {
					return err
				}
			}
			continue
		case ev, ok := <-evs:
			if !ok {
				return ctx.Err()
			}
			e = ev
		}
		if e.Kind == control.CaughtUp {
			caughtUp = true
			continue
		}
		if e.Thread == 0 {
			continue
		}
		// Shown in the conversations of the thread's links here, each its
		// own rows; a thread with none is not shown by this front end.
		convs, err := f.convsOf(ctx, e.Thread)
		if err != nil {
			return err
		}
		linked := len(convs) > 0
		var rows []Row
		if linked {
			if rows, err = f.rowsFor(ctx, r, e, convs); err != nil {
				return err
			}
			// A thread seen for the first time since the start: what its
			// earlier events tell about the progress line, first.
			if err := r.catchUp(ctx, e.Thread, e.Pos); err != nil {
				return err
			}
			rows = everyLink(append(append(r.track(e), rows...), r.subagents(e)...), convs)
			rows = append(rows, f.mirrorRows(e, convs)...)
		}
		lr, err := f.loginEvent(ctx, e, linked)
		if err != nil {
			return err
		}
		rows = append(rows, lr...)
		err = f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
			if err := f.Outbox.WriteTx(ctx, tx, rows...); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO chat_cursors (client, pos) VALUES (?, ?)
				ON CONFLICT (client) DO UPDATE SET pos = excluded.pos`, f.Name, e.Pos)
			return err
		})
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			f.Outbox.Wake()
		}
	}
}

// eventData holds the fields of the event kinds the front end shows.
type eventData struct {
	Text     string   `json:"text"`
	Final    bool     `json:"final"`
	Approval bool     `json:"approval"`
	Choices  []string `json:"choices"`
	Outcome  string   `json:"outcome"`
	Reason   string   `json:"reason"`
	Key      string   `json:"key"`
	Session  int64    `json:"session"`
	File     string   `json:"file"`
	More     []string `json:"more"`
	Caption  string   `json:"caption"`
	By       string   `json:"by"` // turn_started: who started the turn
	// input, answer: the link it came through, since 3.6
	Link *control.LinkRef `json:"link"`
	// request, answer, request_withdrawn: the request; answer since 3.8
	Request string `json:"request"`
	Multi   bool   `json:"multi"` // request: several choices at once, since 3.8
}

// typedAtTerminal is the line that goes before the final answer of a turn a
// person typed at the agent's terminal (`hataori attach`), so that the chat
// thread shows the whole conversation and says where that turn came from.
const typedAtTerminal = "(typed at the terminal)"

// rowsFor returns the rows that show e. A kind it does not show gives none.
//
// The rows are for the thread's first conversation here, convs[0], and
// everyLink copies them to the others; a final answer's is for the
// conversation its turn's input came through, when that is one of convs.
func (f *FrontEnd) rowsFor(ctx context.Context, r *reader, e control.Event, convs []surface.Conversation) ([]Row, error) {
	conv := convs[0]
	switch e.Kind {
	case "input", "answer":
		var d eventData
		json.Unmarshal(e.Data, &d)
		if _, seen := r.via[e.Thread]; !seen && r.turn[e.Thread] != "" {
			r.via[e.Thread] = d.Link // the turn's first input or answer
		}
		if e.Kind == "answer" {
			return f.questionDone(ctx, r, e, conv, d.Request)
		}
		return nil, nil
	case "request_withdrawn":
		var d eventData
		json.Unmarshal(e.Data, &d)
		return f.questionDone(ctx, r, e, conv, d.Request)
	case "thread_created", "turn_started", "text", "request", "turn_ended", "file_out":
	default:
		return nil, nil // a kind the front end does not show, or does not know
	}
	var d eventData
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &d); err != nil {
			f.Log.Warn("chat: an event that cannot be read", "pos", e.Pos, "kind", e.Kind, "err", err)
			return nil, nil
		}
	}
	row := func(m surface.Message) []Row { return []Row{{Event: e.Pos, Conv: conv, Message: m}} }
	switch e.Kind {
	case "thread_created":
		r.first[e.Thread] = e.Pos
	case "turn_started":
		r.turn[e.Thread] = d.Key
		r.by[e.Thread] = d.By
		delete(r.via, e.Thread)
	case "text":
		if strings.TrimSpace(d.Text) == "" {
			return nil, nil
		}
		if !d.Final {
			return row(surface.Message{Text: d.Text, Quiet: true}), nil
		}
		key, by, err := r.turnKey(ctx, e)
		if err != nil {
			return nil, err
		}
		// The reply goes where the turn's input was written; a link of
		// another client, or one this front end no longer has, gets none.
		replies := true
		if l := r.via[e.Thread]; l != nil {
			c := surface.Conversation{Channel: l.Channel, Thread: l.Thread}
			if l.Client == f.Name && slices.Contains(convs, c) {
				conv = c
			} else {
				replies = false
			}
		}
		row = func(m surface.Message) []Row { return []Row{{Event: e.Pos, Conv: conv, Message: m}} }
		if by == "terminal" {
			return row(surface.Message{Text: typedAtTerminal + "\n" + d.Text}), nil
		}
		rows := row(surface.Message{Text: d.Text})
		if key == "" || !replies {
			return rows, nil // the agent started the turn by itself, or it came through no link here
		}
		trigger, err := f.isTrigger(ctx, conv.Channel, key)
		if err != nil {
			return nil, err
		}
		if !trigger {
			rows[0].Message.ReplyTo = &surface.MessageRef{Conv: conv, ID: key}
			return rows, nil
		}
		// The trigger is in the channel; in the thread its prompt stands
		// for it.
		first, err := r.firstPos(ctx, e.Thread)
		if err != nil {
			return nil, err
		}
		rows[0].ReplyToRow = &RowKey{Event: first, Part: 0}
		return rows, nil
	case "request":
		if d.Approval {
			return nil, nil // allowed by the core in phase 1; nothing to answer
		}
		return row(f.questionMessage(d.Request, d.Text, d.Choices, d.Multi)), nil
	case "file_out":
		return f.fileRows(ctx, e, conv, d.Session, append([]string{d.File}, d.More...), d.Caption), nil
	case "turn_ended":
		delete(r.turn, e.Thread)
		delete(r.by, e.Thread)
		delete(r.via, e.Thread)
		if d.Outcome == "failure" {
			return row(surface.Message{Text: failureLine(d.Reason)}), nil
		}
	}
	return nil, nil
}

// question shows a question with its options numbered; the next message
// in the thread answers it.
func question(text string, choices []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	for i, c := range choices {
		if i == 0 && b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("\n" + strconv.Itoa(i+1) + ". " + c)
	}
	return b.String()
}

// failureLine is the one line a failed turn gets.
func failureLine(reason string) string {
	switch reason {
	case "limit":
		return "The turn failed: the usage limit was reached."
	case "login":
		return "The turn failed: the agent is not logged in."
	case "crashed":
		return "The turn failed: the agent crashed."
	case "":
		return "The turn failed: error."
	}
	return "The turn failed: error (" + reason + ")."
}

// isTrigger reports whether key is a trigger message this front end
// received in channel.
func (f *FrontEnd) isTrigger(ctx context.Context, channel, key string) (bool, error) {
	_, ok, err := f.stored(ctx, surface.MessageRef{Conv: surface.Conversation{Channel: channel}, ID: key})
	return ok, err
}

// turnKey returns the key of the call that started the turn e is in, and
// who started it.
func (r *reader) turnKey(ctx context.Context, e control.Event) (key, by string, err error) {
	if k, ok := r.turn[e.Thread]; ok {
		return k, r.by[e.Thread], nil
	}
	err = r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
		switch ev.Kind {
		case "turn_started":
			var d eventData
			json.Unmarshal(ev.Data, &d)
			r.turn[e.Thread] = d.Key
			r.by[e.Thread] = d.By
			delete(r.via, e.Thread)
		case "input", "answer":
			var d eventData
			json.Unmarshal(ev.Data, &d)
			if _, seen := r.via[e.Thread]; !seen {
				r.via[e.Thread] = d.Link
			}
		case "turn_ended":
			delete(r.turn, e.Thread)
			delete(r.by, e.Thread)
			delete(r.via, e.Thread)
		}
	})
	return r.turn[e.Thread], r.by[e.Thread], err
}

// firstPos returns the position of a thread's first event.
func (r *reader) firstPos(ctx context.Context, id control.ThreadID) (uint64, error) {
	if p, ok := r.first[id]; ok {
		return p, nil
	}
	p, _, err := startOutcome(ctx, r.api, id)
	if err == nil {
		r.first[id] = p
	}
	return p, err
}

// catchUp follows the thread's events before position before for the
// progress line, once per thread.
func (r *reader) catchUp(ctx context.Context, id control.ThreadID, before uint64) error {
	if r.known[id] {
		return nil
	}
	r.known[id] = true
	return r.scan(ctx, id, before, func(e control.Event) { r.track(e); r.subagents(e) })
}

// stopEvents ends a subscription to API.Events and waits until it has
// stopped: its channel is closed only once nothing is read for it any more.
// A caller that returns without waiting leaves the store in use, so it
// could be closed while a read is still going on.
func stopEvents(cancel context.CancelFunc, evs <-chan control.Event) {
	cancel()
	for range evs {
	}
}

// scan calls fn with the thread's stored events before position before.
func (r *reader) scan(ctx context.Context, id control.ThreadID, before uint64, fn func(control.Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := r.api.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		return err
	}
	defer stopEvents(cancel, evs)
	for e := range evs {
		if e.Kind == control.CaughtUp || e.Pos >= before {
			return nil
		}
		fn(e)
	}
	return ctx.Err()
}
