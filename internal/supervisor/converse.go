package supervisor

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// A Call asks the supervisor of a scope for a conversation (design, section
// 7.1, "Conversations in any chat thread"): a message by which a person
// spoke to it.
type Call struct {
	Scope string
	// Thread is the conversation of the scope's own channel the message was
	// written in, which is linked to the new thread; empty for a message in
	// the channel itself, on which the scope's owner opens one
	// (control.ScopeOwner.NewThread, with Key).
	Thread string
	Key    string // the message's id: the same one starts nothing twice
	// From is who wrote the message, as Caller.From: "<client>:<id>". A
	// message of a bot or a webhook is no call, also when the config lists
	// its id (design, section 5.2): that is the front end's to see, since
	// the id alone does not tell.
	From string
	// Text is what was written, with the mention of hataori's own account
	// taken out by the front end, which alone knows how its service writes
	// one. Its first line is the thread's title.
	Text string
}

// ErrNoConversations is returned by Converse for a scope in which none
// starts: one that names no supervisor, or has "conversations": false.
var ErrNoConversations = errors.New("no conversation with a supervisor starts in this scope")

// Converse starts a conversation with the supervisor of c.Scope: a thread
// of the kind control.KindConversation, in the scope's directory, with what
// the supervisor's own thread has (Confine, Config) and a token of its own,
// bound to the new thread. c.From starts it, so the core decides, as of any
// start, before a thread, a chat thread or a directory exists: a guest's
// call makes nothing and gives one log line. A call made before returns its
// thread. A thread whose session failed to start is returned with the error,
// and so is the thread of another kind whose key c.Key is, with
// core.ErrKindKey: no conversation is made then.
//
// It is for the daemon's own chat front ends; no socket reaches it.
func (s *Sessions) Converse(ctx context.Context, c Call) (control.ThreadID, error) {
	sv, ok := s.Scopes[c.Scope]
	if !ok || !sv.Conversations {
		reason := "the scope names no supervisor"
		if ok {
			reason = "the scope's supervisor has conversations turned off"
		}
		s.Log.Warn("conversation refused", "scope", c.Scope, "from", c.From, "reason", reason)
		return 0, fmt.Errorf("supervisor: scope %q: %w", c.Scope, ErrNoConversations)
	}
	if c.Key == "" {
		return 0, fmt.Errorf("supervisor: scope %q: a call without a key", c.Scope)
	}
	caller := control.Caller{From: c.From, Scopes: []string{c.Scope}, May: control.OpRead | control.OpStart}
	dir := func(ctx context.Context) (string, error) { return s.Dir(ctx, c.Scope) }
	return s.Core.StartConfined(ctx, caller, control.StartRequest{Key: c.Key, Scope: c.Scope, Thread: c.Thread,
		Input: control.Input{Text: c.Text}, Model: sv.Model, Effort: cmp.Or(sv.Effort, DefaultEffort)},
		control.KindConversation, core.Dir(dir))
}
