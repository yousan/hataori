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
	mu      sync.Mutex
	convs   map[surface.Conversation]*sync.Mutex // one message at a time per conversation
	behind  map[surface.Conversation]bool        // a message failed: only catch-up moves the cursor
	pending chan struct{}
}

func (f *FrontEnd) inboundState() *inbound {
	f.inboundOnce.Do(func() {
		f.in = &inbound{convs: map[surface.Conversation]*sync.Mutex{}, behind: map[surface.Conversation]bool{},
			pending: make(chan struct{}, 1)}
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

func (s *inbound) isBehind(c surface.Conversation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.behind[c]
}

// receive handles a live message and moves its conversation's cursor,
// unless an earlier message there is still to be handled.
func (f *FrontEnd) receive(in surface.Inbound) {
	s := f.inboundState()
	conv := in.Ref.Conv
	defer s.lock(conv)()
	if err := f.handle(in); err != nil {
		s.setBehind(conv, true)
		return
	}
	if !s.isBehind(conv) && in.Cursor != "" {
		f.setCursor(f.context(), conv, in.Cursor)
	}
}

// Connected catches up on every enabled channel and open thread. The
// driver calls it after it connects and after each reconnect.
func (f *FrontEnd) Connected() {
	go f.catchUpAll(f.context())
}

func (f *FrontEnd) catchUpAll(ctx context.Context) {
	var convs []surface.Conversation
	for ch, sc := range f.Scopes {
		if sc.Enabled {
			convs = append(convs, surface.Conversation{Channel: ch})
		}
	}
	api := f.API(control.Caller{Principal: f.Owner, Scopes: f.scopeList(), May: control.OpRead})
	threads, err := api.Threads(ctx, control.Filter{})
	if err != nil {
		f.Log.Error("chat: catch-up: listing threads", "err", err)
	}
	for _, t := range threads {
		switch t.State {
		case control.Closed, control.Removed, control.Failed:
		default:
			convs = append(convs, convOf(t.ID))
		}
	}
	for _, c := range convs {
		if ctx.Err() != nil {
			return
		}
		f.catchUp(ctx, c)
	}
}

// catchUp handles the messages of conv after its cursor, in order, and
// stops at the first that cannot be handled.
func (f *FrontEnd) catchUp(ctx context.Context, conv surface.Conversation) {
	s := f.inboundState()
	defer s.lock(conv)()
	cursor, ok, err := f.cursor(ctx, conv)
	if err != nil {
		f.Log.Error("chat: catch-up: reading the cursor", "channel", conv.Channel, "thread", conv.Thread, "err", err)
		s.setBehind(conv, true)
		return
	}
	if !ok && conv.Thread == "" {
		// A channel never read: its old messages are not orders to run
		// now. Its cursor starts with the next message.
		return
	}
	msgs, err := f.Driver.Since(ctx, conv, cursor)
	if err != nil {
		f.Log.Warn("chat: catch-up: the driver gave no messages", "channel", conv.Channel, "thread", conv.Thread, "err", err)
		s.setBehind(conv, true)
		return
	}
	for _, in := range msgs {
		if in.Direct {
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

func (f *FrontEnd) cursor(ctx context.Context, conv surface.Conversation) (string, bool, error) {
	var c string
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT cursor FROM chat_inbound WHERE client = ? AND channel = ? AND thread = ?`,
		f.Name, conv.Channel, conv.Thread).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return c, err == nil, err
}

func (f *FrontEnd) setCursor(ctx context.Context, conv surface.Conversation, cursor string) error {
	_, err := f.Outbox.Store.DB.ExecContext(ctx, `INSERT INTO chat_inbound (client, channel, thread, cursor) VALUES (?, ?, ?, ?)
		ON CONFLICT (client, channel, thread) DO UPDATE SET cursor = excluded.cursor`, f.Name, conv.Channel, conv.Thread, cursor)
	if err != nil {
		f.Log.Error("chat: storing a cursor", "channel", conv.Channel, "thread", conv.Thread, "err", err)
	}
	return err
}
