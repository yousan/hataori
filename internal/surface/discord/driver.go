package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// Name is the name the driver registers under.
const Name = "discord"

// Intents are the gateway intents the driver asks for. MESSAGE_CONTENT is
// privileged and must be switched on for the application.
//
//   - GUILDS: threads being created and changed, to know a thread's channel.
//   - GUILD_MESSAGES: messages in channels and threads.
//   - MESSAGE_CONTENT: the text of those messages, which a webhook post does
//     not get without it because it does not mention the bot.
//   - DIRECT_MESSAGES: messages in direct conversations, for the owner's
//     replies to the login message. Their text comes without
//     MESSAGE_CONTENT.
//
// A button press or the use of a command (INTERACTION_CREATE) needs no
// intent.
//
// The bot's role in an enabled channel needs: View Channel, Read Message
// History, Create Public Threads, Send Messages in Threads, Attach Files,
// and Add Reactions for lamps. It needs no Manage permission. A direct
// conversation and a button in it need no permission.
const Intents = gateway.IntentGuilds | gateway.IntentGuildMessages | gateway.IntentMessageContent |
	gateway.IntentDirectMessages

// Settings is the driver's section of the instance's config.
type Settings struct {
	// BotID is the bot's user id. Run refuses to work as any other bot.
	BotID string `json:"bot_id"`
	// TokenFile names the file in the instance's config directory that
	// holds the bot token.
	TokenFile string `json:"token_file"`
	// CommandPrefix is put before the name of every command the driver
	// registers, so that several instances in one server, each with its
	// own bot, register different names: with "stg-", the front end's
	// "new" is registered as "/stg-new". The front end still gets "new".
	// Empty registers the front end's names.
	CommandPrefix string `json:"command_prefix,omitempty"`
}

// Driver is the Discord surface driver.
type Driver struct {
	botID      snowflake.ID
	token      string
	client     rest.Client
	gatewayURL string
	log        *slog.Logger

	pageSize int    // messages per history request
	prefix   string // before every registered command's name

	mu      sync.Mutex
	parents map[snowflake.ID]snowflake.ID // thread -> the channel it is in
	direct  map[snowflake.ID]bool         // channel -> whether it is a direct conversation

	commands map[string]surface.CommandSpec // by the name registered by Run, with the front end's name
	answers  map[snowflake.ID]*answer       // command interactions that can still be answered

	lampLocks [64]sync.Mutex // SetStatus, by message id
}

var _ surface.Driver = (*Driver)(nil)

// New is the driver's surface.Factory. The token is read from config, the
// instance's config directory, and nowhere else.
func New(config *os.Root, settings []byte) (surface.Driver, error) {
	var s Settings
	dec := json.NewDecoder(strings.NewReader(string(settings)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("discord: settings: %w", err)
	}
	id, err := snowflake.Parse(strings.TrimSpace(s.BotID))
	if err != nil || id == 0 {
		return nil, fmt.Errorf("discord: settings: %q: bot_id %q is not an id", "bot_id", s.BotID)
	}
	name := strings.TrimSpace(s.TokenFile)
	if name == "" || filepath.IsAbs(name) || !filepath.IsLocal(name) {
		return nil, fmt.Errorf("discord: settings: %q: %q is not a file name inside the config directory", "token_file", s.TokenFile)
	}
	if config == nil {
		return nil, errors.New("discord: no config directory")
	}
	b, err := config.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("discord: read the token file %q in the config directory: %w", name, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("discord: the token file %q is empty", name)
	}
	prefix := strings.TrimSpace(s.CommandPrefix)
	if prefix != "" && !commandPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("discord: settings: %q: %q: use up to 31 of a-z, 0-9, - and _", "command_prefix", s.CommandPrefix)
	}
	d := newDriver(id, token, fmt.Sprintf("%sv%d", rest.API, rest.Version), gateway.URL, nil)
	d.prefix = prefix
	return d, nil
}

func newDriver(botID snowflake.ID, token, restURL, gatewayURL string, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	return &Driver{
		botID:      botID,
		token:      token,
		client:     rest.NewClient(token, rest.WithURL(restURL), rest.WithLogger(log)),
		gatewayURL: gatewayURL,
		log:        log,
		pageSize:   100,
		parents:    map[snowflake.ID]snowflake.ID{},
		direct:     map[snowflake.ID]bool{},
		commands:   map[string]surface.CommandSpec{},
		answers:    map[snowflake.ID]*answer{},
	}
}

// Run connects and delivers inbound messages to sink until ctx ends. It
// first checks that the token is the configured bot's. A dropped
// connection is reopened and the session resumed; events are handled one at
// a time, in the order the service sends them, so inbound order per
// conversation is kept.
//
// When sink implements surface.Reconnected, Run calls Connected each time a
// session is ready: after the first one, and after each resume or new
// session. It is called in order with the messages, before any message the
// session delivers.
//
// When sink implements surface.Commander, Run first makes its commands the
// bot's commands (they can be used in server channels and their threads),
// each under its name with the configured prefix before it, and fails when
// the service refuses them.
func (d *Driver) Run(ctx context.Context, sink surface.Sink) error {
	var me struct {
		ID  snowflake.ID `json:"id"`
		Bot bool         `json:"bot"`
	}
	if err := d.client.Do(rest.GetCurrentUser.Compile(nil), nil, &me, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("discord: check the bot's identity: %w", err)
	}
	if me.ID != d.botID {
		return fmt.Errorf("discord: the token is for bot %s, but the config names bot %s", me.ID, d.botID)
	}
	if c, ok := sink.(surface.Commander); ok {
		if err := d.register(ctx, c.Commands()); err != nil {
			return err
		}
	}

	fatal := make(chan error, 1)
	report := func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}
	// lost is a dropped connection that may be resumed. Reconnecting is
	// done here, under ctx, rather than by the library on a context of its
	// own, so that nothing reconnects or delivers after Run returns.
	lost := make(chan error, 1)
	var stopped atomic.Bool
	q := newQueue(sink)
	defer q.close()
	handle := func(_ gateway.Gateway, t gateway.EventType, _ int, ev gateway.EventData) {
		if stopped.Load() {
			return
		}
		switch e := ev.(type) {
		case gateway.EventReady:
			if e.User.ID != d.botID {
				report(fmt.Errorf("discord: logged in as bot %s, but the config names bot %s", e.User.ID, d.botID))
				return
			}
			q.connected() // a new session
		case gateway.EventResumed:
			q.connected() // the session resumed after a drop
		case gateway.EventRaw:
			if err := d.dispatch(ctx, string(e.EventType), e.Payload, q); err != nil {
				d.log.Warn("discord: event skipped", "type", e.EventType, "err", err)
			}
		}
	}
	gw := gateway.New(d.token, handle,
		gateway.WithURL(d.gatewayURL),
		gateway.WithIntents(Intents),
		gateway.WithCompression(gateway.CompressionNone),
		gateway.WithEnableRawEvents(true),
		gateway.WithAutoReconnect(false),
		gateway.WithLogger(d.log),
		gateway.WithBrowser("hataori"),
		gateway.WithDevice("hataori"),
		gateway.WithCloseHandler(func(_ gateway.Gateway, err error, reconnect bool) {
			if !reconnect {
				report(fmt.Errorf("discord: the gateway closed and may not reconnect: %w", err))
				return
			}
			select {
			case lost <- err:
			default:
			}
		}),
	)
	defer func() {
		stopped.Store(true)
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gw.Close(cctx)
	}()
	for {
		// Open retries with a growing delay until it connects, ctx ends or
		// the service refuses for good; after a drop it resumes the session.
		if err := gw.Open(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("discord: connect to the gateway: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		case err := <-lost:
			d.log.Warn("discord: gateway connection lost; reconnecting", "err", err)
		}
	}
}

// dispatch handles one gateway event.
func (d *Driver) dispatch(ctx context.Context, t string, payload io.Reader, sink surface.Sink) error {
	switch t {
	case "THREAD_CREATE", "THREAD_UPDATE":
		var c wireChannel
		if err := json.NewDecoder(payload).Decode(&c); err != nil {
			return err
		}
		d.setParent(c)
		renamed(sink, c)
	case "GUILD_CREATE":
		var g struct {
			Threads []wireChannel `json:"threads"`
		}
		if err := json.NewDecoder(payload).Decode(&g); err != nil {
			return err
		}
		for _, c := range g.Threads {
			d.setParent(c)
			renamed(sink, c)
		}
	case "INTERACTION_CREATE":
		return d.interaction(ctx, payload, sink)
	case "MESSAGE_CREATE":
		var m wireMessage
		if err := json.NewDecoder(payload).Decode(&m); err != nil {
			return err
		}
		if m.Author.ID == d.botID && m.WebhookID == 0 {
			return nil // the bot's own message
		}
		conv, err := d.conversation(ctx, m)
		if err != nil {
			return err
		}
		if conv.Thread == "" {
			d.setDirect(m.ChannelID, m.GuildID == 0)
		}
		sink.Message(m.inbound(conv, m.GuildID == 0))
	}
	return nil
}

// renamed tells sink the name of thread c, when sink wants names: on its
// creation, on any change of it (Discord sends the whole thread), and for
// each active thread when the driver connects.
func renamed(sink surface.Sink, c wireChannel) {
	r, ok := sink.(surface.Renamer)
	if !ok || !isThread(c.Type) || c.ParentID == 0 || c.Name == "" {
		return
	}
	r.Renamed(surface.Conversation{Channel: c.ParentID.String(), Thread: c.ID.String()}, c.Name)
}

func (d *Driver) setParent(c wireChannel) {
	if isThread(c.Type) && c.ParentID != 0 {
		d.mu.Lock()
		d.parents[c.ID] = c.ParentID
		d.mu.Unlock()
	}
}

// conversation is where m was posted. A message in a thread names the
// thread; its channel comes from the thread events, or from the service
// when the thread was not seen.
func (d *Driver) conversation(ctx context.Context, m wireMessage) (surface.Conversation, error) {
	if m.ChannelType == nil || !isThread(*m.ChannelType) {
		return surface.Conversation{Channel: m.ChannelID.String()}, nil
	}
	parent, err := d.parentOf(ctx, m.ChannelID)
	if err != nil {
		return surface.Conversation{}, err
	}
	return surface.Conversation{Channel: parent.String(), Thread: m.ChannelID.String()}, nil
}

func (d *Driver) parentOf(ctx context.Context, thread snowflake.ID) (snowflake.ID, error) {
	d.mu.Lock()
	p, ok := d.parents[thread]
	d.mu.Unlock()
	if ok {
		return p, nil
	}
	var c wireChannel
	if err := d.client.Do(rest.GetChannel.Compile(nil, thread), nil, &c, rest.WithCtx(ctx)); err != nil {
		return 0, fmt.Errorf("discord: look up thread %s: %w", thread, err)
	}
	if c.ParentID == 0 {
		return 0, fmt.Errorf("discord: thread %s has no channel", thread)
	}
	d.setParent(c)
	return c.ParentID, nil
}

// Discord's channel types for threads.
func isThread(t int) bool { return t == 10 || t == 11 || t == 12 }

// wireChannel is the part of a channel the driver reads.
type wireChannel struct {
	ID       snowflake.ID `json:"id"`
	Type     int          `json:"type"`
	ParentID snowflake.ID `json:"parent_id"`
	Name     string       `json:"name"`
}

// wireMessage is the part of a message the driver reads, from the gateway
// and from REST alike.
type wireMessage struct {
	ID          snowflake.ID `json:"id"`
	ChannelID   snowflake.ID `json:"channel_id"`
	ChannelType *int         `json:"channel_type"` // gateway only
	GuildID     snowflake.ID `json:"guild_id"`     // absent in a direct message
	Type        int          `json:"type"`
	Content     string       `json:"content"`
	Timestamp   time.Time    `json:"timestamp"`
	WebhookID   snowflake.ID `json:"webhook_id"`
	Author      struct {
		ID         snowflake.ID `json:"id"`
		Username   string       `json:"username"`
		GlobalName string       `json:"global_name"`
		Bot        bool         `json:"bot"`
		System     bool         `json:"system"`
	} `json:"author"`
	MessageReference *struct {
		Type      int          `json:"type"`
		MessageID snowflake.ID `json:"message_id"`
		ChannelID snowflake.ID `json:"channel_id"`
	} `json:"message_reference"`
}

// Message types that a person or a poster writes; every other type is the
// service's own notice.
const (
	typeDefault = 0
	typeReply   = 19
)

// inbound turns m, posted in conv, into a surface.Inbound. direct is
// whether conv is a direct conversation: a gateway message says so by having
// no guild_id, but a message read through REST has none either way.
func (m wireMessage) inbound(conv surface.Conversation, direct bool) surface.Inbound {
	in := surface.Inbound{
		Ref:     surface.MessageRef{Conv: conv, ID: m.ID.String()},
		Text:    m.Content,
		Direct:  direct,
		Service: m.Author.System || (m.Type != typeDefault && m.Type != typeReply),
		Time:    m.Timestamp,
		Cursor:  m.ID.String(),
	}
	name := m.Author.GlobalName
	if name == "" {
		name = m.Author.Username
	}
	switch {
	case m.WebhookID != 0:
		// A webhook post's author is the webhook under the name it posted
		// with; author.bot is true for it too, so webhook_id decides.
		in.Author = surface.Author{Kind: surface.Webhook, ID: m.WebhookID.String(), Name: m.Author.Username}
	case m.Author.Bot:
		in.Author = surface.Author{Kind: surface.Bot, ID: m.Author.ID.String(), Name: name}
	default:
		in.Author = surface.Author{Kind: surface.Person, ID: m.Author.ID.String(), Name: name}
	}
	if m.Type == typeReply && m.MessageReference != nil && m.MessageReference.MessageID != 0 {
		in.ReplyTo = &surface.MessageRef{Conv: conv, ID: m.MessageReference.MessageID.String()}
	}
	return in
}
