package chat

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// A message in a thread that failed at its start reaches no agent (#708).
// It gets one reply: that nothing was sent, why the thread could not start,
// in the words of its "could not start" message, and what to do next. A
// start that files blocked has the choices of that message (trustbutton.go),
// as buttons of the reply too; any other is started in a new thread. The
// reply is written once the core refused the message as its author's, so
// whoever may not send here gets nothing. A message that got it is never
// sent later, also when a choice starts the thread again and the message is
// delivered again.

const (
	// FailedLine starts the reply to a message in a thread that failed at
	// its start.
	FailedLine = "Nothing was sent to the agent."
	// NewThreadLine is the next step of a start that has no choices.
	NewThreadLine = "What to do next: start the work in a new thread."
)

// failedReplyPart is the first part of the replies, rows of the failure's
// state event, so that the choice made for it finds them (chosenRows). It is
// far above the parts of what an event shows; the reply to the message with
// notice number n has part failedReplyPart+n.
const failedReplyPart = 1 << 27

// failedKey is the key in chat_notices of the reply to the message in.
func failedKey(in surface.Inbound) string { return "failed:" + in.Ref.ID }

// failedAnswered reports whether the message in got the reply.
func (f *FrontEnd) failedAnswered(ctx context.Context, in surface.Inbound) (bool, error) {
	var n int
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_notices WHERE client = ? AND channel = ? AND message = ?`,
		f.Name, in.Ref.Conv.Channel, failedKey(in)).Scan(&n)
	return n > 0, err
}

// errMovedOn is a thread that was started again or closed as its message was
// about to be answered: the message is tried again, as one in what the thread
// is now.
var errMovedOn = errors.New("chat: the thread is no longer failed")

// failedReply answers the message in, which the core refused in the failed
// thread t, once. It is errMovedOn when the thread is no longer failed: a
// choice changes the state in the transaction of its event, so a reply written
// before it is found by chosenRows, and one after it is not written, its
// buttons being of a choice already made.
func (f *FrontEnd) failedReply(ctx context.Context, in surface.Inbound, t control.ThreadInfo) error {
	fail := func(err error) error {
		f.Log.Error("follow-up: answering in a failed thread", "scope", in.Ref.Conv.Channel, "thread", in.Ref.Conv.Thread,
			"message", in.Ref.ID, "err", err)
		return err // not answered yet: try again
	}
	d, err := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: []string{t.Scope}, May: control.OpRead}).Thread(ctx, t.ID, 0, 0)
	if err != nil {
		return fail(err)
	}
	var sf startFailed
	m := surface.Message{Text: FailedLine + " " + NewThreadLine}
	if s := d.FailedStart; s != nil {
		sf = startFailed{pos: s.Event, thread: t.ID, scope: t.Scope, session: s.Session,
			reason: strings.TrimPrefix(s.Reason, "start: "), detail: s.Detail, trust: s.Trust}
		m = f.failedReplyMessage(sf, d.Repo, true)
	}
	ref := in.Ref
	m.ReplyTo = &ref
	err = f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, int64(t.ID)).Scan(&state); err != nil {
			return err
		}
		if control.State(state) != control.Failed {
			return errMovedOn
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO chat_notices (client, channel, message, received_at)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, f.Name, in.Ref.Conv.Channel, failedKey(in), time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err // answered by another delivery
		}
		n, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r := Row{Event: sf.pos, Part: failedReplyPart + int(n), Conv: in.Ref.Conv, Message: m}
		if sf.pos == 0 {
			r.Part = -int(n) // as noticeOnce's
		}
		return f.Outbox.WriteTx(ctx, tx, r)
	})
	if errors.Is(err, errMovedOn) {
		return err // nothing was said yet
	}
	if err != nil {
		return fail(err)
	}
	f.Outbox.Wake()
	return nil
}

// failedReplyMessage is the reply for the failure sf; buttons offers a
// blocked start's choices, as failedMessage does.
func (f *FrontEnd) failedReplyMessage(sf startFailed, repo string, buttons bool) surface.Message {
	m := f.failedMessage(sf, repo, buttons)
	m.Text = FailedLine + " " + m.Text
	if len(sf.trust) == 0 {
		m.Text += "\n" + NewThreadLine
	}
	return m
}

// failedReplies are the parts of the replies written for the failure at pos.
func (f *FrontEnd) failedReplies(ctx context.Context, pos uint64) ([]int, error) {
	rows, err := f.Outbox.Store.DB.QueryContext(ctx, `SELECT DISTINCT part FROM chat_outbox
		WHERE client = ? AND event = ? AND part > ? AND part < ? ORDER BY part`, f.Name, pos, failedReplyPart, threadPart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}
