package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// Scope is the chat front end's config of one scope, a channel. Who may
// act in it is not the front end's to decide: the core checks every input
// against the people config (design, section 5.1).
type Scope struct {
	Enabled bool
}

// FrontEnd shows threads in one chat service. It is the surface.Sink of its
// driver and the control.ScopeOwner of its scopes.
type FrontEnd struct {
	Name    string // the client's name, as scopes name their owner
	Driver  surface.Driver
	Outbox  *Outbox
	API     func(control.Caller) control.API // the core, as one caller
	Trigger string                           // the word that starts a thread
	// People is the instance's people config. The front end uses it only
	// to find the owner: whether a direct message is the owner's, and the
	// owner's id on this service to send the login alert to. It checks no
	// input against it.
	People *people.Directory
	Scopes map[string]Scope // by channel
	Log    *slog.Logger

	// The progress line: posted after QuietAfter (90s) of a turn without
	// output, edited at most every EditEvery (15s), checked every Tick (1s).
	QuietAfter, EditEvery, Tick time.Duration
	// FileRoots returns the directories a session's files may be handed
	// over from: the thread's workspace and the session's state
	// directory. A file elsewhere, also through a symbolic link, is
	// refused; without FileRoots every file is.
	FileRoots func(ctx context.Context, thread control.ThreadID, session int64) ([]string, error)
	// FileDir is where the front end keeps the copies of the files it
	// posts, taken when it checks them. Without it no file is posted.
	FileDir string

	// Instance names the instance in the owner's login message. Relay is
	// the login relay; nil when the config switched it off, and the
	// message then has no link and no button.
	Instance string
	Relay    LoginRelay

	loginOnce sync.Once
	login     *logins

	inboundOnce sync.Once
	in          *inbound
	opening     sync.Mutex       // held while a trigger's thread is looked up or opened
	Now         func() time.Time // the clock; time.Now when nil

	ctx context.Context
}

var (
	_ surface.Sink        = (*FrontEnd)(nil)
	_ surface.Reconnected = (*FrontEnd)(nil)
	_ control.ScopeOwner  = (*FrontEnd)(nil)
)

// Run connects the driver, shows the events of its scopes and sends the
// outbox until ctx ends.
func (f *FrontEnd) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.ctx = ctx
	f.Outbox.init() // its defaults, before the goroutines below read them
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); f.runCatchUp(ctx) }()
	go func() { defer wg.Done(); f.Outbox.Run(ctx) }()
	go func() { defer wg.Done(); f.runLogins(ctx) }()
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if err := f.show(ctx); err != nil && ctx.Err() == nil {
				f.Log.Error("chat: reading events", "err", err)
			}
			select { // and start again from the stored cursor
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	err := f.Driver.Run(ctx, f)
	cancel() // the driver stopped: stop the rest too
	wg.Wait()
	return err
}

func (f *FrontEnd) context() context.Context {
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}

// NewThread opens a chat thread in the channel scope and returns its id.
// The core calls it once a Start is allowed. For a trigger's Start, whose
// key is the trigger message's id, the thread is opened from the trigger
// message (threadOf); for one another client makes, in the channel.
func (f *FrontEnd) NewThread(ctx context.Context, scope, key, title string) (string, error) {
	var n int
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_triggers WHERE client = ? AND channel = ? AND message = ?`,
		f.Name, scope, key).Scan(&n)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return f.threadOf(ctx, surface.MessageRef{Conv: surface.Conversation{Channel: scope}, ID: key}, title)
	}
	conv, err := f.Driver.OpenThread(ctx, surface.Conversation{Channel: scope}, title)
	if err != nil {
		return "", err
	}
	return conv.Thread, nil
}

// from is the id the core resolves an author's input by.
func (f *FrontEnd) from(a surface.Author) string { return people.Join(f.Name, a.ID) }

// owner reports whether author is the owner, by the people config.
func (f *FrontEnd) owner(a surface.Author) bool {
	name, ok := f.People.Resolve(f.from(a))
	return ok && name == f.People.Owner()
}

// ownerPrincipal is the principal of the front end's own reads.
func (f *FrontEnd) ownerPrincipal() control.Principal { return control.Principal(f.People.Owner()) }

// Message handles an inbound message. In a channel, a trigger starts a
// thread; in a thread, a message is a follow-up; a direct message may be
// a login code; everything else is ignored.
func (f *FrontEnd) Message(in surface.Inbound) {
	if in.Direct {
		if !in.Service {
			f.direct(in)
		}
		return
	}
	f.receive(in)
}

// handle handles a message in a channel or a thread. An error means it was
// not handled and may be tried again: the cursor stays before it.
func (f *FrontEnd) handle(in surface.Inbound) error {
	if in.Service {
		return nil
	}
	if in.Ref.Conv.Thread != "" {
		return f.followUp(in)
	}
	ctx := f.context()
	channel := in.Ref.Conv.Channel
	// A message delivered again (after an outage, or edited since) starts
	// what it started the first time.
	text, seen, err := f.stored(ctx, in.Ref)
	if err != nil {
		f.Log.Error("trigger: reading stored messages", "scope", channel, "message", in.Ref.ID, "err", err)
		return err
	}
	if !seen {
		text = in.Text
	}
	o, ok := parseTrigger(f.Trigger, text)
	if !ok {
		return nil
	}
	refuse := func(reason string) {
		f.Log.Warn("trigger ignored", "reason", reason, "scope", channel,
			"author", in.Author.ID, "message", in.Ref.ID)
	}
	// Whether the author may start a thread is the core's to decide, in
	// Start; a refused trigger opens nothing.
	sc, known := f.Scopes[channel]
	switch {
	case in.Author.Kind == surface.Bot:
		refuse("the author is a bot account")
		return nil
	case !known || !sc.Enabled:
		refuse("the scope is not enabled")
		return nil
	case o.Repo == "":
		refuse("no repo:<url> on the trigger line")
		return nil
	case o.Prompt == "":
		refuse("no prompt")
		return nil
	}
	var n int64
	if text, n, err = f.remember(ctx, in.Ref, text); err != nil {
		f.Log.Error("trigger: storing the message", "scope", channel, "message", in.Ref.ID, "err", err)
		return err
	}
	if o, ok = parseTrigger(f.Trigger, text); !ok {
		return nil // stored by a concurrent delivery of another text
	}
	return f.start(ctx, in, o, n)
}

// refuseEffort answers a trigger whose effort the core does not know. It
// is not about an event: the row is keyed by event 0 and the trigger's
// number n, so a second delivery writes it once.
func (f *FrontEnd) refuseEffort(ctx context.Context, in surface.Inbound, o order, n int64) error {
	f.Log.Warn("trigger ignored", "reason", "unknown effort "+strconv.Quote(o.Effort), "scope", in.Ref.Conv.Channel,
		"author", in.Author.ID, "message", in.Ref.ID)
	err := f.Outbox.Write(ctx, Row{Event: 0, Part: int(n), Conv: in.Ref.Conv, Message: surface.Message{
		Text:    "Nothing was started: effort " + strconv.Quote(o.Effort) + " is not one of " + strings.Join(Efforts, ", ") + ".",
		ReplyTo: &in.Ref}})
	if err != nil {
		f.Log.Error("trigger: writing the refusal", "message", in.Ref.ID, "err", err)
	}
	return err
}

// stored returns the text of a trigger message as it was first received.
func (f *FrontEnd) stored(ctx context.Context, ref surface.MessageRef) (string, bool, error) {
	var text string
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT text FROM chat_triggers WHERE client = ? AND channel = ? AND message = ?`,
		f.Name, ref.Conv.Channel, ref.ID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return text, err == nil, err
}

// remember stores the text of a trigger message the first time it is
// received, and returns the text stored and the trigger's number.
func (f *FrontEnd) remember(ctx context.Context, ref surface.MessageRef, text string) (string, int64, error) {
	var n int64
	err := f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO chat_triggers (client, channel, message, text, received_at)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, f.Name, ref.Conv.Channel, ref.ID, text, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT rowid, text FROM chat_triggers WHERE client = ? AND channel = ? AND message = ?`,
			f.Name, ref.Conv.Channel, ref.ID).Scan(&n, &text)
	})
	return text, n, err
}

func (f *FrontEnd) start(ctx context.Context, in surface.Inbound, o order, n int64) error {
	channel := in.Ref.Conv.Channel
	api := f.API(control.Caller{From: f.from(in.Author), Scopes: []string{channel},
		May: control.OpRead | control.OpStart})
	// No thread is named: once the core allows the Start, it asks this
	// front end for one (NewThread), which opens it from the trigger.
	id, startErr := api.Start(ctx, control.StartRequest{Key: in.Ref.ID, Scope: channel, Repo: o.Repo,
		Model: o.Model, Effort: o.Effort, Input: control.Input{Text: o.Prompt}})
	switch {
	case errors.Is(startErr, control.ErrForbidden):
		return nil // the core logged it; nothing is shown to the author
	case errors.Is(startErr, control.ErrUnknownEffort):
		return f.refuseEffort(ctx, in, o, n)
	}
	if id.Key == "" {
		f.Log.Error("trigger: start failed", "scope", channel, "message", in.Ref.ID, "err", startErr)
		return startErr
	}
	// The thread's rows are keyed by its first event, and what they show
	// is read from the thread's events, so a trigger delivered again
	// writes the same rows, which the outbox keeps once.
	pos, failure, err := startOutcome(ctx, api, id)
	if err != nil {
		f.Log.Error("trigger: reading the new thread", "thread", id.Key, "err", err)
		return err
	}
	if failure == "" && startErr != nil {
		failure = startErr.Error() // not recorded as the session's end
	}
	conv := surface.Conversation{Channel: channel, Thread: id.Key}
	rows := []Row{{Event: pos, Part: 0, Conv: conv, Message: surface.Message{Text: o.Prompt, Quiet: true}}}
	if failure != "" {
		rows = append(rows, Row{Event: pos, Part: 1, Conv: conv,
			Message: surface.Message{Text: "This thread could not start: " + failure}})
	}
	if err := f.Outbox.Write(ctx, rows...); err != nil {
		f.Log.Error("trigger: writing the prompt", "thread", id.Key, "err", err)
		return err
	}
	return nil
}

// threadOf returns the chat thread of the trigger message ref: the one
// opened for it before, or one opened now from the message and stored
// with it, so that the trigger delivered again opens no second thread.
// Its title is the prompt's first line; the driver cuts it to what the
// service takes.
func (f *FrontEnd) threadOf(ctx context.Context, ref surface.MessageRef, prompt string) (string, error) {
	f.opening.Lock()
	defer f.opening.Unlock()
	db := f.Outbox.Store.DB
	var thread string
	if err := db.QueryRowContext(ctx, `SELECT thread FROM chat_triggers WHERE client = ? AND channel = ? AND message = ?`,
		f.Name, ref.Conv.Channel, ref.ID).Scan(&thread); err != nil {
		return "", err
	}
	if thread != "" {
		return thread, nil
	}
	title, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	conv, err := f.Driver.OpenThread(ctx, surface.Conversation{Channel: ref.Conv.Channel, Thread: ref.ID}, strings.TrimSpace(title))
	if err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `UPDATE chat_triggers SET thread = ? WHERE client = ? AND channel = ? AND message = ?`,
		conv.Thread, f.Name, ref.Conv.Channel, ref.ID); err != nil {
		return "", err
	}
	return conv.Thread, nil
}

var errNoEvent = errors.New("the thread has no events")

// startOutcome reads a new thread's stored events: the position of its
// first, and why its first session failed before it started, if it did.
func startOutcome(ctx context.Context, api control.API, id control.ThreadID) (first uint64, failure string, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := api.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		return 0, "", err
	}
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		if first == 0 {
			first = e.Pos
		}
		if e.Kind != "state" || failure != "" {
			continue
		}
		var d struct{ From, To, Reason string }
		if json.Unmarshal(e.Data, &d) == nil && d.From == string(control.Creating) && d.To == string(control.Failed) {
			failure = strings.TrimPrefix(d.Reason, "start: ")
		}
	}
	if first == 0 {
		return 0, "", fmt.Errorf("chat: thread %s: %w", id.Key, errNoEvent)
	}
	return first, failure, nil
}

// followUp sends a message in a thread to the thread's session. A thread
// hataori does not know gets nothing.
func (f *FrontEnd) followUp(in surface.Inbound) error {
	if in.Author.Kind == surface.Bot {
		return nil // the front end's own posts come back this way
	}
	ctx := f.context()
	conv := in.Ref.Conv
	if _, known := f.Scopes[conv.Channel]; !known {
		return nil
	}
	api := f.API(control.Caller{From: f.from(in.Author), Scopes: []string{conv.Channel},
		May: control.OpRead | control.OpSend})
	id := control.ThreadID{Scope: conv.Channel, Key: conv.Thread}
	ts, err := api.Threads(ctx, control.Filter{Thread: id})
	if err != nil {
		return err // not known yet whether it is one of ours: try again
	}
	if len(ts) == 0 {
		return nil // not a hataori thread
	}
	switch ts[0].State {
	case control.Failed, control.Closed, control.Removed:
		// Its session cannot take input; the message is not for later.
		f.Log.Warn("follow-up ignored", "reason", "the thread is "+string(ts[0].State), "scope", conv.Channel,
			"thread", conv.Thread, "message", in.Ref.ID)
		return nil
	}
	// The key makes a second delivery one input; a running turn gets it
	// delivered, an open question gets it as the answer. Whether the author
	// may give input at all, the core decides too.
	err = api.Send(ctx, id, in.Ref.ID, control.Input{Text: in.Text}, control.Deliver)
	if errors.Is(err, control.ErrForbidden) {
		return nil // the core logged it
	}
	if err != nil {
		f.Log.Error("follow-up: send failed", "scope", conv.Channel, "thread", conv.Thread, "message", in.Ref.ID, "err", err)
		return err
	}
	return nil
}

// Command handles a command; there are none yet.
func (f *FrontEnd) Command(surface.Command) {}
