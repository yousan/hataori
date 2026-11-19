// Package chat is the chat front end: the client of the control API that
// shows threads in a chat service, shared by every surface driver.
package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// Row is one message the front end posts. It is keyed by the event it
// shows, the link of its conversation and a part number, so that writing
// it twice writes one row. A row in a conversation that is no link has
// link 0. ReplyToRow and Target name rows of the same conversation.
type Row struct {
	Event   uint64 // position of the event in the event log
	Part    int    // which of the event's messages this is
	Conv    surface.Conversation
	Message surface.Message
	// Decoration marks a message that may be lost: it is dropped after
	// MaxFailures failed posts and never holds back the rows behind it.
	Decoration bool
	// ReplyToRow makes the message a reply to the message another row
	// posted, once that row is sent; Message.ReplyTo is used otherwise.
	ReplyToRow *RowKey
	// Op is what the row does: post (the default), or edit or delete the
	// message that the row Target posted. An edit or a delete whose target
	// was never posted is dropped. A delete is never dropped for failing.
	Op     Op
	Target *RowKey
}

// Op is what a row does.
type Op string

// The values of Op.
const (
	Post   Op = ""
	Edit   Op = "edit"
	Delete Op = "delete"
)

// RowKey names a row.
type RowKey struct {
	Event uint64 `json:"event"`
	Part  int    `json:"part"`
}

// Key is the key a row is posted with, the same on every attempt.
func (r Row) Key() string { return key(r.Event, r.Part) }

func key(event uint64, part int) string {
	return strconv.FormatUint(event, 10) + "." + strconv.Itoa(part)
}

// Outbox holds what one front end posts, and its sender posts it. Rows of
// one conversation are posted in the order of their keys, the order of the
// events they show.
type Outbox struct {
	Store  *store.Store
	Client string // the front end's name; rows of other clients are not touched
	Driver surface.Driver
	Log    *slog.Logger
	// MaxFailures is how many failed posts drop a decoration (default 3).
	MaxFailures int
	// Retry is how long the sender waits before trying a failed post
	// again (default 5s).
	Retry time.Duration
	Now   func() time.Time // the clock of sent_at; time.Now when nil

	once sync.Once
	wake chan struct{}
	mu   sync.Mutex
	busy map[surface.Conversation]bool
}

func (o *Outbox) init() {
	o.once.Do(func() {
		o.wake = make(chan struct{}, 1)
		o.busy = map[surface.Conversation]bool{}
		if o.MaxFailures <= 0 {
			o.MaxFailures = 3
		}
		if o.Retry <= 0 {
			o.Retry = 5 * time.Second
		}
		if o.Log == nil {
			o.Log = slog.Default()
		}
	})
}

func (o *Outbox) poke() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// message is surface.Message as stored.
type message struct {
	Text    string           `json:"text,omitempty"`
	Files   []surface.File   `json:"files,omitempty"`
	Choices []surface.Choice `json:"choices,omitempty"`
	ReplyTo *ref             `json:"reply_to,omitempty"`
	Notify  []string         `json:"notify,omitempty"`
	Quiet   bool             `json:"quiet,omitempty"`
	// ReplyRow is Row.ReplyToRow, resolved when the row is posted.
	ReplyRow *RowKey `json:"reply_row,omitempty"`
	Op       Op      `json:"op,omitempty"`
	Target   *RowKey `json:"target,omitempty"`
}

type ref struct {
	Channel string `json:"channel"`
	Thread  string `json:"thread,omitempty"`
	ID      string `json:"id"`
}

func encode(r Row) ([]byte, error) {
	m := r.Message
	s := message{Text: m.Text, Files: m.Files, Choices: m.Choices, Notify: m.Notify, Quiet: m.Quiet,
		ReplyRow: r.ReplyToRow, Op: r.Op, Target: r.Target}
	if r := m.ReplyTo; r != nil {
		s.ReplyTo = &ref{Channel: r.Conv.Channel, Thread: r.Conv.Thread, ID: r.ID}
	}
	return json.Marshal(s)
}

func decode(data []byte) (surface.Message, message, error) {
	var s message
	if err := json.Unmarshal(data, &s); err != nil {
		return surface.Message{}, s, err
	}
	m := surface.Message{Text: s.Text, Files: s.Files, Choices: s.Choices, Notify: s.Notify, Quiet: s.Quiet}
	if r := s.ReplyTo; r != nil {
		m.ReplyTo = &surface.MessageRef{Conv: surface.Conversation{Channel: r.Channel, Thread: r.Thread}, ID: r.ID}
	}
	return m, s, nil
}

// Write stores rows in one transaction. A row whose key exists is left as
// it is.
func (o *Outbox) Write(ctx context.Context, rows ...Row) error {
	if err := o.Store.Update(ctx, func(tx *store.Tx) error { return o.WriteTx(ctx, tx, rows...) }); err != nil {
		return err
	}
	o.Wake()
	return nil
}

// WriteTx stores rows in tx. The sender sees them once tx is committed
// and Wake is called (Write does both).
func (o *Outbox) WriteTx(ctx context.Context, tx *store.Tx, rows ...Row) error {
	now := time.Now().UnixMilli()
	for _, r := range rows {
		msg, err := encode(r)
		if err != nil {
			return fmt.Errorf("chat: outbox: %w", err)
		}
		// link: the link of the conversation, or 0 (design, section 5.2).
		_, err = tx.ExecContext(ctx, `INSERT INTO chat_outbox
			(client, event, part, channel, thread, message, decoration, created_at, link) VALUES (?, ?, ?, ?, ?, ?, ?, ?,
			coalesce((SELECT id FROM links WHERE client = ? AND channel = ? AND thread = ?), 0))
			ON CONFLICT (client, event, link, part) DO NOTHING`,
			o.Client, r.Event, r.Part, r.Conv.Channel, r.Conv.Thread, string(msg), r.Decoration, now,
			o.Client, r.Conv.Channel, r.Conv.Thread)
		if err != nil {
			return fmt.Errorf("chat: outbox: write %s: %w", r.Key(), err)
		}
	}
	return nil
}

// Wake tells the sender that rows may be waiting.
func (o *Outbox) Wake() {
	o.init()
	o.poke()
}

// Sent returns the reference of the posted message of a row, in any of
// its conversations, and false while none is sent (or the row does not
// exist, or was dropped).
func (o *Outbox) Sent(ctx context.Context, event uint64, part int) (surface.MessageRef, bool, error) {
	var r surface.MessageRef
	err := o.Store.DB.QueryRowContext(ctx, `SELECT channel, thread, ref FROM chat_outbox
		WHERE client = ? AND event = ? AND part = ? AND state = 'sent' ORDER BY id LIMIT 1`, o.Client, event, part).
		Scan(&r.Conv.Channel, &r.Conv.Thread, &r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return surface.MessageRef{}, false, nil
	}
	if err != nil {
		return surface.MessageRef{}, false, fmt.Errorf("chat: outbox: %w", err)
	}
	return r, true, nil
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// lineChanges returns, for the rows in conv of event with a part of
// linePart or more (a progress line and its edits), when the last of them
// was sent (zero for none) and whether one is pending.
func (o *Outbox) lineChanges(ctx context.Context, conv surface.Conversation, event uint64) (last time.Time, pending bool, err error) {
	var ms sql.NullInt64
	var n int
	err = o.Store.DB.QueryRowContext(ctx, `SELECT max(CASE WHEN state = 'sent' THEN sent_at END),
		count(CASE WHEN state = 'pending' THEN 1 END) FROM chat_outbox
		WHERE client = ? AND channel = ? AND thread = ? AND event = ? AND part >= ?`,
		o.Client, conv.Channel, conv.Thread, event, linePart).Scan(&ms, &n)
	if err != nil {
		return time.Time{}, false, err
	}
	if ms.Valid {
		last = time.UnixMilli(ms.Int64)
	}
	return last, n > 0, nil
}

// state returns the state of the row k in conv ("" when it does not
// exist) and its message reference once it is sent.
func (o *Outbox) state(ctx context.Context, conv surface.Conversation, k RowKey) (string, surface.MessageRef, error) {
	var (
		state string
		r     surface.MessageRef
	)
	err := o.Store.DB.QueryRowContext(ctx, `SELECT state, channel, thread, ref FROM chat_outbox
		WHERE client = ? AND channel = ? AND thread = ? AND event = ? AND part = ?`,
		o.Client, conv.Channel, conv.Thread, k.Event, k.Part).Scan(&state, &r.Conv.Channel, &r.Conv.Thread, &r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", surface.MessageRef{}, nil
	}
	if err != nil {
		o.Log.Error("chat outbox: reading a row", "key", key(k.Event, k.Part), "err", err)
	}
	return state, r, err
}

// Run posts the pending rows until ctx ends. Each conversation is sent by
// a goroutine of its own, so a failing one holds back no other.
func (o *Outbox) Run(ctx context.Context) error {
	o.init()
	var wg sync.WaitGroup
	defer wg.Wait()
	tick := time.NewTicker(o.Retry)
	defer tick.Stop()
	for {
		convs, err := o.pendingConvs(ctx)
		if err != nil && ctx.Err() == nil {
			o.Log.Error("chat outbox: reading pending rows", "err", err)
		}
		for _, c := range convs {
			o.mu.Lock()
			if o.busy[c] {
				o.mu.Unlock()
				continue
			}
			o.busy[c] = true
			o.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.drain(ctx, c)
			}()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.wake:
		case <-tick.C:
		}
	}
}

func (o *Outbox) pendingConvs(ctx context.Context) ([]surface.Conversation, error) {
	rows, err := o.Store.DB.QueryContext(ctx, `SELECT DISTINCT channel, thread FROM chat_outbox
		WHERE client = ? AND state = 'pending'`, o.Client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []surface.Conversation
	for rows.Next() {
		var c surface.Conversation
		if err := rows.Scan(&c.Channel, &c.Thread); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type pending struct {
	id         int64
	event      uint64
	part       int
	message    []byte
	decoration bool
	failures   int
	link       int64
}

// drain posts conv's pending rows in order until none is left or a reply
// fails; the next tick tries again.
func (o *Outbox) drain(ctx context.Context, conv surface.Conversation) {
	defer func() {
		o.mu.Lock()
		delete(o.busy, conv)
		o.mu.Unlock()
	}()
	failed := map[int64]bool{} // decorations that failed in this call; tried again on the next tick
	for ctx.Err() == nil {
		rows, err := o.pendingRows(ctx, conv)
		if err != nil {
			if ctx.Err() == nil {
				o.Log.Error("chat outbox: reading pending rows", "err", err)
			}
			return
		}
		left := false
	rows:
		for _, r := range rows {
			if failed[r.id] {
				continue
			}
			left = true
			switch o.send(ctx, conv, r) {
			case stuck:
				return
			case reread:
				break rows
			case failedPost:
				if !r.decoration {
					return // a reply holds back the rows behind it
				}
				failed[r.id] = true
			}
		}
		if !left {
			return
		}
	}
}

func (o *Outbox) pendingRows(ctx context.Context, conv surface.Conversation) ([]pending, error) {
	rows, err := o.Store.DB.QueryContext(ctx, `SELECT id, event, part, message, decoration, failures, link FROM chat_outbox
		WHERE client = ? AND channel = ? AND thread = ? AND state = 'pending' ORDER BY event, part`,
		o.Client, conv.Channel, conv.Thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.event, &p.part, &p.message, &p.decoration, &p.failures, &p.link); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type outcome int

const (
	posted     outcome = iota
	failedPost         // the post failed, or the row was dropped
	stuck              // the outcome could not be recorded: stop, or the row is posted again at once
	reread             // the queue changed in front of the row: read it again
)

// postKey is the key row r is posted with, the same on every attempt. A
// surface may hold a key unique across all of its conversations, as one
// that keys by the author does, so the copy of a row in a second link has
// a key of its own. The row of the thread's first link, and every row written before
// rows were per link, keep the key they always had.
func (o *Outbox) postKey(ctx context.Context, r pending) (string, error) {
	k := key(r.event, r.part)
	if r.link == 0 {
		return k, nil
	}
	var n int
	err := o.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_outbox
		WHERE client = ? AND event = ? AND part = ? AND link != 0 AND link < ?`, o.Client, r.event, r.part, r.link).Scan(&n)
	if err != nil || n == 0 {
		return k, err
	}
	return k + "@" + strconv.FormatInt(r.link, 10), nil
}

// send posts one row and records the outcome.
func (o *Outbox) send(ctx context.Context, conv surface.Conversation, r pending) outcome {
	k, kerr := o.postKey(ctx, r)
	if kerr != nil {
		return stuck
	}
	m, meta, err := decode(r.message)
	replyRow := meta.ReplyRow
	if err == nil && replyRow != nil {
		state, to, err := o.state(ctx, conv, *replyRow)
		switch {
		case err != nil:
			return stuck
		case state == "sent":
			m.ReplyTo = &to
		case state == "pending":
			// Written since this drain read the queue; it comes earlier in
			// the order, so read the queue again.
			return reread
		case state == "" && r.failures+1 < o.MaxFailures:
			// The row it replies to is not written yet; give it a few ticks.
			return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET failures = failures + 1 WHERE id = ?`, r.id)
		}
		// Otherwise (dropped, or never written) it is posted without the reply.
	}
	if err != nil {
		// Unreadable rows cannot become readable: drop it, so it holds
		// nothing back.
		o.Log.Error("chat outbox: dropping an unreadable row", "key", k, "err", err)
		return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped' WHERE id = ?`, r.id)
	}
	if meta.Op == Edit || meta.Op == Delete {
		return o.change(ctx, conv, r, meta, m)
	}
	ref, err := o.Driver.Post(ctx, conv, k, m)
	if err == nil {
		return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'sent', ref = ?, sent_at = ? WHERE id = ?`,
			ref.ID, o.now().UnixMilli(), r.id)
	}
	if ctx.Err() != nil {
		return stuck
	}
	if r.decoration && r.failures+1 >= o.MaxFailures {
		o.Log.Warn("chat outbox: dropping a decoration", "key", k, "failures", r.failures+1, "err", err)
		return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped', failures = failures + 1 WHERE id = ?`, r.id)
	}
	o.Log.Warn("chat outbox: post failed", "key", k, "decoration", r.decoration, "err", err)
	return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET failures = failures + 1 WHERE id = ?`, r.id)
}

// change edits or deletes the message another row posted.
func (o *Outbox) change(ctx context.Context, conv surface.Conversation, r pending, meta message, m surface.Message) outcome {
	k := key(r.event, r.part)
	var (
		state string
		to    surface.MessageRef
		err   error
	)
	if meta.Target != nil {
		state, to, err = o.state(ctx, conv, *meta.Target)
	}
	switch {
	case err != nil:
		return stuck
	case state == "pending":
		// It comes earlier in the order, so it failed or was written since
		// this drain read the queue: try again on the next tick. Not a
		// failure of this row.
		return failedPost
	case state != "sent":
		// Never posted, or dropped: there is nothing to change.
		return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped' WHERE id = ?`, r.id)
	}
	if meta.Op == Edit {
		err = o.Driver.Edit(ctx, to, m)
	} else {
		err = o.Driver.Delete(ctx, to)
	}
	if err == nil {
		return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'sent', ref = ?, sent_at = ? WHERE id = ?`,
			to.ID, o.now().UnixMilli(), r.id)
	}
	if ctx.Err() != nil {
		return stuck
	}
	if meta.Op == Edit && r.decoration && r.failures+1 >= o.MaxFailures {
		o.Log.Warn("chat outbox: dropping an edit", "key", k, "failures", r.failures+1, "err", err)
		return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped', failures = failures + 1 WHERE id = ?`, r.id)
	}
	o.Log.Warn("chat outbox: "+string(meta.Op)+" failed", "key", k, "decoration", r.decoration, "err", err)
	return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET failures = failures + 1 WHERE id = ?`, r.id)
}

// record runs q on row id and returns ok, or stuck when it fails.
func (o *Outbox) record(ctx context.Context, ok outcome, id int64, q string, args ...any) outcome {
	err := o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			o.Log.Error("chat outbox: recording a post", "row", id, "err", err)
		}
		return stuck
	}
	return ok
}
