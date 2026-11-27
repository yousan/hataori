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
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
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
	// MaxFailures failed posts and never holds back the rows behind it. Any
	// other post that fails holds them back until it is posted, or is given
	// up as the service refused it (surface.ErrRefused).
	Decoration bool
	// ReplyToRow makes the message a reply to the message another row
	// posted, once that row is sent; Message.ReplyTo is used otherwise.
	ReplyToRow *RowKey
	// Op is what the row does: post (the default), or edit or delete the
	// message that the row Target posted. An edit or a delete whose target
	// was never posted is dropped. A delete is never dropped for failing.
	// Lamp shows Status on the message On names, or on the one the row
	// Target posted; a lamp is a decoration, and is dropped once a later
	// lamp on the same message is written. Archive and Unarchive set the
	// archived flag of the chat thread Conv, once (archive.go).
	Op     Op
	Target *RowKey
	On     *surface.MessageRef
	Status surface.Status
}

// Op is what a row does.
type Op string

// The values of Op.
const (
	Post   Op = ""
	Edit   Op = "edit"
	Delete Op = "delete"
	Lamp   Op = "lamp"

	Archive   Op = "archive"
	Unarchive Op = "unarchive"
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
	// Hooks runs the plugins' message_sending handlers (design, section
	// 7.3) before a row is posted, and before an edit that changes a
	// post's text. Nil: none, and rows go as they are.
	Hooks control.Hooks
	// Scopes are the keys of the scopes this front end shows: a row with
	// no link in a scope's own channel, such as a reply to a trigger, is
	// hooked in that scope. A row in no link and no scope's channel, such
	// as a direct message, is not hooked.
	Scopes []string
	// PostSent writes a post_sent event for each row of a link once it is
	// posted. The daemon sets it when a plugin is granted that event kind;
	// without one, no reader needs it, and posting writes no event.
	PostSent bool

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
	Multi   bool             `json:"multi,omitempty"`
	ReplyTo *ref             `json:"reply_to,omitempty"`
	Notify  []string         `json:"notify,omitempty"`
	Quiet   bool             `json:"quiet,omitempty"`
	// ReplyRow is Row.ReplyToRow, resolved when the row is posted.
	ReplyRow *RowKey `json:"reply_row,omitempty"`
	Op       Op      `json:"op,omitempty"`
	Target   *RowKey `json:"target,omitempty"`
	On       *ref    `json:"on,omitempty"`
	Status   int     `json:"status,omitempty"`
	// Lit names the message a lamp is on, the same for every lamp on it:
	// "row:<event>.<part>" or "msg:<channel>/<thread>/<id>".
	Lit string `json:"lit,omitempty"`
}

type ref struct {
	Channel string `json:"channel"`
	Thread  string `json:"thread,omitempty"`
	ID      string `json:"id"`
}

func encode(r Row) ([]byte, error) {
	m := r.Message
	s := message{Text: m.Text, Files: m.Files, Choices: m.Choices, Multi: m.Multi, Notify: m.Notify, Quiet: m.Quiet,
		ReplyRow: r.ReplyToRow, Op: r.Op, Target: r.Target}
	if r := m.ReplyTo; r != nil {
		s.ReplyTo = &ref{Channel: r.Conv.Channel, Thread: r.Conv.Thread, ID: r.ID}
	}
	if r.Op == Lamp {
		s.Status = int(r.Status)
		switch {
		case r.On != nil:
			s.On = &ref{Channel: r.On.Conv.Channel, Thread: r.On.Conv.Thread, ID: r.On.ID}
			s.Lit = "msg:" + r.On.Conv.Channel + "/" + r.On.Conv.Thread + "/" + r.On.ID
		case r.Target != nil:
			s.Lit = "row:" + key(r.Target.Event, r.Target.Part)
		default:
			return nil, errors.New("a lamp on no message")
		}
	}
	return json.Marshal(s)
}

func decode(data []byte) (surface.Message, message, error) {
	var s message
	if err := json.Unmarshal(data, &s); err != nil {
		return surface.Message{}, s, err
	}
	m := surface.Message{Text: s.Text, Files: s.Files, Choices: s.Choices, Multi: s.Multi, Notify: s.Notify, Quiet: s.Quiet}
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
	hook       string         // what message_sending made of it: "", "passed"
	hookText   sql.NullString // its text as the handlers rewrote it
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
			if ctx.Err() != nil {
				return // a confirmed post is recorded, but no next post starts
			}
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
	rows, err := o.Store.DB.QueryContext(ctx, `SELECT id, event, part, message, decoration, failures, link, hook, hook_text FROM chat_outbox
		WHERE client = ? AND channel = ? AND thread = ? AND state = 'pending' ORDER BY event, part`,
		o.Client, conv.Channel, conv.Thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.event, &p.part, &p.message, &p.decoration, &p.failures, &p.link, &p.hook, &p.hookText); err != nil {
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
	if meta.Op == Lamp {
		return o.lamp(ctx, conv, r, meta)
	}
	if meta.Op == Archive || meta.Op == Unarchive {
		return o.archive(ctx, conv, r, meta.Op)
	}
	text, out, ok := o.hooked(ctx, conv, r, m.Text, len(m.Files) > 0, false)
	if !ok {
		return out
	}
	m.Text = text
	ref, err := o.Driver.Post(ctx, conv, k, m)
	if err == nil {
		return o.sent(ctx, r, ref, m.Text)
	}
	if ctx.Err() != nil {
		return stuck
	}
	if r.decoration && r.failures+1 >= o.MaxFailures {
		o.Log.Warn("chat outbox: dropping a decoration", "key", k, "failures", r.failures+1, "err", err)
		return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped', failures = failures + 1 WHERE id = ?`, r.id)
	}
	if !r.decoration && errors.Is(err, surface.ErrRefused) {
		return o.refused(ctx, conv, r, k, ref, err)
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
	if meta.Op == Edit && meta.Target != nil && err == nil {
		// A later edit of the same message makes this one moot: one that
		// failed and is tried again never puts back what a newer one took
		// away, as a progress line's button.
		var later int
		err = o.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_outbox WHERE client = ? AND channel = ? AND thread = ?
			AND json_extract(message, '$.op') = 'edit' AND json_extract(message, '$.target.event') = ?
			AND json_extract(message, '$.target.part') = ? AND (event > ? OR (event = ? AND part > ?))`,
			o.Client, conv.Channel, conv.Thread, meta.Target.Event, meta.Target.Part, r.event, r.event, r.part).Scan(&later)
		if err == nil && later > 0 {
			return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'dropped' WHERE id = ?`, r.id)
		}
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
	if meta.Op == Edit && o.Hooks != nil {
		// Only an edit that changes the text the post shows is hooked: as
		// the post, or its last edit that was sent, left it.
		var data []byte
		var shown sql.NullString
		err := o.Store.DB.QueryRowContext(ctx, `SELECT message, hook_text FROM chat_outbox
			WHERE client = ? AND channel = ? AND thread = ? AND state = 'sent' AND ((event = ? AND part = ?)
				OR (json_extract(message, '$.op') = 'edit' AND json_extract(message, '$.target.event') = ?
					AND json_extract(message, '$.target.part') = ?))
			ORDER BY event DESC, part DESC LIMIT 1`,
			o.Client, conv.Channel, conv.Thread, meta.Target.Event, meta.Target.Part, meta.Target.Event, meta.Target.Part).Scan(&data, &shown)
		if err != nil {
			return stuck
		}
		before, _, _ := decode(data)
		if shown.Valid {
			before.Text = shown.String
		}
		if m.Text != before.Text {
			text, out, ok := o.hooked(ctx, conv, r, m.Text, len(m.Files) > 0, true)
			if !ok {
				return out
			}
			m.Text = text
		}
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

// lamp shows a status on a message. A later lamp on the same message, in
// the order rows are written, makes this one moot: it is dropped, so a
// lamp that failed and is tried again never covers a newer one.
func (o *Outbox) lamp(ctx context.Context, conv surface.Conversation, r pending, meta message) outcome {
	k := key(r.event, r.part)
	var later int
	err := o.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_outbox WHERE client = ? AND channel = ? AND thread = ?
		AND json_extract(message, '$.lit') = ? AND (event > ? OR (event = ? AND part > ?))`,
		o.Client, conv.Channel, conv.Thread, meta.Lit, r.event, r.event, r.part).Scan(&later)
	if err != nil {
		return stuck
	}
	if later > 0 {
		return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'dropped' WHERE id = ?`, r.id)
	}
	var to surface.MessageRef
	switch {
	case meta.On != nil:
		to = surface.MessageRef{Conv: surface.Conversation{Channel: meta.On.Channel, Thread: meta.On.Thread}, ID: meta.On.ID}
	case meta.Target != nil:
		state, ref, err := o.state(ctx, conv, *meta.Target)
		switch {
		case err != nil:
			return stuck
		case state == "pending" && r.failures+1 < o.MaxFailures:
			return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET failures = failures + 1 WHERE id = ?`, r.id)
		case state != "sent":
			return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped' WHERE id = ?`, r.id)
		}
		to = ref
	}
	err = o.Driver.SetStatus(ctx, to, surface.Status(meta.Status))
	if err == nil {
		return o.record(ctx, posted, r.id, `UPDATE chat_outbox SET state = 'sent', ref = ?, sent_at = ? WHERE id = ?`,
			to.ID, o.now().UnixMilli(), r.id)
	}
	if ctx.Err() != nil {
		return stuck
	}
	if r.failures+1 >= o.MaxFailures {
		o.Log.Warn("chat outbox: dropping a lamp", "key", k, "failures", r.failures+1, "err", err)
		return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET state = 'dropped', failures = failures + 1 WHERE id = ?`, r.id)
	}
	o.Log.Warn("chat outbox: lamp failed", "key", k, "err", err)
	return o.record(ctx, failedPost, r.id, `UPDATE chat_outbox SET failures = failures + 1 WHERE id = ?`, r.id)
}

// PointMessageSending is the hook point of the outbox (design, section 7.3).
const PointMessageSending = "message_sending"

// where is the thread, scope and link of row r in conv: those of its link,
// or, for a row with no link in a scope's own channel or a chat thread of
// it, that scope with no thread. ok is false for a row in neither, which is not hooked.
func (o *Outbox) where(ctx context.Context, conv surface.Conversation, r pending) (thread control.ThreadID, scope string, link control.LinkRef, ok bool, err error) {
	if r.link != 0 {
		var id int64
		err = o.Store.DB.QueryRowContext(ctx, `SELECT l.thread_id, t.scope, l.client, l.channel, l.thread FROM links l
			JOIN threads t ON t.id = l.thread_id WHERE l.id = ?`, r.link).Scan(&id, &scope, &link.Client, &link.Channel, &link.Thread)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", link, false, nil
		}
		return control.ThreadID(id), scope, link, err == nil, err
	}
	if slices.Contains(o.Scopes, conv.Channel) {
		return 0, conv.Channel, control.LinkRef{Client: o.Client, Channel: conv.Channel}, true, nil
	}
	return 0, "", link, false, nil
}

// hooked runs message_sending on row r, whose text is text, unless it was
// run before: the result is stored in the row before anything is posted,
// so that a row posted again, after a restart too, is not hooked again; a
// daemon that dies during the call calls it again. The same event's part
// with the same text in another link takes the result stored there, so
// that every link shows the same. It returns the text to post, or ok
// false with the outcome when the row is cancelled or the result could
// not be stored.
func (o *Outbox) hooked(ctx context.Context, conv surface.Conversation, r pending, text string, files, edit bool) (string, outcome, bool) {
	if r.hook == "passed" {
		if r.hookText.Valid {
			return r.hookText.String, posted, true
		}
		return text, posted, true
	}
	if o.Hooks == nil {
		return text, posted, true
	}
	thread, scope, link, ok, err := o.where(ctx, conv, r)
	if err != nil {
		return "", stuck, false
	}
	if !ok {
		return text, posted, true
	}
	var res control.HookResult
	var hook, by, reason string
	var failed bool
	var rewritten sql.NullString
	if r.event != 0 {
		unlock := lockPart(RowKey{Event: r.event, Part: r.part})
		defer unlock()
		// Another link's copy of the same part, with the same text.
		err = o.Store.DB.QueryRowContext(ctx, `SELECT hook, hook_text, hook_by, hook_reason, hook_failed FROM chat_outbox
			WHERE event = ? AND part = ? AND id != ? AND hook != '' AND coalesce(json_extract(message, '$.text'), '') = ?
			ORDER BY id LIMIT 1`, r.event, r.part, r.id, text).Scan(&hook, &rewritten, &by, &reason, &failed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", stuck, false
		}
	}
	switch hook {
	case "passed":
		res = control.HookResult{Text: text}
		if rewritten.Valid {
			res.Text = rewritten.String
		}
	case "cancelled":
		res = control.HookResult{Stopped: true, By: by, Reason: reason, Failed: failed}
	default:
		res = o.Hooks.Run(ctx, control.HookCall{Point: PointMessageSending, Thread: thread, Scope: scope, Text: text, Files: files})
		if ctx.Err() != nil {
			return "", stuck, false
		}
	}
	k := key(r.event, r.part)
	if res.Stopped {
		err := o.Store.Update(ctx, func(tx *store.Tx) error {
			x, err := tx.ExecContext(ctx, `UPDATE chat_outbox SET state = 'cancelled', hook = 'cancelled', hook_by = ?, hook_reason = ?,
				hook_failed = ? WHERE id = ? AND state = 'pending'`, res.By, res.Reason, res.Failed, r.id)
			if err != nil {
				return err
			}
			if n, _ := x.RowsAffected(); n == 0 || thread == 0 {
				return nil
			}
			d := map[string]any{"link": link, "event": r.event, "part": r.part, "plugin": res.By}
			if res.Failed {
				d["failed"] = true
			}
			if edit {
				d["edit"] = true
			}
			if res.Reason != "" {
				d["reason"] = res.Reason
			}
			data, _ := json.Marshal(d)
			_, err = tx.Append(ctx, control.Event{Thread: thread, Scope: scope, Time: o.now(), Kind: "post_cancelled", Data: data})
			return err
		})
		if err != nil {
			if ctx.Err() == nil {
				o.Log.Error("chat outbox: recording a cancelled post", "key", k, "err", err)
			}
			return "", stuck, false
		}
		// Without the reason, which may quote the post.
		o.Log.Warn("chat outbox: a plugin cancelled a post", "key", k, "thread", int64(thread), "scope", scope,
			"plugin", res.By, "failed", res.Failed, "edit", edit)
		return "", posted, false // done: the rows behind it go on
	}
	rewritten = sql.NullString{}
	if res.Text != text {
		rewritten = sql.NullString{String: res.Text, Valid: true}
	}
	if o.record(ctx, posted, r.id, `UPDATE chat_outbox SET hook = 'passed', hook_text = ? WHERE id = ?`, rewritten, r.id) == stuck {
		return "", stuck, false
	}
	return res.Text, posted, true
}

// hooking holds the parts being hooked, by the event log's position, which
// is the instance's: the copies of one part in several links, of one
// front end or of several, which are sent by goroutines of their own, are
// hooked one after another, and the later ones take the first's result.
var hooking = struct {
	sync.Mutex
	parts map[RowKey]*partLock
}{parts: map[RowKey]*partLock{}}

type partLock struct {
	sync.Mutex
	users int // holding or waiting, under hooking's lock
}

// lockPart locks the hooking of part k and returns its unlock.
func lockPart(k RowKey) func() {
	hooking.Lock()
	l := hooking.parts[k]
	if l == nil {
		l = &partLock{}
		hooking.parts[k] = l
	}
	l.users++
	hooking.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		hooking.Lock()
		if l.users--; l.users == 0 {
			delete(hooking.parts, k)
		}
		hooking.Unlock()
	}
}

// sent records that row r was posted as ref with text, and, for a row of a
// link, writes its post_sent event in the same transaction: once per row,
// also when it was posted again after a restart.
func (o *Outbox) sent(ctx context.Context, r pending, ref surface.MessageRef, text string) outcome {
	// The service has accepted the post. Finish its local record even if
	// shutdown cancels the sender between the reply and this transaction,
	// or the next daemon would post the same row again (#995).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := o.Store.Update(ctx, func(tx *store.Tx) error {
		x, err := tx.ExecContext(ctx, `UPDATE chat_outbox SET state = 'sent', ref = ?, sent_at = ? WHERE id = ? AND state = 'pending'`,
			ref.ID, o.now().UnixMilli(), r.id)
		if err != nil {
			return err
		}
		if n, _ := x.RowsAffected(); n == 0 || r.link == 0 || !o.PostSent {
			return nil
		}
		var (
			thread int64
			scope  string
			link   control.LinkRef
		)
		err = tx.QueryRowContext(ctx, `SELECT l.thread_id, t.scope, l.client, l.channel, l.thread FROM links l
			JOIN threads t ON t.id = l.thread_id WHERE l.id = ?`, r.link).Scan(&thread, &scope, &link.Client, &link.Channel, &link.Thread)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		d := map[string]any{"link": link, "event": r.event, "part": r.part, "message": ref.ID}
		if text != "" {
			d["text"] = text
		}
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: control.ThreadID(thread), Scope: scope, Time: o.now(), Kind: "post_sent", Data: data})
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			o.Log.Error("chat outbox: recording a post", "row", r.id, "err", err)
		}
		return stuck
	}
	return posted
}

// refused gives up row r, whose post the chat service refused for good with
// cause: it would hold back the rows behind it for ever. first is the first
// message of a post of several, when the service took that one and refused a
// later one; what was posted stays. For a row of a
// link it writes the post_refused event in the same transaction, from which
// ThreadDetail.Refused is read.
func (o *Outbox) refused(ctx context.Context, conv surface.Conversation, r pending, k string, first surface.MessageRef, cause error) outcome {
	var thread control.ThreadID
	err := o.Store.Update(ctx, func(tx *store.Tx) error {
		x, err := tx.ExecContext(ctx, `UPDATE chat_outbox SET state = 'dropped', failures = failures + 1
			WHERE id = ? AND state = 'pending'`, r.id)
		if err != nil {
			return err
		}
		if n, _ := x.RowsAffected(); n == 0 || r.link == 0 {
			return nil
		}
		var (
			id    int64
			scope string
			link  control.LinkRef
		)
		err = tx.QueryRowContext(ctx, `SELECT l.thread_id, t.scope, l.client, l.channel, l.thread FROM links l
			JOIN threads t ON t.id = l.thread_id WHERE l.id = ?`, r.link).Scan(&id, &scope, &link.Client, &link.Channel, &link.Thread)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		thread = control.ThreadID(id)
		d := map[string]any{"link": link, "event": r.event, "part": r.part, "reason": cut(cause.Error(), 500)}
		if first.ID != "" {
			d["message"] = first.ID
		}
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: thread, Scope: scope, Time: o.now(), Kind: "post_refused", Data: data})
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			o.Log.Error("chat outbox: recording a refused post", "key", k, "err", err)
		}
		return stuck
	}
	o.Log.Error("chat outbox: the chat service refused a post, which is given up", "key", k, "thread", int64(thread),
		"channel", conv.Channel, "partly", first.ID != "", "err", cause)
	return posted // done: the rows behind it go on
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
