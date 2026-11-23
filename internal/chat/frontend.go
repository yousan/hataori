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
	// Supervisor is what the front end does with the posts of the
	// scope's supervisor (#657).
	Supervisor Posts
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
	// ShowIn lists, by channel of this client, the scopes of other channels
	// whose show_in names it: their threads may be attached there.
	ShowIn map[string][]string
	// ArchiveWhenAsleep holds the scopes, of any client, whose threads'
	// chat threads are archived when the thread goes asleep or is closed,
	// and un-archived when it is woken (archiveRows): also a chat thread
	// of this client that shows another client's scope.
	ArchiveWhenAsleep map[string]bool
	Log               *slog.Logger

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
	// SignInWay is the dashboard listener a sign-in link asked for in chat
	// is for: control.WayWeb when the instance has a web listener, since a
	// loopback link is of no use on a phone; empty otherwise.
	SignInWay control.Way

	loginOnce sync.Once
	login     *logins

	inboundOnce sync.Once
	in          *inbound
	opening     sync.Mutex       // held while a trigger's thread is looked up or opened
	pressing    sync.Map         // the Stop buttons (stopKey) whose press is being handled
	renames     renames          // the names of conversations still to record
	layout      layoutState      // the chat's layout being recorded (layout.go)
	Now         func() time.Time // the clock; time.Now when nil

	// RetryFirst is how long the front end waits before it runs its
	// driver again after the driver's Run failed, doubling at each failure
	// in a row up to RetryMax; 1s and 1 min when zero. While it keeps
	// failing, it logs at most one line per LogEvery (1 min when zero).
	RetryFirst, RetryMax, LogEvery time.Duration

	conn conn // whether the driver is connected (State)

	ctx context.Context
	// bg counts the goroutines the driver's calls start (Connected's
	// catch-up and its retry); Run returns only once they have ended.
	bg sync.WaitGroup
}

var (
	_ surface.Sink         = (*FrontEnd)(nil)
	_ surface.Reconnected  = (*FrontEnd)(nil)
	_ surface.Disconnected = (*FrontEnd)(nil)
	_ control.ScopeOwner   = (*FrontEnd)(nil)
	_ control.Connection   = (*FrontEnd)(nil)
)

// Run connects the driver, shows the events of its scopes and sends the
// outbox until ctx ends. A driver whose Run fails, at the start or later,
// is run again after a delay (runDriver), for as long as ctx lasts.
func (f *FrontEnd) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.ctx = ctx
	f.conn.down(f.now()) // not connected until the driver says so
	f.inboundState().started()
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
	f.runDriver(ctx)
	cancel() // ctx ended: stop the rest too
	wg.Wait()
	f.bg.Wait()
	return nil
}

func (f *FrontEnd) context() context.Context {
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}

// NewThread opens a chat thread in the channel scope and returns it as the
// thread's first link. The core calls it once a Start is allowed. For a
// trigger's Start, whose key is the trigger message's id, the thread is
// opened from the trigger message (threadOf); for one another client
// makes, in the channel.
func (f *FrontEnd) NewThread(ctx context.Context, scope, key, title string) (control.LinkRef, error) {
	var n int
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_triggers WHERE client = ? AND channel = ? AND message = ?`,
		f.Name, scope, key).Scan(&n)
	if err != nil {
		return control.LinkRef{}, err
	}
	thread := ""
	if n > 0 {
		thread, err = f.threadOf(ctx, surface.MessageRef{Conv: surface.Conversation{Channel: scope}, ID: key}, title)
	} else {
		var conv surface.Conversation
		conv, err = f.Driver.OpenThread(ctx, surface.Conversation{Channel: scope}, title)
		thread = conv.Thread
	}
	if err != nil {
		return control.LinkRef{}, err
	}
	return control.LinkRef{Client: f.Name, Channel: scope, Thread: thread}, nil
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
		if done, err := f.textCommandOnce(f.context(), in); done || err != nil {
			return err
		}
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
		if done, err := f.textCommandOnce(ctx, in); done || err != nil {
			return err
		}
		text = in.Text
	}
	o, ok := parseTrigger(f.Trigger, text)
	if !ok {
		return nil
	}
	o = f.newWords(o)
	if arg, ok := attachArg(o); ok && !f.Driver.Limits().Commands {
		// On a surface without commands only; a refusal shows nothing. A
		// bot account's post is no one's request.
		if in.Author.Kind == surface.Bot {
			return nil
		}
		return f.attach(ctx, in.Ref.Conv, in.Author, arg, in.Ref.ID, nil)
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
	ref := in.Ref
	_, err = f.start(ctx, request{author: in.Author, conv: in.Ref.Conv, key: in.Ref.ID, reply: &ref}, f.newWords(o), n)
	return err
}

// request is what asked for a Start: a trigger message, or a /new command.
type request struct {
	author surface.Author
	conv   surface.Conversation // where it was asked; its channel is the scope
	key    string               // the Start's key, under which chat_triggers keeps it
	reply  *surface.MessageRef  // what an answer replies to
}

// refuseEffort answers a trigger whose effort the core does not know. It
// is not about an event: the row is keyed by event 0 and the trigger's
// number n, so a second delivery writes it once.
func (f *FrontEnd) refuseEffort(ctx context.Context, r request, o order, n int64) error {
	f.Log.Warn("trigger ignored", "reason", "unknown effort "+strconv.Quote(o.Effort), "scope", r.conv.Channel,
		"author", r.author.ID, "message", r.key)
	err := f.Outbox.Write(ctx, Row{Event: 0, Part: int(n), Conv: r.conv, Message: surface.Message{
		Text:    "Nothing was started: effort " + strconv.Quote(o.Effort) + " is not one of " + strings.Join(Efforts, ", ") + ".",
		ReplyTo: r.reply}})
	if err != nil {
		f.Log.Error("trigger: writing the refusal", "message", r.key, "err", err)
	}
	return err
}

// refuseStopped answers a trigger whose Start a plugin stopped, naming the
// plugin and why; the core logged it. Nothing was started, so the reply
// is to the trigger, keyed by the trigger's number as an effort refusal
// is.
func (f *FrontEnd) refuseStopped(ctx context.Context, r request, n int64, err error) error {
	err = f.Outbox.Write(ctx, Row{Event: 0, Part: int(n), Conv: r.conv, Message: surface.Message{
		Text: "Nothing was started: " + StoppedLine(err), ReplyTo: r.reply}})
	if err != nil {
		f.Log.Error("trigger: writing the refusal", "message", r.key, "err", err)
	}
	return err
}

// StoppedLine says which plugin stopped an input and why, from the error
// of a Start or Send that a plugin stopped.
func StoppedLine(err error) string {
	var s *control.Stopped
	if !errors.As(err, &s) {
		return "a plugin stopped it."
	}
	if s.Failed {
		return fmt.Sprintf("the plugin %s failed (%s), and its policy blocks the input.", s.Plugin, s.Reason)
	}
	return fmt.Sprintf("the plugin %s stopped it: %s", s.Plugin, s.Reason)
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

// start starts the thread r asks for, and returns it; 0 when nothing was
// started.
func (f *FrontEnd) start(ctx context.Context, r request, o order, n int64) (control.ThreadID, error) {
	channel := r.conv.Channel
	api := f.API(control.Caller{From: f.from(r.author), Scopes: []string{channel},
		May: control.OpRead | control.OpStart})
	// The trigger message gets the turn's lamp; a command has none, and
	// its prompt stands for it.
	s := sender{lamp: "message", msg: surface.MessageRef{Conv: surface.Conversation{Channel: channel}, ID: r.key}, ping: pingOf(r.author)}
	if strings.HasPrefix(r.key, commandKey) {
		s = sender{lamp: "prompt", ping: s.ping}
	}
	if err := f.rememberSender(ctx, r.key, s); err != nil {
		return 0, err
	}
	// No thread is named: once the core allows the Start, it asks this
	// front end for one (NewThread), which opens it from the trigger.
	id, startErr := api.Start(ctx, control.StartRequest{Key: r.key, Scope: channel, Repo: o.Repo,
		Agent: o.Agent, Model: o.Model, Effort: o.Effort, Input: control.Input{Text: o.Prompt}})
	switch {
	case errors.Is(startErr, control.ErrForbidden), errors.Is(startErr, control.ErrUnknownAgent):
		return 0, nil // the core logged it; nothing is shown to the author
	case errors.Is(startErr, control.ErrUnknownEffort):
		return 0, f.refuseEffort(ctx, r, o, n)
	case errors.Is(startErr, control.ErrStopped):
		return 0, f.refuseStopped(ctx, r, n, startErr)
	}
	if id == 0 {
		f.Log.Error("trigger: start failed", "scope", channel, "message", r.key, "err", startErr)
		return 0, startErr
	}
	// The thread's rows are keyed by its first event, and what they show
	// is read from the thread's events, so a trigger delivered again
	// writes the same rows, which the outbox keeps once.
	out, err := startOutcome(ctx, api, id)
	if err != nil {
		f.Log.Error("trigger: reading the new thread", "thread", id, "err", err)
		return 0, err
	}
	conv, ok, err := f.convOf(ctx, id)
	if err == nil && !ok {
		err = fmt.Errorf("chat: thread %d has no link here", id)
	}
	if err != nil {
		f.Log.Error("trigger: reading the new thread", "thread", id, "err", err)
		return 0, err
	}
	rows := []Row{{Event: out.first, Part: 0, Conv: conv, Message: surface.Message{Text: o.Prompt, Quiet: true}}}
	switch {
	case out.failed != nil:
		// The event reader writes the same row when it reads the event.
		rows = append(rows, out.failed.row(conv, out.first))
	case startErr != nil:
		// Not recorded as the session's end: no event shows it.
		rows = append(rows, Row{Event: out.first, Part: 1, Conv: conv,
			Message: surface.Message{Text: couldNotStart + startErr.Error()}})
	}
	if err := f.Outbox.Write(ctx, rows...); err != nil {
		f.Log.Error("trigger: writing the prompt", "thread", id, "err", err)
		return 0, err
	}
	return id, nil
}

// threadOf returns the chat thread of the trigger message ref: the one
// opened for it before, or one opened now from the message and stored
// with it, so that the trigger delivered again opens no second thread. A
// /new command is no message: its thread is opened in the channel alone.
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
	from := ref.ID
	if strings.HasPrefix(from, commandKey) {
		from = ""
	}
	conv, err := f.Driver.OpenThread(ctx, surface.Conversation{Channel: ref.Conv.Channel, Thread: from}, strings.TrimSpace(title))
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

// startResult is what startOutcome reads of a thread's stored events.
type startResult struct {
	first   uint64       // the position of its first event
	session int64        // its first session
	failed  *startFailed // its first session's start failure, if it failed before it started
}

// startOutcome reads a new thread's stored events: the position of its
// first, and how its first session failed before it started, if it did.
func startOutcome(ctx context.Context, api control.API, id control.ThreadID) (startResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var out startResult
	evs, err := api.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		return out, err
	}
	defer stopEvents(cancel, evs)
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		if out.first == 0 {
			out.first = e.Pos
			out.session = sessionOf(e)
		}
		if out.failed == nil {
			if sf, ok := startFailure(e); ok && sf.session == out.session {
				out.failed = &sf
			}
		}
	}
	if out.first == 0 {
		return out, fmt.Errorf("chat: thread %d: %w", id, errNoEvent)
	}
	return out, nil
}

// sessionOf is the session an event names, 0 for none.
func sessionOf(e control.Event) int64 {
	var d struct{ Session int64 }
	json.Unmarshal(e.Data, &d)
	return d.Session
}

// couldNotStart begins the message of a thread whose session could not
// start.
const couldNotStart = "This thread could not start: "

// startFailed is a session's change from creating to failed: it ended
// before it started.
type startFailed struct {
	pos     uint64 // the state event's position
	session int64
	reason  string // as recorded, without the "start: " the core puts before its own
	detail  string // the event's detail, for a person
}

// startFailure reads e as a session's change from creating to failed.
func startFailure(e control.Event) (startFailed, bool) {
	if e.Kind != "state" {
		return startFailed{}, false
	}
	var d struct {
		Session                  int64
		From, To, Reason, Detail string
	}
	if json.Unmarshal(e.Data, &d) != nil || d.From != string(control.Creating) || d.To != string(control.Failed) {
		return startFailed{}, false
	}
	return startFailed{pos: e.Pos, session: d.Session, reason: strings.TrimPrefix(d.Reason, "start: "), detail: d.Detail}, true
}

// row is the one message the failure gets, keyed by its event, so that the
// trigger's start and the event reader write it once between them, and
// again after a restart write nothing. The failure of the thread's first
// session replies to the thread's prompt, at first, and so is posted after
// it.
func (sf startFailed) row(conv surface.Conversation, first uint64) Row {
	r := Row{Event: sf.pos, Part: 0, Conv: conv, Message: surface.Message{Text: sf.text()}}
	if first != 0 {
		r.ReplyToRow = &RowKey{Event: first, Part: 0}
	}
	return r
}

// text says why the session could not start: its reason, and the event's
// detail when that is text for a person, after the reason on its line, or
// below it when it has several lines. A detail with control characters
// other than line breaks is terminal output, such as the screen of a start
// that timed out: the dashboard shows it, not the chat.
func (sf startFailed) text() string {
	t := couldNotStart + sf.reason
	d := ReadableDetail(sf.detail)
	switch {
	case strings.Contains(d, "\n"):
		t += ".\n" + d
	case d != "":
		t += ": " + d
	}
	return t
}

// maxDetail bounds the detail a failure message carries.
const maxDetail = 2000

// ReadableDetail returns a state event's detail when it is text for a
// person: lines with no control character, such as an escape code or a
// carriage return; "" otherwise.
func ReadableDetail(detail string) string {
	d := strings.TrimSpace(detail)
	if d == "" || len(d) > maxDetail {
		return ""
	}
	for _, r := range d {
		if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) {
			return ""
		}
	}
	return d
}

// followUp sends a message in a thread to the thread's session. A thread
// hataori does not know gets nothing.
func (f *FrontEnd) followUp(in surface.Inbound) error {
	if in.Author.Kind == surface.Bot {
		return nil // the front end's own posts come back this way
	}
	ctx := f.context()
	conv := in.Ref.Conv
	scopes := f.scopesIn(conv.Channel)
	if len(scopes) == 0 {
		return nil
	}
	api := f.API(control.Caller{From: f.from(in.Author), Scopes: scopes, May: control.OpRead | control.OpSend})
	t, ok, err := f.threadIn(ctx, api, conv)
	if err != nil {
		return err // not known yet whether it is one of ours: try again
	}
	if !ok {
		return nil // not a hataori thread
	}
	id := t.ID
	if t.State == control.Failed {
		// Its session cannot take input; the message is not for later.
		f.Log.Warn("follow-up ignored", "reason", "the thread is "+string(t.State), "scope", conv.Channel,
			"thread", conv.Thread, "message", in.Ref.ID)
		return nil
	}
	// The key makes a second delivery one input; a running turn gets it
	// delivered, an open question gets it as the answer. Whether the author
	// may give input at all, the core decides too, and it refuses input to a
	// closed thread, which a message does not wake.
	if err := f.rememberSender(ctx, in.Ref.ID, sender{lamp: "message", msg: in.Ref, ping: pingOf(in.Author)}); err != nil {
		return err
	}
	err = api.Send(ctx, id, in.Ref.ID, control.Input{Text: in.Text,
		Link: &control.LinkRef{Client: f.Name, Channel: conv.Channel, Thread: conv.Thread}}, control.Deliver)
	if errors.Is(err, control.ErrForbidden) {
		return nil // the core logged it
	}
	if errors.Is(err, control.ErrClosed) {
		f.Log.Warn("follow-up ignored", "reason", "the thread is closed", "scope", conv.Channel,
			"thread", conv.Thread, "message", in.Ref.ID)
		return f.closedNotice(ctx, in)
	}
	if errors.Is(err, control.ErrStopped) {
		return f.refusedNotice(ctx, in, "Not sent to the agent: "+StoppedLine(err)) // the core logged it
	}
	if err != nil {
		f.Log.Error("follow-up: send failed", "scope", conv.Channel, "thread", conv.Thread, "message", in.Ref.ID, "err", err)
		return err
	}
	return nil
}

// ClosedLine is what a message in a closed thread gets.
const ClosedLine = "This thread is closed: nothing was sent to the agent."

// closedNotice answers a message in a closed thread with ClosedLine. The
// row is keyed by the message's place in chat_notices, so the message
// delivered again writes it once. Its event is 0 and its part negative,
// which no other row uses.
func (f *FrontEnd) closedNotice(ctx context.Context, in surface.Inbound) error {
	return f.refusedNotice(ctx, in, ClosedLine)
}

// refusedNotice answers a message that reached no agent with text, once: the row
// is keyed by the message's place in chat_notices, as closedNotice's.
func (f *FrontEnd) refusedNotice(ctx context.Context, in surface.Inbound, text string) error {
	err := f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_notices (client, channel, message, received_at)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, f.Name, in.Ref.Conv.Channel, in.Ref.ID, time.Now().UnixMilli()); err != nil {
			return err
		}
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT rowid FROM chat_notices WHERE client = ? AND channel = ? AND message = ?`,
			f.Name, in.Ref.Conv.Channel, in.Ref.ID).Scan(&n); err != nil {
			return err
		}
		ref := in.Ref
		return f.Outbox.WriteTx(ctx, tx, Row{Event: 0, Part: -int(n), Conv: in.Ref.Conv,
			Message: surface.Message{Text: text, ReplyTo: &ref}})
	})
	if err != nil {
		f.Log.Error("follow-up: writing a notice", "scope", in.Ref.Conv.Channel, "thread", in.Ref.Conv.Thread,
			"message", in.Ref.ID, "err", err)
		return err
	}
	f.Outbox.Wake()
	return nil
}
