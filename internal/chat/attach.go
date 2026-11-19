package chat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// attachCommand is the command that shows a thread here (design, section
// 5.2): `/attach <number>`, or `<trigger> attach <number>` on a surface
// without commands.
const attachCommand = "attach"

// attachArg returns the number of a trigger line that asks to attach.
func attachArg(o order) (string, bool) {
	if o.Agent != "" || o.Repo != "" || o.Model != "" || o.Effort != "" {
		return "", false
	}
	word, arg, _ := strings.Cut(o.Prompt, " ")
	if word != attachCommand || strings.ContainsAny(strings.TrimSpace(arg), " \t\n\r") {
		return "", false
	}
	return strings.TrimSpace(arg), true
}

// attach links thread arg ("7" or "#7") to conv: to conv itself when it is
// a chat thread, or to a chat thread the core has opened in conv's
// channel, titled like the thread. key is the request's safe-to-repeat id.
// reply, when set, is where the command waits for its answer, which only
// the person sees; without it a refusal shows nothing. Whether the person
// may attach, and whether the channel may show the thread, is the core's
// to decide (Link); a refused attach makes nothing.
func (f *FrontEnd) attach(ctx context.Context, conv surface.Conversation, author surface.Author, arg, key string, reply *surface.MessageRef) error {
	answer := func(text string) error {
		if reply == nil {
			return nil
		}
		return f.notice(ctx, conv, key, 0, surface.Message{Text: text, ReplyTo: reply})
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil || n <= 0 {
		return answer("Usage: /attach <number>, the number of a hataori thread.")
	}
	id := control.ThreadID(n)
	// Whether the thread is shown in that channel here already: attaching
	// it again changes nothing, and posts nothing there either.
	var before int
	if err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM links WHERE thread_id = ? AND client = ? AND channel = ?`,
		int64(id), f.Name, conv.Channel).Scan(&before); err != nil {
		return err
	}
	api := f.API(control.Caller{From: f.from(author), Scopes: f.scopeList(), May: control.OpRead | control.OpSend})
	l, err := api.Link(ctx, id, control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread})
	switch {
	case errors.Is(err, control.ErrForbidden):
		return answer("Not attached: thread #" + strconv.FormatInt(n, 10) + " cannot be shown here by you.")
	case errors.Is(err, control.ErrLinkedElsewhere):
		return answer("Not attached: this conversation shows another thread.")
	case err != nil && strings.Contains(err.Error(), "not found"):
		return answer("Not attached: there is no thread #" + strconv.FormatInt(n, 10) + ".")
	case err != nil:
		answer("Not attached: an error, which is in the log.")
		return err
	}
	here := surface.Conversation{Channel: l.Channel, Thread: l.Thread}
	if before > 0 {
		return answer(fmt.Sprintf("Thread #%d is shown here already.", n))
	}
	if here != conv {
		if err := f.notice(ctx, here, key, 1, surface.Message{Text: fmt.Sprintf("Thread #%d is shown here too.", n)}); err != nil {
			return err
		}
	}
	return answer(fmt.Sprintf("Thread #%d is shown here.", n))
}

// notice writes one message that answers a request of key, not an event:
// it is keyed by event 0 and the request's place in chat_notices, so the
// request delivered again writes it once. which tells two messages of one
// request apart. Its part is below every closed-thread notice's, which
// shares event 0.
func (f *FrontEnd) notice(ctx context.Context, conv surface.Conversation, key string, which int, m surface.Message) error {
	err := f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_notices (client, channel, message, received_at)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, f.Name, conv.Channel, "attach:"+key, time.Now().UnixMilli()); err != nil {
			return err
		}
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT rowid FROM chat_notices WHERE client = ? AND channel = ? AND message = ?`,
			f.Name, conv.Channel, "attach:"+key).Scan(&n); err != nil {
			return err
		}
		return f.Outbox.WriteTx(ctx, tx, Row{Event: 0, Part: -(1 << 30) - int(2*n+int64(which)), Conv: conv, Message: m})
	})
	if err != nil {
		return err
	}
	f.Outbox.Wake()
	return nil
}
