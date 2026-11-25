// Package surface declares a chat service, as seen by the chat front end.
//
// A Driver knows one chat service and nothing about agents or threads of
// work. Drivers register a Factory under a name; the name is used only as a
// key and as a path segment.
package surface

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/yousan/hataori/internal/registry"
)

// ErrGone is what a driver returns, wrapped, when a conversation no longer
// exists on the service, as when it was deleted. Asking again will not
// bring it back.
var ErrGone = errors.New("the conversation is gone")

// ErrRefused is what a driver's error wraps, or matches with errors.Is,
// when the same call can never succeed: the service, or the driver for it,
// refuses what it asks. Any other error may pass, and the call may be made
// again.
var ErrRefused = errors.New("the chat service refuses it")

// ErrOtherIdentity is what a driver's error wraps when its credential is for
// another identity on the service than the one its settings name. It does
// not pass: the driver makes no request with that credential again.
var ErrOtherIdentity = errors.New("the credential is not for the configured identity")

// Driver is one chat service. A driver whose settings name the identity its
// credential is for makes no request with it, in any call, before it has
// checked that (Identified).
type Driver interface {
	// Run connects and delivers inbound events to sink until ctx ends.
	// It reconnects by itself and calls sink in order per conversation.
	// A Run that returned before may be called again, unless its error
	// wraps ErrOtherIdentity.
	Run(ctx context.Context, sink Sink) error

	// Since returns the messages in a conversation after cursor,
	// for catching up after an outage. When the conversation no longer
	// exists on the service, the error wraps ErrGone.
	Since(ctx context.Context, conv Conversation, cursor string) ([]Inbound, error)

	// OpenThread opens a thread in parent.Channel and returns it. When
	// parent.Thread is set, it names a message in that channel, and the
	// thread is opened from that message; when that message has a thread
	// already, OpenThread returns it and opens none.
	OpenThread(ctx context.Context, parent Conversation, title string) (Conversation, error)
	// OpenDirect returns the private conversation with one person, given
	// by the person's id on the service.
	OpenDirect(ctx context.Context, who string) (Conversation, error)
	// Post posts m. key makes the post safe to repeat where the service
	// supports it. A post that fails with ErrRefused is given up; with any
	// other error it is posted again later. When a post that became several
	// messages failed after the first, the first is returned with the error.
	Post(ctx context.Context, conv Conversation, key string, m Message) (MessageRef, error)
	// Edit replaces a posted message.
	Edit(ctx context.Context, ref MessageRef, m Message) error
	// Delete deletes a posted message.
	Delete(ctx context.Context, ref MessageRef) error
	// SetStatus shows a lamp on a message.
	SetStatus(ctx context.Context, ref MessageRef, s Status) error
	// SetThread sets a thread's title and archived flag; an empty Title
	// leaves the title as it is. It is optional (Limits.SetThread).
	// Archiving never locks the thread and deletes nothing: a message
	// posted into an archived thread un-archives it, where the service
	// does that by itself.
	SetThread(ctx context.Context, conv Conversation, t ThreadMeta) error

	// Limits reports message length, choices per message, and which
	// optional calls work.
	Limits() Limits
}

// Identified is implemented by a Driver whose settings name the identity its
// credential is for. CheckIdentity asks the service which one it is for, and
// returns an error that wraps ErrOtherIdentity when it is another one. Any
// other error may pass, and it may be called again. Once it has returned
// nil, it costs the service nothing.
type Identified interface {
	CheckIdentity(ctx context.Context) error
}

// Namer is implemented by a Driver that can tell the name the service shows
// people for a conversation, such as "#general" for a channel. A
// conversation the service gives no name, such as a direct one, has "".
type Namer interface {
	Name(ctx context.Context, conv Conversation) (string, error)
}

// Linker is implemented by a Driver whose service shows a link to a
// conversation in the text of a message: Link returns the text that does,
// which a person opens conv with, or "" when conv cannot be linked to.
type Linker interface {
	Link(conv Conversation) string
}

// Verbatimer is implemented by a Driver whose service reads the text of a
// message as markup. Verbatim returns the text that shows s, which has one
// line or more, as its own characters and lines, as in a block of code:
// nothing in s is read as a link, a mention or any other formatting, and
// nothing in it ends that. It stands on lines of its own in a message's
// text. On a Driver without it, a text shows as its characters.
type Verbatimer interface {
	Verbatim(s string) string
}

// Mentioner is implemented by a Driver whose service writes a mention in
// the text of a message. MentionsOthers reports whether text, as an Inbound
// has it, mentions an account other than the driver's own, and neither its
// own nor a group that may hold it, such as a role or everyone. On a Driver
// without it, no text does.
type Mentioner interface {
	MentionsOthers(text string) bool
}

// Renamer is implemented by a Sink that wants the names of conversations
// as the service shows them: Run calls Renamed when it learns a thread's
// name, and when the name changes.
type Renamer interface {
	Renamed(conv Conversation, name string)
}

// Layouter is implemented by a Driver that can tell how the service lays
// out the conversations it can see, as a person sees them in the service's
// own client: groups of channels, the channels in order, and each channel's
// open threads. Layout answers from what the driver keeps, so calling it
// costs the service nothing. A driver that knows no layout yet, such as
// before it has connected, answers an empty one.
type Layouter interface {
	Layout(ctx context.Context) (Layout, error)
}

// LayoutWatcher is implemented by a Sink that wants to know when the layout
// changes (Layouter): Run calls LayoutChanged when it learns the layout,
// and after any change it learns of, such as a channel or a thread
// created, renamed, archived or deleted. Several changes may come as one
// call. It must not block.
type LayoutWatcher interface {
	LayoutChanged()
}

// Layout is how a service lays out its conversations (Layouter).
type Layout struct {
	Groups []LayoutGroup // in the service's order
}

// LayoutGroup is a group of channels, such as a category. The channels in
// no group are a group with no ID and no Name.
type LayoutGroup struct {
	ID       string
	Name     string
	Channels []LayoutChannel // in the service's order
}

// LayoutChannel is one channel. URL, when the service has one, opens the
// channel in the service's own client.
type LayoutChannel struct {
	ID      string
	Name    string
	URL     string
	Threads []LayoutThread // the open threads, in the service's order; none archived
}

// LayoutThread is one open thread of a channel. ID is the Thread of its
// Conversation.
type LayoutThread struct {
	ID   string
	Name string
	URL  string
}

// Sink is implemented by the chat front end, not by the core.
type Sink interface {
	// Message delivers an inbound message: text, files, author, ids.
	Message(Inbound)
	// Choice delivers the use of a button or menu.
	Choice(Selection)
	// Command delivers a slash or text command.
	Command(Command)
}

// Commander is implemented by a Sink that handles commands. Run asks it for
// the commands once, when it starts, and makes them the service's commands
// for the bot; a service where they are already these is left unchanged.
type Commander interface {
	Commands() []CommandSpec
}

// CommandSpec is a command the front end handles.
type CommandSpec struct {
	Name        string
	Description string
	Args        []ArgSpec // in the order the service shows them; required ones first
	// Private says that the answer is shown only to the person who used
	// the command.
	Private bool
}

// ArgSpec is one argument of a command. Every argument is text.
type ArgSpec struct {
	Name        string
	Description string
	Required    bool
}

// Reconnected is implemented by a Sink that wants to know when the driver
// has connected: Run calls Connected after it connects and after each
// reconnect, so that the sink can catch up on what it missed with Since.
type Reconnected interface {
	Connected()
}

// Disconnected is implemented by a Sink that wants to know when the driver
// has lost its connection and is connecting again by itself: Run calls
// Disconnected then, and Connected once it is connected again. A Run that
// returns is not connected either, and does not call it.
type Disconnected interface {
	Disconnected()
}

// Conversation is a place messages are posted in: a channel, or a thread
// inside it. Both fields are opaque ids of the service.
type Conversation struct {
	Channel string
	Thread  string // empty for the channel itself
}

// MessageRef identifies a posted message.
type MessageRef struct {
	Conv Conversation
	ID   string
}

// AuthorKind tells people from automated posters.
type AuthorKind int

// The kinds of Author.
const (
	Person AuthorKind = iota
	Bot
	Webhook
)

// Author is who wrote an inbound message. ID is opaque: a user id, or a
// webhook id for a webhook post.
type Author struct {
	Kind AuthorKind
	ID   string
	Name string
}

// Inbound is a message received from the service.
type Inbound struct {
	Ref     MessageRef
	Author  Author
	Text    string
	Files   []File
	ReplyTo *MessageRef
	Direct  bool // the conversation is a private one with the author
	Service bool // written by the service itself, such as a notice that a thread was renamed
	Time    time.Time
	Cursor  string // pass to Since to get the messages after this one
}

// Selection is a use of a choice on a posted message.
type Selection struct {
	Ref    MessageRef
	Author Author
	Token  string // the token of the Choice that was used; the first one of a menu
	// Tokens are the tokens of every Choice chosen: Token alone for a
	// button, all the options chosen in a menu.
	Tokens []string
}

// Command is a slash or text command.
type Command struct {
	Conv   Conversation
	Author Author
	ID     string // safe-to-repeat key of the command
	Name   string
	Args   []Arg // the arguments given, in the order of the command's spec
	// Reply, when set, is where the answer goes: a Post into Conv with
	// ReplyTo set to Reply answers the command in place, privately when the
	// command's spec says Private. A driver sets it when the service waits
	// for an answer to the command.
	Reply *MessageRef
}

// Arg is one argument given with a command.
type Arg struct {
	Name  string
	Value string
}

// Message says what to show, not how.
type Message struct {
	Text    string
	Files   []File
	Choices []Choice    // rendered as buttons or a menu; each carries an opaque token
	Multi   bool        // several Choices may be chosen at once; needs Limits.MultiSelect
	ReplyTo *MessageRef // quote-reply
	Notify  []string    // ids of people to ping
	Quiet   bool        // no notification
}

// File is a file in a message, stored on local disk.
type File struct {
	Name string // the name shown in chat
	Path string // where the content is
}

// Choice is one option of a message.
type Choice struct {
	Label string
	Token string
}

// Status is a lamp shown on a message.
type Status int

// The values of Status.
const (
	NoStatus Status = iota
	Working
	Waiting
	Done
	Failed
)

// ThreadMeta is what SetThread changes.
type ThreadMeta struct {
	Title    string // "" for the title it has
	Archived bool
}

// Limits is what a service can do. The chat front end uses it to avoid
// asking for what the service cannot do.
type Limits struct {
	MaxText    int // characters in one message
	MaxChoices int // choices in one message, as buttons or as a menu
	MaxFiles   int // files in one message
	// MaxFileBytes is the size of the largest file a message may carry;
	// 0 for no limit.
	MaxFileBytes int64
	Edit         bool
	Delete       bool
	SetStatus    bool
	SetThread    bool
	MultiSelect  bool // a Message can be Multi
	Commands     bool // Run registers a Commander's commands and delivers their use
}

// Factory makes a Driver from its section of the instance's config, which
// is opaque to everything but the driver. config is the instance's config
// directory, the one place a driver may read a secret from.
type Factory func(config *os.Root, settings []byte) (Driver, error)

var drivers registry.Registry[Factory]

// Register registers a driver's Factory under name. Registering a name twice
// is an error.
func Register(name string, f Factory) error { return drivers.Register(name, f) }

// Lookup returns the Factory registered under name.
func Lookup(name string) (Factory, bool) { return drivers.Lookup(name) }

// Names returns the names of the registered drivers.
func Names() []string { return drivers.Names() }
