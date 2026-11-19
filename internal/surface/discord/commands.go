package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// Discord's limits on application commands (application commands reference).
const (
	maxCommands           = 100
	maxCommandArgs        = 25
	maxCommandDescription = 100
	// answerWithin is how long an interaction's token can answer it: the
	// service accepts edits and follow-ups for 15 minutes.
	answerWithin = 15 * time.Minute
)

var commandName = regexp.MustCompile(`^[-_a-z0-9]{1,32}$`)

// Discord's values the driver uses for commands.
const (
	interactionCommand     = 2
	commandChatInput       = 1
	optionString           = 3
	contextGuild           = 0
	installGuild           = 0
	callbackDeferMessage   = 5
	messageFlagEphemeral   = 64
	commandReplyPrefix     = "command:"
	answerOp               = "answer a command"
	registerCommandsOpName = "register the commands"
)

// wireCommand is a command as the driver registers it, and the part of a
// registered command it compares.
type wireCommand struct {
	Type                     int          `json:"type"`
	Name                     string       `json:"name"`
	Description              string       `json:"description"`
	Options                  []wireOption `json:"options,omitempty"`
	Contexts                 []int        `json:"contexts"`
	IntegrationTypes         []int        `json:"integration_types"`
	DefaultMemberPermissions *string      `json:"default_member_permissions"`
}

type wireOption struct {
	Type        int    `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

// wireCommands turns specs into the commands to register. Commands can be
// used in server channels and their threads, not in direct conversations.
func wireCommands(specs []surface.CommandSpec) ([]wireCommand, error) {
	if len(specs) > maxCommands {
		return nil, fmt.Errorf("discord: %d commands, at most %d can be registered", len(specs), maxCommands)
	}
	out := []wireCommand{}
	seen := map[string]bool{}
	for _, s := range specs {
		if !commandName.MatchString(s.Name) {
			return nil, fmt.Errorf("discord: command name %q: use 1 to 32 of a-z, 0-9, - and _", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("discord: command %q is given twice", s.Name)
		}
		seen[s.Name] = true
		if err := description(s.Description); err != nil {
			return nil, fmt.Errorf("discord: command %q: %w", s.Name, err)
		}
		if len(s.Args) > maxCommandArgs {
			return nil, fmt.Errorf("discord: command %q has %d arguments, at most %d fit", s.Name, len(s.Args), maxCommandArgs)
		}
		c := wireCommand{Type: commandChatInput, Name: s.Name, Description: s.Description,
			Contexts: []int{contextGuild}, IntegrationTypes: []int{installGuild}}
		args := map[string]bool{}
		optional := false
		for _, a := range s.Args {
			if !commandName.MatchString(a.Name) || args[a.Name] {
				return nil, fmt.Errorf("discord: command %q: argument name %q is not a name or is given twice", s.Name, a.Name)
			}
			args[a.Name] = true
			if err := description(a.Description); err != nil {
				return nil, fmt.Errorf("discord: command %q, argument %q: %w", s.Name, a.Name, err)
			}
			if a.Required && optional {
				return nil, fmt.Errorf("discord: command %q: required argument %q comes after an optional one", s.Name, a.Name)
			}
			optional = optional || !a.Required
			c.Options = append(c.Options, wireOption{Type: optionString, Name: a.Name, Description: a.Description, Required: a.Required})
		}
		out = append(out, c)
	}
	return out, nil
}

func description(s string) error {
	if n := utf8.RuneCountInString(s); n == 0 || n > maxCommandDescription {
		return fmt.Errorf("the description must have 1 to %d characters", maxCommandDescription)
	}
	return nil
}

// register makes specs the bot's commands. It reads the commands the
// service has and writes them only when they differ, so a restart with the
// same specs registers nothing.
func (d *Driver) register(ctx context.Context, specs []surface.CommandSpec) error {
	want, err := wireCommands(specs)
	if err != nil {
		return err
	}
	var app struct {
		ID snowflake.ID `json:"id"`
	}
	if err := d.client.Do(rest.GetCurrentApplication.Compile(nil), nil, &app, rest.WithCtx(ctx)); err != nil {
		return wrap(registerCommandsOpName, err)
	}
	var have []wireCommand
	if err := d.client.Do(rest.GetGlobalCommands.Compile(nil, app.ID), nil, &have, rest.WithCtx(ctx)); err != nil {
		return wrap(registerCommandsOpName, err)
	}
	d.mu.Lock()
	d.commands = map[string]surface.CommandSpec{}
	for _, s := range specs {
		d.commands[s.Name] = s
	}
	d.mu.Unlock()
	if sameCommands(have, want) {
		return nil
	}
	return wrap(registerCommandsOpName, d.client.Do(rest.SetGlobalCommands.Compile(nil, app.ID), want, nil, rest.WithCtx(ctx)))
}

func sameCommands(a, b []wireCommand) bool {
	byName := func(x, y wireCommand) int { return strings.Compare(x.Name, y.Name) }
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, byName)
	slices.SortFunc(b, byName)
	norm := func(cs []wireCommand) {
		for i := range cs {
			if len(cs[i].Options) == 0 {
				cs[i].Options = nil
			}
		}
	}
	norm(a)
	norm(b)
	return reflect.DeepEqual(a, b)
}

// answer is what the driver keeps to answer one command interaction.
// mu serializes the answers to it, so that two answers cannot both replace
// the deferred response.
type answer struct {
	mu      sync.Mutex
	app     snowflake.ID
	token   string
	private bool
	at      time.Time
	posted  map[string]*progress // by key
	started bool                 // the deferred response has been replaced
}

// progress is how far the answer posted with one key got, so that a retry
// after a failed part sends only the parts that are missing.
type progress struct {
	first surface.MessageRef
	sent  int
}

// command handles the use of a command. It is acknowledged first, as a
// deferred answer ("thinking"), so that the service's 3-second deadline is
// met however long the sink takes; the front end's Post with ReplyTo set to
// the command's Reply replaces it.
func (d *Driver) command(ctx context.Context, in wireInteraction, sink surface.Sink) error {
	d.mu.Lock()
	spec, known := d.commands[in.Data.Name]
	d.mu.Unlock()
	private := known && spec.Private
	body := map[string]any{"type": callbackDeferMessage}
	if private {
		body["data"] = map[string]any{"flags": messageFlagEphemeral}
	}
	actx, cancel := context.WithTimeout(ctx, ackWithin)
	err := d.client.Do(rest.CreateInteractionResponse.Compile(nil, in.ID, in.Token), body, nil, rest.WithCtx(actx))
	cancel()
	acked := err == nil
	if !acked {
		d.log.Warn("discord: a command was not acknowledged in time", "command", in.Data.Name, "err", err)
	}
	u := in.user()
	if u == nil {
		return errors.New("discord: a command without a user")
	}
	if u.Bot || u.ID == d.botID {
		return nil
	}
	conv := d.interactionConv(in)
	c := surface.Command{
		Conv:   conv,
		Author: surface.Author{Kind: surface.Person, ID: u.ID.String(), Name: u.name()},
		ID:     in.ID.String(),
		Name:   in.Data.Name,
	}
	for _, o := range in.Data.Options {
		var v string
		if err := json.Unmarshal(o.Value, &v); err != nil {
			v = string(o.Value) // a number or a flag, from a command registered otherwise
		}
		c.Args = append(c.Args, surface.Arg{Name: o.Name, Value: v})
	}
	if acked {
		app := in.ApplicationID
		if app == 0 {
			app = d.botID
		}
		d.mu.Lock()
		now := time.Now()
		for id, a := range d.answers {
			if now.Sub(a.at) > answerWithin {
				delete(d.answers, id)
			}
		}
		d.answers[in.ID] = &answer{app: app, token: in.Token, private: private, at: now, posted: map[string]*progress{}}
		d.mu.Unlock()
		c.Reply = &surface.MessageRef{Conv: conv, ID: commandReplyPrefix + in.ID.String()}
	}
	sink.Command(c)
	return nil
}

// postAnswer answers a command: the first part replaces the deferred
// response, and each further part is a follow-up. A repeated key sends only
// the parts that key has not sent yet, and returns its first message.
func (d *Driver) postAnswer(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	id, err := snowflake.Parse(strings.TrimPrefix(m.ReplyTo.ID, commandReplyPrefix))
	if err != nil {
		return surface.MessageRef{}, &Error{Op: answerOp, Local: true, Err: fmt.Errorf("%q is not a command", m.ReplyTo.ID)}
	}
	if len(m.Files) > 0 {
		return surface.MessageRef{}, &Error{Op: answerOp, Local: true, Err: errors.New("an answer to a command cannot carry files")}
	}
	buttons, err := components(m.Choices)
	if err != nil {
		return surface.MessageRef{}, err
	}
	text := m.Text
	for _, who := range slices.Backward(m.Notify) {
		text = fmt.Sprintf("<@%s> ", who) + text
	}
	if text == "" {
		return surface.MessageRef{}, errors.New("discord: nothing to post")
	}
	d.mu.Lock()
	a := d.answers[id]
	if a != nil && time.Since(a.at) > answerWithin {
		delete(d.answers, id)
		a = nil
	}
	d.mu.Unlock()
	if a == nil {
		return surface.MessageRef{}, &Error{Op: answerOp, Local: true, Err: errors.New("the command can no longer be answered")}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := &progress{}
	if key != "" {
		if a.posted[key] == nil {
			a.posted[key] = p
		}
		p = a.posted[key]
	}
	mentions := map[string]any{"parse": []string{}}
	var users []string
	for _, who := range m.Notify {
		if _, err := snowflake.Parse(who); err == nil {
			users = append(users, who)
		}
	}
	if len(users) > 0 {
		mentions["users"] = users
	}
	parts := split(text, maxText)
	for i := p.sent; i < len(parts); i++ {
		body := map[string]any{"content": parts[i], "allowed_mentions": mentions}
		if i == len(parts)-1 && len(m.Choices) > 0 {
			body["components"] = buttons
		}
		var out wireMessage
		if !a.started {
			err = d.client.Do(rest.UpdateInteractionResponse.Compile(nil, a.app, a.token), body, &out, rest.WithCtx(ctx))
		} else {
			if a.private {
				body["flags"] = messageFlagEphemeral
			}
			err = d.client.Do(rest.CreateFollowupMessage.Compile(nil, a.app, a.token), body, &out, rest.WithCtx(ctx))
		}
		if err != nil {
			return p.first, wrap(answerOp, err)
		}
		a.started = true
		if i == 0 {
			p.first = surface.MessageRef{Conv: conv, ID: out.ID.String()}
		}
		p.sent = i + 1
	}
	return p.first, nil
}
