package surface

import (
	"context"
	"time"
)

// ThreadLister is optional. A Driver without it cannot list chat threads.
// Only public threads of channel are returned, never private threads.
type ThreadLister interface {
	ListThreads(ctx context.Context, channel string, q ThreadQuery) (ThreadPage, error)
}

// ThreadQuery selects active or archived threads. Before is a Next cursor
// returned by the same query; it is opaque to callers. Limit is 1..100.
type ThreadQuery struct {
	Archived bool
	Before   string
	Limit    int
}

type ThreadPage struct {
	Threads []ChatThread
	Next    string // empty at the end; a page may be empty and still have Next
}

type ThreadType string

const (
	PublicThread  ThreadType = "public"
	PrivateThread ThreadType = "private"
)

type ChatThread struct {
	Conv     Conversation
	Name     string
	Type     ThreadType
	Archived bool
}

// MessageReader is optional. It reads service messages, including the
// driver's own posts, but not service notices. It downloads no files.
// A Driver without it cannot read history or an individual message.
type MessageReader interface {
	ReadMessages(ctx context.Context, conv Conversation, q MessageQuery) (MessagePage, error)
	ReadMessage(ctx context.Context, ref MessageRef) (HistoryMessage, error)
}

// MessageQuery reads at most Limit service messages (1..100), before or
// after a message id, exclusively. Both empty means the last Limit.
// Before and After cannot both be set. Notices consume places in a page.
type MessageQuery struct {
	Before string
	After  string
	Limit  int
}

// MessagePage is in chronological order. Before and After advance past all
// service messages read, including notices left out of Messages. Continue
// with the cursor for the requested direction until it is empty.
type MessagePage struct {
	Messages []HistoryMessage
	Type     ThreadType // empty for a channel or direct conversation
	Before   string
	After    string
}

type HistoryMessage struct {
	Ref       MessageRef
	Author    Author
	Time      time.Time
	Text      string
	ReplyTo   *MessageRef
	Forwarded []ForwardedMessage
	Files     []RemoteFile
	Own       bool // this driver's account; Ref identifies the post and its conversation
	Type      ThreadType
}

// ForwardedMessage has no author when the service reports none. It is
// outside text; forwarding does not make its text the sender's own.
type ForwardedMessage struct {
	Text  string
	Files []RemoteFile
}

// RemoteFile is metadata as the service lists it. URL is untrusted input,
// not a path on disk or permission to fetch it.
type RemoteFile struct {
	ID          string
	Name        string
	Size        int64
	ContentType string
	URL         string
}
