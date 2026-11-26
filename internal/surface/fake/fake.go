// Package fake is an in-memory chat service for tests. It records what is
// posted to it and delivers what a test injects. It imitates no real
// service.
package fake

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/surface"
)

// Op is one kind of driver call.
type Op int

// The kinds of call. A test makes the next calls of a kind fail with Fail.
const (
	OpPost Op = iota
	OpEdit
	OpDelete
	OpSetStatus
	OpSetThread
	OpOpenThread
	OpOpenDirect
	OpSince
	OpRun      // Run fails before it connects
	OpIdentity // CheckIdentity
)

func (o Op) String() string {
	switch o {
	case OpPost:
		return "post"
	case OpEdit:
		return "edit"
	case OpDelete:
		return "delete"
	case OpSetStatus:
		return "set-status"
	case OpSetThread:
		return "set-thread"
	case OpOpenThread:
		return "open-thread"
	case OpOpenDirect:
		return "open-direct"
	case OpSince:
		return "since"
	case OpRun:
		return "run"
	case OpIdentity:
		return "identity"
	}
	return "op(" + strconv.Itoa(int(o)) + ")"
}

// Call is one recorded call that changed something on the surface.
type Call struct {
	Op      Op
	Ref     MessageRef           // the message posted, edited, deleted or given a status
	Key     string               // Post's key
	Message surface.Message      // Post's and Edit's message
	Status  surface.Status       // SetStatus's status
	Thread  surface.ThreadMeta   // SetThread's meta
	Opened  surface.Conversation // OpenThread's new thread
	Title   string               // OpenThread's title
	From    string               // OpenThread's message the thread is opened from, if any
}

// MessageRef is surface.MessageRef, repeated here for short test code.
type MessageRef = surface.MessageRef

// ErrNoMessage is returned for a reference the driver never handed out, or
// one that was deleted.
var ErrNoMessage = errors.New("fake surface: no such message")

// ErrUndeliverable is returned for a direct message to a person who does
// not take them (see Undeliverable).
var ErrUndeliverable = errors.New("fake surface: the person does not take direct messages")

// ErrNoChoice is returned by Press for a message without that choice.
var ErrNoChoice = errors.New("fake surface: the message has no such choice")

// ErrInjected is the default error of a call a test made fail.
var ErrInjected = errors.New("fake surface: injected failure")

type failure struct {
	n          int
	err        error
	afterwards bool // the call takes effect, then returns err
}

// Driver is an in-memory surface.Driver. The zero value is not usable; call New.
type Driver struct {
	// deliver orders Inject's calls of the sink as their messages were
	// received. It is taken before mu, and the sink must not call Inject.
	deliver   sync.Mutex
	mu        sync.Mutex
	sink      surface.Sink
	connected bool
	runs      int
	drop      chan error // ends the running Run (Drop)
	commands  []surface.CommandSpec
	next      int
	limits    surface.Limits
	calls     map[surface.Conversation][]Call
	inbound   map[surface.Conversation][]surface.Inbound
	live      map[MessageRef]bool
	received  map[MessageRef]bool            // injected messages, which take a status as a posted one does
	shown     map[MessageRef]surface.Message // what each live message shows now
	blocked   map[string]bool                // people whose direct messages fail
	fail      map[Op]*failure
	direct    map[string]surface.Conversation
	// fromMessage is the thread opened from each message.
	fromMessage map[MessageRef]surface.Conversation
	names       map[surface.Conversation]string // what Name answers
	private     map[surface.Conversation]bool
	archived    map[surface.Conversation]bool // threads archived by SetThread
}

var _ surface.Driver = (*Driver)(nil)

// DefaultLimits is what a new Driver reports until SetLimits.
var DefaultLimits = surface.Limits{
	MaxText:    2000,
	MaxChoices: 5,
	MaxFiles:   10,
	// MaxFileBytes: 8 MiB.
	MaxFileBytes: 8 << 20,
	Edit:         true,
	Delete:       true,
	SetStatus:    true,
	SetThread:    true,
}

// New returns an empty, disconnected driver.
func New() *Driver {
	return &Driver{
		limits:   DefaultLimits,
		calls:    map[surface.Conversation][]Call{},
		inbound:  map[surface.Conversation][]surface.Inbound{},
		live:     map[MessageRef]bool{},
		received: map[MessageRef]bool{},
		shown:    map[MessageRef]surface.Message{},
		blocked:  map[string]bool{},
		fail:     map[Op]*failure{},
		direct:   map[string]surface.Conversation{},

		fromMessage: map[MessageRef]surface.Conversation{},
		names:       map[surface.Conversation]string{},
		archived:    map[surface.Conversation]bool{},
	}
}

var _ surface.Namer = (*Driver)(nil)

var _ surface.Linker = (*Driver)(nil)

// Link returns a text that names conv.
func (d *Driver) Link(conv surface.Conversation) string {
	return "[link to " + conv.Channel + "/" + conv.Thread + "]"
}

var _ surface.Verbatimer = (*Driver)(nil)

// Verbatim returns s marked as verbatim.
func (d *Driver) Verbatim(s string) string {
	return "[verbatim]\n" + s + "\n[end of verbatim]"
}

// Self is the id of the driver's own account.
const Self = "hataori"

var _ surface.Mentioner = (*Driver)(nil)

// MentionsOthers reads "<@id>" in text as a mention of the account id, as
// a service with markup does.
func (d *Driver) MentionsOthers(text string) bool {
	others := false
	for _, part := range strings.Split(text, "<@")[1:] {
		id, _, ok := strings.Cut(part, ">")
		if !ok {
			continue
		}
		if id == Self {
			return false
		}
		others = true
	}
	return others
}

// SetName makes Name answer name for conv, as a service that shows people
// that name; "" for none.
func (d *Driver) SetName(conv surface.Conversation, name string) {
	d.mu.Lock()
	d.names[conv] = name
	d.mu.Unlock()
}

// Rename gives conv the name name, as a person renaming it on the service
// would: Name answers it from now on, and a sink that wants names
// (surface.Renamer) is told.
func (d *Driver) Rename(conv surface.Conversation, name string) {
	d.deliver.Lock()
	defer d.deliver.Unlock()
	d.mu.Lock()
	d.names[conv] = name
	sink := d.sink
	d.mu.Unlock()
	if r, ok := sink.(surface.Renamer); ok {
		r.Renamed(conv, name)
	}
}

// Name answers what SetName set for conv, "" until then.
func (d *Driver) Name(_ context.Context, conv surface.Conversation) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.names[conv], nil
}

// LayoutDriver is a Driver that can also tell how the service lays out its
// conversations (surface.Layouter), as a service with a channel list does.
// A Driver alone cannot, as a service without one.
type LayoutDriver struct {
	*Driver
	layout surface.Layout // guarded by Driver.mu
}

var _ surface.Layouter = (*LayoutDriver)(nil)

// WithLayout returns d with a layout, empty until SetLayout.
func (d *Driver) WithLayout() *LayoutDriver { return &LayoutDriver{Driver: d} }

// SetLayout makes Layout answer l, as the service would after a person
// changed its channels or threads, and tells a sink that wants to know
// (surface.LayoutWatcher).
func (d *LayoutDriver) SetLayout(l surface.Layout) {
	d.deliver.Lock()
	defer d.deliver.Unlock()
	d.mu.Lock()
	d.layout = l
	sink := d.sink
	d.mu.Unlock()
	if w, ok := sink.(surface.LayoutWatcher); ok {
		w.LayoutChanged()
	}
}

// Layout answers what SetLayout set.
func (d *LayoutDriver) Layout(context.Context) (surface.Layout, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.layout, nil
}

func (d *Driver) id(prefix string) string {
	d.next++
	return prefix + strconv.Itoa(d.next)
}

// CheckIdentity returns the planned failure of OpIdentity (Fail), or nil.
func (d *Driver) CheckIdentity(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	before, after := d.failing(OpIdentity)
	return errors.Join(before, after)
}

// Run connects: injected messages go to sink until ctx ends or Disconnect.
// It returns the planned failure of OpRun (Fail) at once, without
// connecting, and returns Drop's error when Drop is called.
func (d *Driver) Run(ctx context.Context, sink surface.Sink) error {
	d.mu.Lock()
	d.runs++
	if before, after := d.failing(OpRun); before != nil || after != nil {
		d.mu.Unlock()
		return errors.Join(before, after)
	}
	d.sink = sink
	d.connected = true
	drop := make(chan error, 1)
	d.drop = drop
	if c, ok := sink.(surface.Commander); ok {
		d.commands = c.Commands()
	}
	d.mu.Unlock()
	connected(sink)
	var err error
	select {
	case <-ctx.Done():
	case err = <-drop:
	}
	d.mu.Lock()
	d.sink = nil
	d.connected = false
	d.drop = nil
	d.mu.Unlock()
	return err
}

// Runs counts the calls of Run, also those that failed.
func (d *Driver) Runs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs
}

// Connected reports whether Run has connected the driver to a sink.
func (d *Driver) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// Disconnect simulates an outage that the driver itself ends: injected
// messages are kept for Since but not delivered, until Reconnect. A sink
// that wants to know is told (surface.Disconnected).
func (d *Driver) Disconnect() {
	d.mu.Lock()
	d.connected = false
	sink := d.sink
	d.mu.Unlock()
	if s, ok := sink.(surface.Disconnected); ok {
		s.Disconnected()
	}
}

// Drop ends a running Run with err (ErrInjected when nil), as a connection
// that drops and that the driver cannot make again. The next Run connects
// again.
func (d *Driver) Drop(err error) {
	if err == nil {
		err = ErrInjected
	}
	d.mu.Lock()
	ch := d.drop
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- err:
		default:
		}
	}
}

// Reconnect ends an outage started by Disconnect, if Run is still running.
// Messages injected during the outage are not delivered; Since returns them.
func (d *Driver) Reconnect() {
	d.mu.Lock()
	d.connected = d.sink != nil
	sink := d.sink
	d.mu.Unlock()
	if sink != nil {
		connected(sink)
	}
}

// connected tells a sink that wants to know that the driver connected.
func connected(sink surface.Sink) {
	if r, ok := sink.(surface.Reconnected); ok {
		r.Connected()
	}
}

// Person, Webhook and Bot make an author of each kind.
func Person(id string) surface.Author { return surface.Author{Kind: surface.Person, ID: id, Name: id} }
func Webhook(id string) surface.Author {
	return surface.Author{Kind: surface.Webhook, ID: id, Name: id}
}
func Bot(id string) surface.Author { return surface.Author{Kind: surface.Bot, ID: id, Name: id} }

// Inject receives a message in conv (a channel when conv.Thread is empty,
// else a thread) from author. While connected it is delivered to the sink
// before Inject returns; either way Since finds it. It returns the message.
// A message in an archived thread un-archives it first, as a person
// writing in it does on a service that archives threads.
func (d *Driver) Inject(conv surface.Conversation, author surface.Author, text string) surface.Inbound {
	return d.InjectWith(conv, author, text, nil)
}

// InjectWith is Inject of a message that set changes first, such as one
// that is a call: the driver itself tells of no message that it is one.
func (d *Driver) InjectWith(conv surface.Conversation, author surface.Author, text string, set func(*surface.Inbound)) surface.Inbound {
	d.deliver.Lock()
	defer d.deliver.Unlock()
	d.mu.Lock()
	delete(d.archived, conv)
	id := d.id("in")
	in := surface.Inbound{
		Ref:    MessageRef{Conv: conv, ID: id},
		Author: author,
		Text:   text,
		Time:   time.Now(),
		Cursor: id,
	}
	for who, c := range d.direct {
		if c == conv && who == author.ID {
			in.Direct = true
		}
	}
	if set != nil {
		set(&in)
	}
	d.inbound[conv] = append(d.inbound[conv], in)
	d.received[in.Ref] = true
	var sink surface.Sink
	if d.connected {
		sink = d.sink
	}
	d.mu.Unlock()
	if sink != nil {
		sink.Message(in)
	}
	return in
}

// InjectCommand delivers the use of a command in conv, when connected,
// with a Reply in conv as a service that waits for an answer gives one.
// It returns the command.
func (d *Driver) InjectCommand(conv surface.Conversation, author surface.Author, name string, args ...surface.Arg) surface.Command {
	d.deliver.Lock()
	defer d.deliver.Unlock()
	d.mu.Lock()
	id := d.id("cmd")
	c := surface.Command{Conv: conv, Author: author, ID: id, Name: name, Args: args,
		Reply: &surface.MessageRef{Conv: conv, ID: id}}
	var sink surface.Sink
	if d.connected {
		sink = d.sink
	}
	d.mu.Unlock()
	if sink != nil {
		sink.Command(c)
	}
	return c
}

// Commands returns the commands the sink gave when Run connected it.
func (d *Driver) Commands() []surface.CommandSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.commands
}

// Since returns the messages received in conv after the one whose cursor is
// given; all of them for an empty cursor.
func (d *Driver) Since(_ context.Context, conv surface.Conversation, cursor string) ([]surface.Inbound, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if before, after := d.failing(OpSince); before != nil || after != nil {
		return nil, errors.Join(before, after)
	}
	all := d.inbound[conv]
	start := 0
	if cursor != "" {
		start = -1
		for i, in := range all {
			if in.Cursor == cursor {
				start = i + 1
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("fake surface: unknown cursor %q", cursor)
		}
	}
	return append([]surface.Inbound(nil), all[start:]...), nil
}

// Fail makes the next n calls of op fail with err (ErrInjected when nil).
func (d *Driver) Fail(op Op, n int, err error) {
	d.setFailure(op, n, err, false)
}

// FailAfter makes the next n calls of op take effect on the surface and
// then return err (ErrInjected when nil), as when the service accepted a
// post but the answer was lost.
func (d *Driver) FailAfter(op Op, n int, err error) {
	d.setFailure(op, n, err, true)
}

func (d *Driver) setFailure(op Op, n int, err error, afterwards bool) {
	if err == nil {
		err = ErrInjected
	}
	d.mu.Lock()
	d.fail[op] = &failure{n: n, err: err, afterwards: afterwards}
	d.mu.Unlock()
}

// failing consumes one planned failure of op. It returns the error to give
// before the call takes effect, or the one to give after it.
func (d *Driver) failing(op Op) (before, after error) {
	f := d.fail[op]
	if f == nil || f.n <= 0 {
		return nil, nil
	}
	f.n--
	if f.afterwards {
		return nil, f.err
	}
	return f.err, nil
}

// SetLimits sets what Limits reports.
func (d *Driver) SetLimits(l surface.Limits) {
	d.mu.Lock()
	d.limits = l
	d.mu.Unlock()
}

// Limits reports the limits set by the test, DefaultLimits otherwise.
func (d *Driver) Limits() surface.Limits {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.limits
}

// Calls returns the recorded calls in conv, in order.
func (d *Driver) Calls(conv surface.Conversation) []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Call, len(d.calls[conv]))
	for i, c := range d.calls[conv] {
		c.Message = clone(c.Message)
		out[i] = c
	}
	return out
}

func (d *Driver) record(conv surface.Conversation, c Call) {
	c.Message = clone(c.Message)
	d.calls[conv] = append(d.calls[conv], c)
}

// clone copies m so that a caller changing its slices later does not change
// what was recorded, and a test changing a recorded call changes nothing.
func clone(m surface.Message) surface.Message {
	m.Files = append([]surface.File(nil), m.Files...)
	m.Choices = append([]surface.Choice(nil), m.Choices...)
	m.Notify = append([]string(nil), m.Notify...)
	if m.ReplyTo != nil {
		r := *m.ReplyTo
		m.ReplyTo = &r
	}
	return m
}

// OpenThread opens a new thread in parent's channel, from the message
// parent.Thread names when it is set. The call is recorded in the channel,
// with the message in From. A message that has a thread already gets that
// thread back, and nothing is recorded.
func (d *Driver) OpenThread(_ context.Context, parent surface.Conversation, title string) (surface.Conversation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	before, after := d.failing(OpOpenThread)
	if before != nil {
		return surface.Conversation{}, before
	}
	from := MessageRef{Conv: surface.Conversation{Channel: parent.Channel}, ID: parent.Thread}
	if conv, ok := d.fromMessage[from]; ok && parent.Thread != "" {
		if after != nil {
			return surface.Conversation{}, after
		}
		return conv, nil
	}
	conv := surface.Conversation{Channel: parent.Channel, Thread: d.id("thread")}
	if parent.Thread != "" {
		d.fromMessage[from] = conv
	}
	d.record(surface.Conversation{Channel: parent.Channel}, Call{Op: OpOpenThread, Opened: conv, Title: title, From: parent.Thread})
	if after != nil {
		return surface.Conversation{}, after
	}
	return conv, nil
}

// OpenDirect returns the private conversation with who, the same one each time.
func (d *Driver) OpenDirect(_ context.Context, who string) (surface.Conversation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	before, after := d.failing(OpOpenDirect)
	if before != nil {
		return surface.Conversation{}, before
	}
	conv := d.directConv(who)
	if after != nil {
		return surface.Conversation{}, after
	}
	return conv, nil
}

// Post records m in conv and returns its reference.
func (d *Driver) Post(_ context.Context, conv surface.Conversation, key string, m surface.Message) (MessageRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	before, after := d.failing(OpPost)
	if before != nil {
		return MessageRef{}, before
	}
	for who, c := range d.direct {
		if c == conv && d.blocked[who] {
			return MessageRef{}, fmt.Errorf("%w: %s", ErrUndeliverable, who)
		}
	}
	ref := MessageRef{Conv: conv, ID: d.id("m")}
	d.live[ref] = true
	d.shown[ref] = clone(m)
	d.record(conv, Call{Op: OpPost, Ref: ref, Key: key, Message: m})
	if after != nil {
		return MessageRef{}, after
	}
	return ref, nil
}

// Edit records the new content of ref.
func (d *Driver) Edit(_ context.Context, ref MessageRef, m surface.Message) error {
	return d.change(OpEdit, ref, Call{Op: OpEdit, Ref: ref, Message: m})
}

// Delete records the deletion of ref.
func (d *Driver) Delete(_ context.Context, ref MessageRef) error {
	return d.change(OpDelete, ref, Call{Op: OpDelete, Ref: ref})
}

// SetStatus records a status on ref, a message the driver posted or one
// it received: a service shows a lamp on anyone's message.
func (d *Driver) SetStatus(_ context.Context, ref MessageRef, s surface.Status) error {
	return d.change(OpSetStatus, ref, Call{Op: OpSetStatus, Ref: ref, Status: s})
}

func (d *Driver) change(op Op, ref MessageRef, c Call) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	before, after := d.failing(op)
	if before != nil {
		return before
	}
	if !d.live[ref] && !(op == OpSetStatus && d.received[ref]) {
		return ErrNoMessage
	}
	switch op {
	case OpDelete:
		delete(d.live, ref)
		delete(d.shown, ref)
	case OpEdit:
		d.shown[ref] = clone(c.Message)
	}
	d.record(ref.Conv, c)
	return after
}

// SetThread records a thread's title and archived flag, and sets the
// flag Archived reports. A post into an archived thread leaves it archived,
// so that a test sees whether the code under test un-archived it.
func (d *Driver) SetThread(_ context.Context, conv surface.Conversation, t surface.ThreadMeta) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	before, after := d.failing(OpSetThread)
	if before != nil {
		return before
	}
	if t.Archived {
		d.archived[conv] = true
	} else {
		delete(d.archived, conv)
	}
	d.record(conv, Call{Op: OpSetThread, Thread: t})
	return after
}

// Archived reports whether the thread conv is archived: SetThread archived
// it, and no message was injected into it since.
func (d *Driver) Archived(conv surface.Conversation) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.archived[conv]
}

// InjectDirect receives a direct message from the person who, in the
// private conversation with them, delivered as direct.
// It is not a call of the code under test, so failures planned for
// OpenDirect do not touch it.
func (d *Driver) InjectDirect(who, text string) surface.Inbound {
	d.mu.Lock()
	conv := d.directConv(who)
	d.mu.Unlock()
	return d.Inject(conv, Person(who), text)
}

// directConv returns the private conversation with who; d.mu is held.
func (d *Driver) directConv(who string) surface.Conversation {
	conv, ok := d.direct[who]
	if !ok {
		conv = surface.Conversation{Channel: d.id("direct")}
		d.direct[who] = conv
	}
	return conv
}

// Press uses the choice with token on the message ref as the person who:
// the press is delivered to the sink as a Selection. The message must be
// one the driver posted and still shows that choice.
func (d *Driver) Press(ref MessageRef, who, token string) error {
	return d.PressMany(ref, who, token)
}

// PressMany chooses several options of a menu on the message ref as the
// person who, as Press does one: the Selection's Token is the first and
// Tokens are all of them.
func (d *Driver) PressMany(ref MessageRef, who string, tokens ...string) error {
	d.deliver.Lock()
	defer d.deliver.Unlock()
	d.mu.Lock()
	m, ok := d.shown[ref]
	sink := d.sink
	if !d.connected {
		sink = nil
	}
	d.mu.Unlock()
	if !ok {
		return ErrNoMessage
	}
	if len(tokens) == 0 || (len(tokens) > 1 && !m.Multi) {
		return ErrNoChoice
	}
	for _, token := range tokens {
		found := false
		for _, c := range m.Choices {
			found = found || c.Token == token
		}
		if !found {
			return ErrNoChoice
		}
	}
	if sink != nil {
		sink.Choice(surface.Selection{Ref: ref, Author: Person(who), Token: tokens[0], Tokens: tokens})
	}
	return nil
}

// Undeliverable makes every post into the direct conversation with who
// fail with ErrUndeliverable, as when a person does not take direct
// messages; Deliverable undoes it.
func (d *Driver) Undeliverable(who string) { d.setBlocked(who, true) }

// Deliverable undoes Undeliverable.
func (d *Driver) Deliverable(who string) { d.setBlocked(who, false) }

func (d *Driver) setBlocked(who string, b bool) {
	d.mu.Lock()
	d.blocked[who] = b
	d.mu.Unlock()
}

// SetPrivate controls the privacy of a fake thread.
func (d *Driver) SetPrivate(conv surface.Conversation, private bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.private == nil {
		d.private = map[surface.Conversation]bool{}
	}
	d.private[conv] = private
}

func (d *Driver) PrivateThread(_ context.Context, conv surface.Conversation) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.private[conv], nil
}
