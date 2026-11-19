package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// The commands (design, section 10): /new starts a thread, /stop stops the
// running turn of the thread it is used in, /status shows a thread or the
// threads of a channel's scope. On a surface without commands the same
// words follow the trigger: `<trigger> new repo:<url> <prompt>`,
// `<trigger> stop`, `<trigger> status`, and `/dashboard` (dashboard.go)
// with `<trigger> dashboard`. Whether the person may do any of
// it is the core's to decide: each call is made with From set to the
// author's id, and a refused call shows nothing.
const (
	newCommand    = "new"
	stopCommand   = "stop"
	statusCommand = "status"
)

// commandKey starts the Start key of a /new command, and the name its
// chat_triggers row is kept under: the command's id is no message, so its
// thread is opened in the channel alone.
const commandKey = "cmd:"

// statusMax is how many threads a channel's status lists at most, the
// newest ones.
const statusMax = 20

// Commands returns the commands the front end handles; it makes it a
// surface.Commander.
func (f *FrontEnd) Commands() []surface.CommandSpec {
	return []surface.CommandSpec{{
		Name:        newCommand,
		Description: "Start a hataori thread",
		Args: []surface.ArgSpec{
			{Name: "repository", Description: "The repository to clone", Required: true},
			{Name: "prompt", Description: "What the agent should do", Required: true},
			{Name: "model", Description: "The model; the channel's default when left out"},
			{Name: "effort", Description: "The effort: " + strings.Join(Efforts, ", ")},
		},
		Private: true,
	}, {
		Name:        stopCommand,
		Description: "Stop the running turn of this hataori thread",
		Private:     true,
	}, {
		Name:        statusCommand,
		Description: "Show this hataori thread, or the threads of this channel",
		Private:     true,
	}, {
		Name:        dashboardCommand,
		Description: "Get a dashboard sign-in link in a direct message",
		Private:     true,
	}, {
		Name:        attachCommand,
		Description: "Show a hataori thread here too",
		Args:        []surface.ArgSpec{{Name: "number", Description: "The thread's number", Required: true}},
		Private:     true,
	}}
}

var _ surface.Commander = (*FrontEnd)(nil)

// Command handles a command.
func (f *FrontEnd) Command(c surface.Command) {
	if err := f.command(f.context(), c); err != nil {
		f.Log.Error("command failed", "command", c.Name, "channel", c.Conv.Channel, "id", c.ID, "err", err)
	}
}

// command handles a command, given as a command or in words after the
// trigger. A bot account's is no one's request.
func (f *FrontEnd) command(ctx context.Context, c surface.Command) error {
	if c.Author.Kind == surface.Bot {
		return nil
	}
	switch c.Name {
	case newCommand:
		return f.newThread(ctx, c)
	case stopCommand:
		return f.stopTurn(ctx, c)
	case statusCommand:
		return f.status(ctx, c)
	case attachCommand:
		return f.attach(ctx, c.Conv, c.Author, argOf(c, "number"), c.ID, c.Reply)
	case dashboardCommand:
		return f.dashboardLink(ctx, c)
	}
	return nil
}

func argOf(c surface.Command, name string) string {
	for _, a := range c.Args {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

// textCommand returns the stop, status or dashboard command a message gives
// in words after the trigger, on a surface without commands.
func (f *FrontEnd) textCommand(in surface.Inbound) (surface.Command, bool) {
	if f.Driver.Limits().Commands {
		return surface.Command{}, false
	}
	o, ok := parseTrigger(f.Trigger, in.Text)
	if !ok || o.Agent != "" || o.Repo != "" || o.Model != "" || o.Effort != "" || (o.Prompt != stopCommand && o.Prompt != statusCommand && o.Prompt != dashboardCommand) {
		return surface.Command{}, false
	}
	ref := in.Ref
	return surface.Command{Conv: in.Ref.Conv, Author: in.Author, ID: in.Ref.ID, Name: o.Prompt, Reply: &ref}, true
}

// newWords reads `<trigger> new ...` on a surface without commands as the
// trigger without the word.
func (f *FrontEnd) newWords(o order) order {
	word, rest, _ := strings.Cut(o.Prompt, " ")
	if f.Driver.Limits().Commands || o.Agent != "" || o.Repo != "" || o.Model != "" || o.Effort != "" || strings.TrimSpace(word) != newCommand {
		return o
	}
	n, _ := parseTrigger(f.Trigger, f.Trigger+" "+rest)
	return n
}

// answer answers command c in place, once however often c is delivered.
// A command with no Reply waits for no answer, and gets none: posted in
// the conversation, an answer meant for one person would be shown to all.
func (f *FrontEnd) answer(ctx context.Context, c surface.Command, text string) error {
	if c.Reply == nil {
		f.Log.Warn("command answer dropped", "reason", "the command waits for no answer", "command", c.Name,
			"channel", c.Conv.Channel, "id", c.ID)
		return nil
	}
	return f.notice(ctx, c.Conv, c.ID, 0, surface.Message{Text: text, ReplyTo: c.Reply})
}

// once records key in channel and reports whether it was new; with record
// false it forgets key again.
func (f *FrontEnd) once(ctx context.Context, channel, key string, record bool) (bool, error) {
	db := f.Outbox.Store.DB
	if !record {
		_, err := db.ExecContext(ctx, `DELETE FROM chat_notices WHERE client = ? AND channel = ? AND message = ?`, f.Name, channel, key)
		return false, err
	}
	res, err := db.ExecContext(ctx, `INSERT INTO chat_notices (client, channel, message, received_at) VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, f.Name, channel, key, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ignored logs one line for a command that does nothing.
func (f *FrontEnd) ignored(c surface.Command, reason string) {
	f.Log.Warn("command ignored", "command", c.Name, "reason", reason, "channel", c.Conv.Channel,
		"author", c.Author.ID, "id", c.ID)
}

// newThread starts a thread in the channel c is used in, as a trigger
// does: the core decides whether the author may, and only then is the chat
// thread opened, in the channel, and the full prompt posted in it.
func (f *FrontEnd) newThread(ctx context.Context, c surface.Command) error {
	channel := c.Conv.Channel
	o := order{Repo: strings.TrimSpace(argOf(c, "repository")), Prompt: strings.TrimSpace(argOf(c, "prompt")),
		Model: strings.TrimSpace(argOf(c, "model")), Effort: strings.TrimSpace(argOf(c, "effort"))}
	sc, known := f.Scopes[channel]
	switch {
	case !known || !sc.Enabled:
		f.ignored(c, "the scope is not enabled")
		return nil
	case o.Repo == "" || o.Prompt == "":
		f.ignored(c, "no repository or no prompt")
		return nil
	}
	ref := surface.MessageRef{Conv: surface.Conversation{Channel: channel}, ID: commandKey + c.ID}
	prompt, n, err := f.remember(ctx, ref, o.Prompt)
	if err != nil {
		return err
	}
	o.Prompt = prompt // as first received
	id, err := f.start(ctx, request{author: c.Author, conv: c.Conv, key: ref.ID, reply: c.Reply}, o, n)
	if err != nil || id == 0 {
		return err
	}
	return f.answer(ctx, c, fmt.Sprintf("Started thread #%d.", id))
}

// threadHere returns the thread conv shows, found by the front end's own
// read: who asks is checked by the call made for them.
func (f *FrontEnd) threadHere(ctx context.Context, conv surface.Conversation) (control.ThreadInfo, bool, error) {
	scopes := f.scopesIn(conv.Channel)
	if conv.Thread == "" || len(scopes) == 0 {
		return control.ThreadInfo{}, false, nil
	}
	return f.threadIn(ctx, f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: scopes, May: control.OpRead}), conv)
}

// stopTurn stops the running turn of the thread c is used in.
func (f *FrontEnd) stopTurn(ctx context.Context, c surface.Command) error {
	t, ok, err := f.threadHere(ctx, c.Conv)
	if err != nil {
		return err
	}
	if !ok {
		return f.answer(ctx, c, "This is no hataori thread: nothing was stopped.")
	}
	// A stop delivered again must not stop a turn started since: each
	// command stops once.
	first, err := f.once(ctx, c.Conv.Channel, "stop:"+c.ID, true)
	if err != nil || !first {
		return err
	}
	api := f.API(control.Caller{From: f.from(c.Author), Scopes: f.scopesIn(c.Conv.Channel), May: control.OpRead | control.OpStop})
	err = api.Stop(ctx, t.ID)
	switch {
	case errors.Is(err, control.ErrForbidden):
		return nil // the core logged it; nothing is shown
	case err != nil:
		f.once(ctx, c.Conv.Channel, "stop:"+c.ID, false) // to be tried again
		f.answer(ctx, c, "Not stopped: an error, which is in the log.")
		return err
	case t.State == control.Running:
		return f.answer(ctx, c, fmt.Sprintf("Stopped the running turn of thread #%d.", t.ID))
	}
	return f.answer(ctx, c, fmt.Sprintf("Thread #%d has no running turn: it is %s.", t.ID, t.State))
}

// status answers with the thread c is used in, or, in a channel, with the
// threads of the channel's own scope; never with a thread of another
// scope there, even one shown in the channel.
func (f *FrontEnd) status(ctx context.Context, c surface.Command) error {
	api := f.API(control.Caller{From: f.from(c.Author), Scopes: f.scopesIn(c.Conv.Channel), May: control.OpRead,
		ReadAsPerson: true})
	if c.Conv.Thread != "" {
		t, ok, err := f.threadHere(ctx, c.Conv)
		if err != nil {
			return err
		}
		if !ok {
			return f.answer(ctx, c, "This is no hataori thread.")
		}
		d, err := api.Thread(ctx, t.ID, 0, 0)
		if errors.Is(err, control.ErrForbidden) {
			return nil // the core logged it
		}
		if err != nil {
			return err
		}
		return f.answer(ctx, c, threadStatus(d.ThreadInfo))
	}
	if _, own := f.Scopes[c.Conv.Channel]; !own {
		return f.answer(ctx, c, "This channel has no hataori threads of its own.")
	}
	ts, err := api.Threads(ctx, control.Filter{Scope: c.Conv.Channel})
	if errors.Is(err, control.ErrForbidden) {
		return nil // the core logged it
	}
	if err != nil {
		return err
	}
	return f.answer(ctx, c, channelStatus(ts))
}

// threadStatus shows one thread: its state, model and last output.
func threadStatus(t control.ThreadInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Thread #%d: %s", t.ID, t.State)
	if t.Model != "" {
		fmt.Fprintf(&b, "\nModel: %s", t.Model)
		if t.Effort != "" {
			fmt.Fprintf(&b, ", effort %s", t.Effort)
		}
	}
	if t.Last != "" {
		fmt.Fprintf(&b, "\nLast: %s", t.Last)
	}
	return b.String()
}

// channelStatus lists the open threads of a scope, newest last, with their
// states, and counts the closed ones.
func channelStatus(ts []control.ThreadInfo) string {
	var open []control.ThreadInfo
	closed := 0
	for _, t := range ts {
		if t.State == control.Closed || t.State == control.Removed {
			closed++
			continue
		}
		open = append(open, t)
	}
	var b strings.Builder
	if len(open) == 0 {
		b.WriteString("No open threads here.")
	} else {
		b.WriteString("Threads here:")
	}
	if len(open) > statusMax {
		fmt.Fprintf(&b, "\n(%d older ones not shown)", len(open)-statusMax)
		open = open[len(open)-statusMax:]
	}
	for _, t := range open {
		fmt.Fprintf(&b, "\n#%d %s", t.ID, t.State)
		if title, _, _ := strings.Cut(t.Title, "\n"); title != "" {
			fmt.Fprintf(&b, ": %s", title)
		}
	}
	if closed > 0 {
		fmt.Fprintf(&b, "\nAnd %d closed.", closed)
	}
	return b.String()
}
