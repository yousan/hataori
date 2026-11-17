package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	quiet map[control.ThreadID]*quiet               // open turns, for the progress line
	gone  map[control.ThreadID]*RowKey              // the last delete of a progress line
	known map[control.ThreadID]bool                 // threads whose earlier events were read
	subs  map[control.ThreadID]map[string]*subagent // running subagents, by id
	// outbox and ctx are the front end's, for looking up rows.
	outbox *Outbox
	ctx    context.Context
	// quietAfter is FrontEnd.QuietAfter.
	quietAfter time.Duration
}

// show reads the events of the front end's scopes, from where it stopped
// before, and writes what to post into the outbox, until ctx ends.
func (f *FrontEnd) show(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the subscription when reading stops on an error
	var scopes []string
	for s := range f.Scopes {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	r := &reader{
		api:   f.API(control.Caller{Principal: f.Owner, Scopes: scopes, May: control.OpRead}),
		first: map[control.ThreadID]uint64{},
		turn:  map[control.ThreadID]string{},
		quiet: map[control.ThreadID]*quiet{},
		gone:  map[control.ThreadID]*RowKey{},
		known: map[control.ThreadID]bool{},
		subs:  map[control.ThreadID]map[string]*subagent{},

		outbox: f.Outbox,
		ctx:    ctx,

		quietAfter: f.quietAfter(),
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
		if t.State == control.Running {
			if err := r.catchUp(ctx, t.ID, after+1); err != nil {
				return err
			}
		}
	}
	evs, err := r.api.Events(ctx, control.Filter{}, after)
	if err != nil {
		return err
	}
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
		if e.Thread.Key == "" {
			continue
		}
		rows, err := f.rowsFor(ctx, r, e)
		if err != nil {
			return err
		}
		// A thread seen for the first time since the start: what its
		// earlier events tell about the progress line, first.
		if err := r.catchUp(ctx, e.Thread, e.Pos); err != nil {
			return err
		}
		rows = append(append(r.track(e), rows...), r.subagents(e)...)
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
}

// rowsFor returns the rows that show e. A kind it does not show gives none.
func (f *FrontEnd) rowsFor(ctx context.Context, r *reader, e control.Event) ([]Row, error) {
	switch e.Kind {
	case "thread_created", "turn_started", "text", "request", "turn_ended":
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
	conv := surface.Conversation{Channel: e.Thread.Scope, Thread: e.Thread.Key}
	row := func(m surface.Message) []Row { return []Row{{Event: e.Pos, Conv: conv, Message: m}} }
	switch e.Kind {
	case "thread_created":
		r.first[e.Thread] = e.Pos
	case "turn_started":
		r.turn[e.Thread] = d.Key
	case "text":
		if strings.TrimSpace(d.Text) == "" {
			return nil, nil
		}
		if !d.Final {
			return row(surface.Message{Text: d.Text, Quiet: true}), nil
		}
		rows := row(surface.Message{Text: d.Text})
		key, err := r.turnKey(ctx, e)
		if err != nil {
			return nil, err
		}
		if key == "" {
			return rows, nil // the agent started the turn by itself
		}
		trigger, err := f.isTrigger(ctx, e.Thread.Scope, key)
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
		return row(surface.Message{Text: question(d.Text, d.Choices)}), nil
	case "turn_ended":
		delete(r.turn, e.Thread)
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

// turnKey returns the key of the call that started the turn e is in.
func (r *reader) turnKey(ctx context.Context, e control.Event) (string, error) {
	if k, ok := r.turn[e.Thread]; ok {
		return k, nil
	}
	err := r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
		switch ev.Kind {
		case "turn_started":
			var d eventData
			json.Unmarshal(ev.Data, &d)
			r.turn[e.Thread] = d.Key
		case "turn_ended":
			delete(r.turn, e.Thread)
		}
	})
	return r.turn[e.Thread], err
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

// scan calls fn with the thread's stored events before position before.
func (r *reader) scan(ctx context.Context, id control.ThreadID, before uint64, fn func(control.Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := r.api.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		return err
	}
	for e := range evs {
		if e.Kind == control.CaughtUp || e.Pos >= before {
			return nil
		}
		fn(e)
	}
	return ctx.Err()
}
