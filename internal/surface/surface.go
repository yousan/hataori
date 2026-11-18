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

// Driver is one chat service.
type Driver interface {
	// Run connects and delivers inbound events to sink until ctx ends.
	// It reconnects by itself and calls sink in order per conversation.
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
	// supports it.
	Post(ctx context.Context, conv Conversation, key string, m Message) (MessageRef, error)
	// Edit replaces a posted message.
	Edit(ctx context.Context, ref MessageRef, m Message) error
	// Delete deletes a posted message.
	Delete(ctx context.Context, ref MessageRef) error
	// SetStatus shows a lamp on a message.
	SetStatus(ctx context.Context, ref MessageRef, s Status) error
	// SetThread sets a thread's title and archived flag.
	SetThread(ctx context.Context, conv Conversation, t ThreadMeta) error

	// Limits reports message length, choices per message, and which
	// optional calls work.
	Limits() Limits
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

// Reconnected is implemented by a Sink that wants to know when the driver
// has connected: Run calls Connected after it connects and after each
// reconnect, so that the sink can catch up on what it missed with Since.
type Reconnected interface {
	Connected()
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
	Token  string // the token of the Choice that was used
}

// Command is a slash or text command.
type Command struct {
	Conv   Conversation
	Author Author
	ID     string // safe-to-repeat key of the command
	Name   string
	Args   string
}

// Message says what to show, not how.
type Message struct {
	Text    string
	Files   []File
	Choices []Choice    // rendered as buttons or a menu; each carries an opaque token
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
	Title    string
	Archived bool
}

// Limits is what a service can do. The chat front end uses it to avoid
// asking for what the service cannot do.
type Limits struct {
	MaxText    int // characters in one message
	MaxChoices int // choices in one message
	MaxFiles   int // files in one message
	// MaxFileBytes is the size of the largest file a message may carry;
	// 0 for no limit.
	MaxFileBytes int64
	Edit         bool
	Delete       bool
	SetStatus    bool
	SetThread    bool
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
