package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// The Reopen button (#670; design, sections 5.1 and 10). The owner's
// message in a closed or removed thread reaches no agent: it is held, and
// the reply to it offers the button. A press reopens the thread as the
// person who pressed, which the core allows the owner only, and then sends
// the held message, as the owner's message it is, under its own id as the
// key: a press delivered twice, or after a restart, sends it once. Anyone
// else's message gets the closed line alone, as before, and anyone else's
// press changes nothing.

// reopenToken starts a Reopen button's token: reopen:<thread>#<message>.
const reopenToken = "reopen:"

// startWait bounds how long a held message waits for the agent of its
// thread to start. One not sent by then stays held: its button sends it.
const startWait = 3 * time.Minute

// ReopenLine follows ClosedLine in the reply to the owner's message in a
// closed thread, above the Reopen button.
const ReopenLine = "Reopen it, and this message is sent to the agent."

// ReopenedLine starts the line every link of a reopened thread gets.
const ReopenedLine = "Reopened."

// Lines of a reopen that could not go on where the thread stopped.
const (
	// RestoredLine: the removed thread's directory was taken back from
	// trash.
	RestoredLine = "Its directory was taken back from trash."
	// RemadeLine: the directory was gone, and was made again.
	RemadeLine = "Its directory was gone and was made again: a new clone, or an empty directory for a supervisor's thread."
	// NewConversationLine: the next message starts a new conversation.
	NewConversationLine = "The earlier conversation is not there, so the next message starts a new one in the same directory; the agent is told that this chat thread holds the earlier exchange."
	// ReplacedLine is what a thread gets when the agent no longer has its
	// conversation (the session_replaced event).
	ReplacedLine = "The agent no longer has this thread's conversation, so a new one starts in the same directory; the agent is told that this chat thread holds the earlier exchange."
)

func reopenChoice(thread control.ThreadID, message string) surface.Choice {
	return surface.Choice{Label: "Reopen", Token: fmt.Sprintf("%s%d#%s", reopenToken, thread, message)}
}

// reopenedText is the line of a reopened event.
func reopenedText(workspace, conversation string) string {
	lines := []string{ReopenedLine}
	switch workspace {
	case control.ReopenRestored:
		lines = append(lines, RestoredLine)
	case control.ReopenRemade:
		lines = append(lines, RemadeLine)
	}
	if conversation == control.ReopenNew {
		lines = append(lines, NewConversationLine)
	}
	return strings.Join(lines, " ")
}

// closedReply answers a message in a closed or removed thread t: the
// owner's is held and gets the Reopen button, when the service shows
// buttons; anyone else's gets ClosedLine alone.
func (f *FrontEnd) closedReply(ctx context.Context, in surface.Inbound, t control.ThreadInfo) error {
	if !f.owner(in.Author) || !f.withButtons([]string{"Reopen"}, false) {
		return f.closedNotice(ctx, in)
	}
	err := f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO chat_reopen_held (client, channel, message, thread_id, conv_thread, author, ping, text, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, f.Name, in.Ref.Conv.Channel, in.Ref.ID, int64(t.ID),
			in.Ref.Conv.Thread, in.Author.ID, pingOf(in.Author), in.Text, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		f.Log.Error("follow-up: holding a message", "scope", in.Ref.Conv.Channel, "thread", in.Ref.Conv.Thread,
			"message", in.Ref.ID, "err", err)
		return err
	}
	return f.noticeOnce(ctx, in, surface.Message{Text: ClosedLine + "\n" + ReopenLine,
		Choices: []surface.Choice{reopenChoice(t.ID, in.Ref.ID)}})
}

// held is a message held in a closed thread.
type held struct {
	thread control.ThreadID
	conv   surface.Conversation
	ref    surface.MessageRef
	author surface.Author
	ping   string
	text   string
}

func (f *FrontEnd) heldMessage(ctx context.Context, channel, message string) (held, bool, error) {
	var (
		h     held
		th    int64
		convT string
	)
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT thread_id, conv_thread, author, ping, text FROM chat_reopen_held
		WHERE client = ? AND channel = ? AND message = ?`, f.Name, channel, message).Scan(&th, &convT, &h.author.ID, &h.ping, &h.text)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, err
	}
	h.thread = control.ThreadID(th)
	h.conv = surface.Conversation{Channel: channel, Thread: convT}
	h.ref = surface.MessageRef{Conv: h.conv, ID: message}
	return h, true, nil
}

// pressReopen reopens the thread a Reopen button was shown in, as the
// person who pressed, and then sends the message held there. A press that
// does nothing writes one log line; a reopen the core refused for a reason
// a person can act on is answered with it.
func (f *FrontEnd) pressReopen(s surface.Selection, rest string) {
	ctx := f.context()
	ignored := func(reason string, err error) {
		f.Log.Warn("button ignored", "reason", reason, "client", f.Name, "id", s.Author.ID,
			"channel", s.Ref.Conv.Channel, "message", s.Ref.ID, "err", err)
	}
	th, message, ok := strings.Cut(rest, "#")
	n, err := strconv.ParseInt(th, 10, 64)
	if !ok || err != nil || message == "" {
		ignored("no button of the front end's", nil)
		return
	}
	thread := control.ThreadID(n)
	if !f.owner(s.Author) {
		// The core would refuse the reopen too; the held message, the
		// owner's, is not sent on anyone else's press.
		ignored("not the owner's", nil)
		return
	}
	h, ok, err := f.heldMessage(ctx, s.Ref.Conv.Channel, message)
	if err != nil || !ok || h.thread != thread || h.conv != s.Ref.Conv {
		ignored("no message held for this button", err)
		return
	}
	key := "reopen:" + message
	if _, busy := f.pressing.LoadOrStore(key, struct{}{}); busy {
		ignored("the thread is being reopened", nil)
		return
	}
	defer f.pressing.Delete(key)
	// One press of a thread at a time, so that the second of two, for two
	// held messages, finds the thread as the first left it.
	mu, _ := f.reopening.LoadOrStore(thread, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	t, ok, err := f.threadHere(ctx, s.Ref.Conv)
	if err != nil || !ok || t.ID != thread {
		ignored("not a button of this thread", err)
		return
	}
	scopes := f.scopesIn(s.Ref.Conv.Channel)
	if t.State == control.Closed || t.State == control.Removed {
		api := f.API(control.Caller{From: f.from(s.Author), Scopes: scopes, May: control.OpRead | control.OpReopen})
		_, err := api.ReopenThread(ctx, thread)
		var refusal *control.ReopenRefused
		switch {
		case errors.Is(err, control.ErrForbidden):
			return // the core logged it
		case errors.As(err, &refusal):
			if err := f.noticeOnce(ctx, surface.Inbound{Ref: h.ref}, surface.Message{Text: "Not reopened: " + refusal.Reason},
				"reopen-refused:"+s.Ref.ID+":"+refusal.Reason); err != nil {
				f.Log.Warn("reopen button: the refusal could not be posted", "thread", int64(thread), "err", err)
			}
			return
		case err != nil:
			ignored("not reopened", err)
			return
		}
		f.Log.Info("reopen button pressed", "thread", int64(thread), "client", f.Name, "id", s.Author.ID)
	}
	// Reopened now or before: the held message goes as the follow-up it
	// was, under its own id, so that it is sent once.
	if err := f.rememberSender(ctx, message, sender{lamp: "message", msg: h.ref, ping: h.ping}); err != nil {
		ignored("not sent", err)
		return
	}
	api := f.API(control.Caller{From: f.from(h.author), Scopes: scopes, May: control.OpRead | control.OpSend})
	for deadline := time.Now().Add(startWait); ; {
		err = api.Send(ctx, thread, message, control.Input{Text: h.text,
			Link: &control.LinkRef{Client: f.Name, Channel: h.conv.Channel, Thread: h.conv.Thread}}, control.Deliver)
		if err == nil || time.Now().After(deadline) {
			break
		}
		// A session that is starting, as for another held message, takes no
		// input yet: this one waits for it.
		if t, ok, terr := f.threadHere(ctx, h.conv); terr != nil || !ok || t.State != control.Creating {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err != nil {
		if !errors.Is(err, control.ErrForbidden) {
			ignored("the held message was not sent", err)
		}
		return
	}
	if _, err := f.Outbox.Store.DB.ExecContext(ctx, `DELETE FROM chat_reopen_held WHERE client = ? AND channel = ? AND message = ?`,
		f.Name, h.conv.Channel, message); err != nil {
		f.Log.Warn("reopen button: the held message's record stays", "thread", int64(thread), "err", err)
	}
}
