package chat

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// Catch-up (design, section 4.4): for each enabled channel and each open
// thread, the front end keeps the cursor of the last message it handled,
// with every message before it, and after a start or a reconnect asks the
// driver for the messages after it. A message that cannot be handled
// leaves the cursor before it, and the conversation is caught up again
// later; one that is handled twice (live and caught up) is one input,
// because the core takes each message id once.
type inbound struct {
	mu     sync.Mutex
	convs  map[surface.Conversation]*sync.Mutex // one message at a time per conversation
	behind map[surface.Conversation]bool        // a message failed: only catch-up moves the cursor
	// gone: the driver said the conversation no longer exists. It is not
	// caught up again while the front end runs.
	gone    map[surface.Conversation]bool
	pending chan struct{}
	// unsure: the open threads could not be listed, so every thread counts
	// as behind until they are.
	unsure bool
	// since is when the driver first connected: the start of a channel
	// never read before, also when storing it has to be tried again.
	since int64
}

func (f *FrontEnd) inboundState() *inbound {
	f.inboundOnce.Do(func() {
		f.in = &inbound{convs: map[surface.Conversation]*sync.Mutex{}, behind: map[surface.Conversation]bool{},
			gone: map[surface.Conversation]bool{}, pending: make(chan struct{}, 1)}
	})
	return f.in
}

func (s *inbound) lock(c surface.Conversation) func() {
	s.mu.Lock()
	m := s.convs[c]
	if m == nil {
		m = &sync.Mutex{}
		s.convs[c] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *inbound) setBehind(c surface.Conversation, b bool) {
	s.mu.Lock()
	if b {
		s.behind[c] = true
	} else {
		delete(s.behind, c)
	}
	s.mu.Unlock()
	if b {
		select {
		case s.pending <- struct{}{}:
		default:
		}
	}
}

func (s *inbound) firstConnected() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.since == 0 {
		s.since = time.Now().UnixMilli()
	}
	return s.since
}

// setGone marks c as gone and no longer behind. It reports whether c was
// not marked gone before.
func (s *inbound) setGone(c surface.Conversation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.behind, c)
	if s.gone[c] {
		return false
	}
	s.gone[c] = true
	return true
}

func (s *inbound) isGone(c surface.Conversation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone[c]
}

func (s *inbound) isBehind(c surface.Conversation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.behind[c]
}

// receive handles a live message and moves its conversation's cursor.
// While the conversation is behind (catching up, or an earlier message
// failed), the message is left to catch-up, which takes it in its order.
func (f *FrontEnd) receive(in surface.Inbound) {
	s := f.inboundState()
	conv := in.Ref.Conv
	defer s.lock(conv)()
	s.mu.Lock()
	unsure := s.unsure && conv.Thread != ""
	s.mu.Unlock()
	if unsure || s.isBehind(conv) {
		s.setBehind(conv, true) // and catch up soon
		return
	}
	if err := f.handle(in); err != nil {
		s.setBehind(conv, true)
		return
	}
	if in.Cursor != "" {
		f.setCursor(f.context(), conv, in.Cursor)
	}
}

// Connected catches up on every enabled channel and open thread, and
// records the names of the scopes' channels and the chat's layout. The driver calls it after it
// connects and after each reconnect. Until a
// conversation is caught up, its live messages wait for the catch-up.
func (f *FrontEnd) Connected() {
	ctx := f.context()
	s := f.inboundState()
	s.mu.Lock()
	if s.since == 0 {
		s.since = time.Now().UnixMilli()
	}
	s.mu.Unlock()
	f.bg.Add(1)
	go func() {
		defer f.bg.Done()
		f.nameScopes(ctx)
	}()
	f.LayoutChanged()
	convs, err := f.toCatchUp(ctx)
	// Marked behind, and sure of the threads, at once: no live message
	// gets in between.
	s.mu.Lock()
	for _, c := range convs {
		s.behind[c] = true
	}
	s.unsure = err != nil
	s.mu.Unlock()
	if err != nil {
		// Which threads are open is not known: all of it again, later.
		retry := f.Outbox.Retry
		if retry <= 0 {
			retry = 5 * time.Second
		}
		f.bg.Add(1)
		go func() {
			defer f.bg.Done()
			select {
			case <-ctx.Done():
			case <-time.After(retry):
				if ctx.Err() == nil {
					f.Connected()
				}
			}
		}()
	}
	f.bg.Add(1)
	go func() {
		defer f.bg.Done()
		for _, c := range convs {
			if ctx.Err() != nil {
				return
			}
			f.catchUp(ctx, c)
		}
	}()
}

// toCatchUp returns the enabled channels and the open threads. A channel
// never read before is read from now on: what was posted in it before is
// not an order for this front end.
func (f *FrontEnd) toCatchUp(ctx context.Context) ([]surface.Conversation, error) {
	var convs []surface.Conversation
	for ch, sc := range f.Scopes {
		if sc.Enabled {
			convs = append(convs, surface.Conversation{Channel: ch})
			if _, err := f.Outbox.Store.DB.ExecContext(ctx, `INSERT INTO chat_inbound (client, channel, thread, cursor, since)
				VALUES (?, ?, '', '', ?) ON CONFLICT DO NOTHING`, f.Name, ch, f.inboundState().firstConnected()); err != nil {
				f.Log.Error("chat: catch-up: a new channel", "channel", ch, "err", err)
			}
		}
	}
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpRead})
	threads, err := api.Threads(ctx, control.Filter{})
	if err != nil {
		f.Log.Error("chat: catch-up: listing threads", "err", err)
		return convs, err
	}
	for _, t := range threads {
		switch t.State {
		case control.Closed, control.Removed, control.Failed:
		default:
			// Each linked conversation, from its own cursor.
			convs = append(convs, f.linkConvs(t)...)
		}
	}
	return convs, nil
}

// catchUp handles the messages of conv after its cursor, in order, and
// stops at the first that cannot be handled. A conversation the driver
// says is gone is not asked for again.
func (f *FrontEnd) catchUp(ctx context.Context, conv surface.Conversation) {
	s := f.inboundState()
	defer s.lock(conv)()
	if s.isGone(conv) {
		s.setGone(conv)
		return
	}
	cursor, since, ok, err := f.cursor(ctx, conv)
	if err != nil {
		f.Log.Error("chat: catch-up: reading the cursor", "channel", conv.Channel, "thread", conv.Thread, "err", err)
		s.setBehind(conv, true)
		return
	}
	if !ok && conv.Thread == "" {
		// A channel never read: what was posted in it before is not an
		// order for this front end. From now on it is.
		if _, err := f.Outbox.Store.DB.ExecContext(ctx, `INSERT INTO chat_inbound (client, channel, thread, cursor, since)
			VALUES (?, ?, '', '', ?) ON CONFLICT DO NOTHING`, f.Name, conv.Channel, s.firstConnected()); err != nil {
			s.setBehind(conv, true)
			return
		}
		cursor, since = "", s.firstConnected()
	}
	msgs, err := f.Driver.Since(ctx, conv, cursor)
	if errors.Is(err, surface.ErrGone) {
		if s.setGone(conv) {
			f.Log.Warn("chat: catch-up: the conversation is gone; it is not caught up again", "channel", conv.Channel, "thread", conv.Thread, "err", err)
		}
		return
	}
	if err != nil {
		f.Log.Warn("chat: catch-up: the driver gave no messages", "channel", conv.Channel, "thread", conv.Thread, "err", err)
		s.setBehind(conv, true)
		return
	}
	for _, in := range msgs {
		if in.Direct || (cursor == "" && since > 0 && in.Time.UnixMilli() < since) {
			continue
		}
		if err := f.handle(in); err != nil {
			s.setBehind(conv, true)
			return
		}
		if in.Cursor != "" {
			if err := f.setCursor(ctx, conv, in.Cursor); err != nil {
				s.setBehind(conv, true)
				return
			}
		}
	}
	s.setBehind(conv, false)
}

// runCatchUp catches up again, every retry, on conversations where a
// message could not be handled, until ctx ends.
func (f *FrontEnd) runCatchUp(ctx context.Context) {
	s := f.inboundState()
	retry := f.Outbox.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.pending:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		s.mu.Lock()
		var convs []surface.Conversation
		for c := range s.behind {
			convs = append(convs, c)
		}
		s.mu.Unlock()
		for _, c := range convs {
			f.catchUp(ctx, c)
		}
	}
}

// cursor returns conv's cursor, and for a channel without one yet the
// time from which its messages are caught up on.
func (f *FrontEnd) cursor(ctx context.Context, conv surface.Conversation) (string, int64, bool, error) {
	var c string
	var since int64
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT cursor, since FROM chat_inbound WHERE client = ? AND channel = ? AND thread = ?`,
		f.Name, conv.Channel, conv.Thread).Scan(&c, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return c, since, err == nil, err
}

func (f *FrontEnd) setCursor(ctx context.Context, conv surface.Conversation, cursor string) error {
	_, err := f.Outbox.Store.DB.ExecContext(ctx, `INSERT INTO chat_inbound (client, channel, thread, cursor) VALUES (?, ?, ?, ?)
		ON CONFLICT (client, channel, thread) DO UPDATE SET cursor = excluded.cursor`, f.Name, conv.Channel, conv.Thread, cursor)
	if err != nil {
		f.Log.Error("chat: storing a cursor", "channel", conv.Channel, "thread", conv.Thread, "err", err)
	}
	return err
}
