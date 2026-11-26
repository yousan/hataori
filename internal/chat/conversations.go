package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

const PrivateConversationLine = "Conversations with the supervisor are disabled in private chat threads."

// CheckLink is the core's in-process check for confined links. A thread
// opened by this client is trusted; an existing one needs a public answer
// from the driver. The core invokes it for starts and Link alike.
func (f *FrontEnd) CheckLink(ctx context.Context, l control.LinkRef, kind control.ThreadKind) error {
	if !kind.Confined() {
		return nil
	}
	if l.Client != f.Name || l.Thread == "" {
		return fmt.Errorf("chat: no existing thread here: %w", control.ErrForbidden)
	}
	var n int
	if err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM links WHERE client = ? AND channel = ? AND thread = ? AND opened = 1`,
		f.Name, l.Channel, l.Thread).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	p, ok := f.Driver.(surface.ThreadPrivacy)
	if ok {
		private, err := p.PrivateThread(ctx, surface.Conversation{Channel: l.Channel, Thread: l.Thread})
		if err == nil && !private {
			return nil
		}
	}
	return fmt.Errorf("chat: %s: %w", PrivateConversationLine, control.ErrForbidden)
}

func (f *FrontEnd) openedHere(t control.ThreadInfo, conv surface.Conversation) bool {
	for _, l := range t.Links {
		if l.Client == f.Name && l.Channel == conv.Channel && l.Thread == conv.Thread {
			return l.Opened
		}
	}
	return false
}

func (f *FrontEnd) refuseCall(in surface.Inbound, reason string) error {
	f.Log.Warn("conversation call ignored", "reason", reason, "scope", in.Ref.Conv.Channel, "author", in.Author.ID, "message", in.Ref.ID)
	return nil
}

// privateCall authorizes before writing the explanation. Refused authors
// get only the core's log line, never a post or a stored message.
func (f *FrontEnd) privateCall(in surface.Inbound, scope string) error {
	if in.Author.Kind != surface.Person {
		return f.refuseCall(in, "the author is not a person")
	}
	if f.CheckCall == nil {
		return nil
	}
	if err := f.CheckCall(scope, f.from(in.Author)); err != nil {
		return nil
	}
	return f.refusedNotice(f.context(), in, PrivateConversationLine)
}

// call handles only the calling message, without files, replies or history.
func (f *FrontEnd) call(in surface.Inbound) error {
	if in.RejectedCall {
		return f.refuseCall(in, "the author is not a person")
	}
	if !in.Call {
		return nil
	}
	if in.Author.Kind != surface.Person {
		return f.refuseCall(in, "the author is not a person")
	}
	conv := in.Ref.Conv
	sc, ok := f.Scopes[conv.Channel]
	if !ok || !sc.Enabled || !sc.Conversations || f.Converse == nil || f.CheckCall == nil {
		return nil
	}
	if err := f.CheckCall(conv.Channel, f.from(in.Author)); err != nil {
		return nil
	}
	// An order on the same calling message starts ordinary work, also in
	// an existing unlinked thread. It keeps the ordinary Start's checks.
	if o, order := parseTrigger(f.Trigger, stripOwnMentions(in)); order {
		if o.Repo == "" || o.Prompt == "" {
			return f.refuseCall(in, "the order needs a repository and prompt")
		}
		text, n, err := f.remember(f.context(), in.Ref, stripOwnMentions(in))
		if err != nil {
			return err
		}
		o, _ = parseTrigger(f.Trigger, text)
		ref := in.Ref
		_, err = f.start(f.context(), request{author: in.Author, conv: conv, thread: conv.Thread, key: in.Ref.ID, reply: &ref}, f.newWords(o), n)
		return err
	}
	if conv.Thread != "" {
		if err := f.CheckLink(f.context(), control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread}, control.KindConversation); err != nil {
			if errors.Is(err, control.ErrForbidden) {
				return f.refusedNotice(f.context(), in, PrivateConversationLine)
			}
			return err
		}
	}
	ctx := f.context()
	text, _, err := f.remember(ctx, in.Ref, stripOwnMentions(in))
	if err != nil {
		return err
	}
	if err := f.rememberSender(ctx, in.Ref.ID, sender{lamp: "message", msg: in.Ref, ping: pingOf(in.Author)}); err != nil {
		return err
	}
	id, err := f.Converse(ctx, conv.Channel, conv.Thread, in.Ref.ID, f.from(in.Author), text)
	if errors.Is(err, control.ErrForbidden) {
		return nil
	}
	if errors.Is(err, control.ErrStopped) {
		return f.refusedNotice(ctx, in, "Not sent to the agent: "+StoppedLine(err))
	}
	// A failed launch is recorded in the thread and shown by the event reader.
	// It was handled: catch-up must not try to start it with the same call.
	if errors.Is(err, surface.ErrRefused) {
		return f.refuseCall(in, "the chat service cannot open a conversation")
	}
	if err != nil && id != 0 {
		var state control.State
		if qerr := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, int64(id)).Scan(&state); qerr != nil {
			return qerr
		}
		if state == control.Failed {
			return nil
		}
	}
	return err
}

func stripOwnMentions(in surface.Inbound) string {
	var b strings.Builder
	at := 0
	for _, span := range in.OwnMentions {
		if span.Start < at || span.End < span.Start || span.End > len(in.Text) {
			continue
		}
		b.WriteString(in.Text[at:span.Start])
		at = span.End
	}
	b.WriteString(in.Text[at:])
	return strings.TrimSpace(b.String())
}

// retryCall uses the already resolved conversation rather than interpreting
// its display channel as a scope: show_in may be another channel or client.
func (f *FrontEnd) retryCall(in surface.Inbound, t control.ThreadInfo) error {
	if f.Retry == nil {
		return nil
	}
	ctx := f.context()
	text, _, err := f.remember(ctx, in.Ref, stripOwnMentions(in))
	if err != nil {
		return err
	}
	if err := f.rememberSender(ctx, in.Ref.ID, sender{lamp: "message", msg: in.Ref, ping: pingOf(in.Author)}); err != nil {
		return err
	}
	l := control.LinkRef{Client: f.Name, Channel: in.Ref.Conv.Channel, Thread: in.Ref.Conv.Thread}
	err = f.Retry(ctx, t.Scope, t.ID, in.Ref.ID, f.from(in.Author), control.Input{Text: text, Link: &l})
	if errors.Is(err, control.ErrForbidden) {
		return nil
	}
	if errors.Is(err, control.ErrStopped) {
		return f.refusedNotice(ctx, in, "Not sent to the agent: "+StoppedLine(err))
	}
	if err != nil {
		var state control.State
		if qerr := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, int64(t.ID)).Scan(&state); qerr != nil {
			return qerr
		}
		if state == control.Failed {
			return nil
		} // recorded launch failure, shown by the event reader
	}
	return err
}
