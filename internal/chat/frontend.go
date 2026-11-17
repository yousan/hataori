package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// Scope is the chat front end's config of one scope, a channel.
type Scope struct {
	Enabled bool
	// Posters may start threads besides the owner: principals, which for
	// chat are the authors' ids on the service (a webhook's id for a
	// webhook post).
	Posters []control.Principal
}

// FrontEnd shows threads in one chat service. It is the surface.Sink of its
// driver and the control.ScopeOwner of its scopes.
type FrontEnd struct {
	Name    string // the client's name, as scopes name their owner
	Driver  surface.Driver
	Outbox  *Outbox
	API     func(control.Caller) control.API // the core, as one caller
	Trigger string                           // the word that starts a thread
	Owner   control.Principal
	Scopes  map[string]Scope // by channel
	Log     *slog.Logger

	ctx context.Context
}

var (
	_ surface.Sink       = (*FrontEnd)(nil)
	_ control.ScopeOwner = (*FrontEnd)(nil)
)

// Run connects the driver and sends the outbox until ctx ends.
func (f *FrontEnd) Run(ctx context.Context) error {
	f.ctx = ctx
	done := make(chan error, 1)
	go func() { done <- f.Outbox.Run(ctx) }()
	err := f.Driver.Run(ctx, f)
	<-done
	return err
}

func (f *FrontEnd) context() context.Context {
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}

// NewThread opens a chat thread in the channel scope and returns its id.
func (f *FrontEnd) NewThread(ctx context.Context, scope, title string) (string, error) {
	conv, err := f.Driver.OpenThread(ctx, surface.Conversation{Channel: scope}, title)
	if err != nil {
		return "", err
	}
	return conv.Thread, nil
}

// Message handles an inbound message. In a channel, a trigger starts a
// thread; everything else is ignored.
func (f *FrontEnd) Message(in surface.Inbound) {
	if in.Direct || in.Ref.Conv.Thread != "" {
		return
	}
	o, ok := parseTrigger(f.Trigger, in.Text)
	if !ok {
		return
	}
	channel := in.Ref.Conv.Channel
	refuse := func(reason string) {
		f.Log.Warn("trigger ignored", "reason", reason, "scope", channel,
			"author", in.Author.ID, "message", in.Ref.ID)
	}
	sc, known := f.Scopes[channel]
	switch who := control.Principal(in.Author.ID); {
	case in.Author.Kind == surface.Bot:
		refuse("the author is a bot account")
		return
	case !known || !sc.Enabled:
		refuse("the scope is not enabled")
		return
	case who != f.Owner && !slices.Contains(sc.Posters, who):
		refuse("the author is not allowed")
		return
	case o.Repo == "":
		refuse("no repo:<url> on the trigger line")
		return
	case o.Prompt == "":
		refuse("no prompt")
		return
	}
	f.start(f.context(), in, o)
}

func (f *FrontEnd) start(ctx context.Context, in surface.Inbound, o order) {
	channel := in.Ref.Conv.Channel
	api := f.API(control.Caller{Principal: control.Principal(in.Author.ID), Scopes: []string{channel},
		May: control.OpRead | control.OpStart})
	id, startErr := api.Start(ctx, control.StartRequest{Key: in.Ref.ID, Scope: channel, Repo: o.Repo,
		Input: control.Input{Text: o.Prompt}})
	if id.Key == "" {
		f.Log.Error("trigger: start failed", "scope", channel, "message", in.Ref.ID, "err", startErr)
		return
	}
	// The thread's rows are keyed by its first event, so a trigger
	// delivered again writes nothing twice.
	pos, err := firstEvent(ctx, api, id)
	if err != nil {
		f.Log.Error("trigger: reading the new thread", "thread", id.Key, "err", err)
		return
	}
	conv := surface.Conversation{Channel: channel, Thread: id.Key}
	rows := []Row{{Event: pos, Part: 0, Conv: conv, Message: surface.Message{Text: o.Prompt, Quiet: true}}}
	if startErr != nil {
		rows = append(rows, Row{Event: pos, Part: 1, Conv: conv,
			Message: surface.Message{Text: "This thread could not start: " + startErr.Error()}})
	}
	if err := f.Outbox.Write(ctx, rows...); err != nil {
		f.Log.Error("trigger: writing the prompt", "thread", id.Key, "err", err)
	}
}

var errNoEvent = errors.New("the thread has no events")

// firstEvent returns the position of a thread's first event.
func firstEvent(ctx context.Context, api control.API, id control.ThreadID) (uint64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := api.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		return 0, err
	}
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		return e.Pos, nil
	}
	return 0, fmt.Errorf("chat: thread %s: %w", id.Key, errNoEvent)
}

// Choice handles the use of a button; there are none yet.
func (f *FrontEnd) Choice(surface.Selection) {}

// Command handles a command; there are none yet.
func (f *FrontEnd) Command(surface.Command) {}
