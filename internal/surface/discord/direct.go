package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// Discord's limits on buttons and menus (message components reference).
const (
	maxChoices       = 25 // buttons in one message (5 rows of 5), and options in one menu
	maxButtonsPerRow = 5
	maxCustomID      = 100 // a button's custom_id, and an option's value
	maxButtonLabel   = 80
	maxOptionLabel   = 100
	// menuID is the custom_id of the menu the driver posts. A selection
	// carries the values of the options chosen, which are the tokens.
	menuID = "hataori-menu"
	// ackWithin is how long the driver gives itself to acknowledge a
	// press; the service's deadline is 3 seconds.
	ackWithin = 2500 * time.Millisecond
)

// OpenDirect returns the direct conversation with the person whose user id
// is who. Discord returns the same one each time.
func (d *Driver) OpenDirect(ctx context.Context, who string) (surface.Conversation, error) {
	id, err := parseID("user", who)
	if err != nil {
		return surface.Conversation{}, err
	}
	var c wireChannel
	err = d.client.Do(rest.CreateDMChannel.Compile(nil), map[string]any{"recipient_id": id}, &c, rest.WithCtx(ctx))
	if err != nil {
		return surface.Conversation{}, wrap("open a direct conversation", err)
	}
	d.setDirect(c.ID, true)
	return surface.Conversation{Channel: c.ID.String()}, nil
}

// components renders choices: as buttons in rows of five, or, when multi is
// set, as one menu in which several can be chosen. No choices render none,
// which removes the buttons and the menu of an edited message.
func components(choices []surface.Choice, multi bool) ([]map[string]any, error) {
	if len(choices) > maxChoices {
		return nil, fmt.Errorf("discord: %d choices, at most %d fit in one message", len(choices), maxChoices)
	}
	if len(choices) == 0 {
		return []map[string]any{}, nil
	}
	seen := map[string]bool{}
	for _, c := range choices {
		if c.Token == "" || len(c.Token) > maxCustomID {
			return nil, fmt.Errorf("discord: a choice's token must have 1 to %d bytes", maxCustomID)
		}
		if seen[c.Token] {
			return nil, fmt.Errorf("discord: two choices have the token %q", c.Token)
		}
		seen[c.Token] = true
		if strings.TrimSpace(c.Label) == "" {
			return nil, fmt.Errorf("discord: the choice %q has no label", c.Token)
		}
	}
	if multi {
		var options []map[string]any
		for _, c := range choices {
			options = append(options, map[string]any{"label": cutRunes(c.Label, maxOptionLabel), "value": c.Token})
		}
		menu := map[string]any{"type": 3, "custom_id": menuID, "options": options,
			"min_values": 1, "max_values": len(choices)}
		return []map[string]any{{"type": 1, "components": []map[string]any{menu}}}, nil
	}
	var rows []map[string]any
	for i := 0; i < len(choices); i += maxButtonsPerRow {
		var buttons []map[string]any
		for _, c := range choices[i:min(i+maxButtonsPerRow, len(choices))] {
			label := c.Label
			if utf8.RuneCountInString(label) > maxButtonLabel {
				label = cutRunes(label, maxButtonLabel)
			}
			buttons = append(buttons, map[string]any{"type": 2, "style": 1, "label": label, "custom_id": c.Token})
		}
		rows = append(rows, map[string]any{"type": 1, "components": buttons})
	}
	return rows, nil
}

// wireInteraction is the part of an interaction the driver reads.
type wireInteraction struct {
	ID            snowflake.ID `json:"id"`
	ApplicationID snowflake.ID `json:"application_id"`
	Type          int          `json:"type"`
	Token         string       `json:"token"`
	ChannelID     snowflake.ID `json:"channel_id"`
	GuildID       snowflake.ID `json:"guild_id"`
	Channel       wireChannel  `json:"channel"`
	Data          struct {
		ComponentType int      `json:"component_type"`
		CustomID      string   `json:"custom_id"`
		Values        []string `json:"values"` // of a menu
		Name          string   `json:"name"`   // of a command
		Options       []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"options"`
	} `json:"data"`
	User   *wireUser `json:"user"` // in a direct conversation
	Member *struct {
		User wireUser `json:"user"`
	} `json:"member"` // in a server
	Message struct {
		ID        snowflake.ID `json:"id"`
		ChannelID snowflake.ID `json:"channel_id"`
	} `json:"message"`
}

type wireUser struct {
	ID         snowflake.ID `json:"id"`
	Username   string       `json:"username"`
	GlobalName string       `json:"global_name"`
	Bot        bool         `json:"bot"`
}

func (u *wireUser) name() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// user is who used the interaction: user in a direct conversation, the
// member's user in a server.
func (in *wireInteraction) user() *wireUser {
	if in.User != nil {
		return in.User
	}
	if in.Member != nil {
		return &in.Member.User
	}
	return nil
}

// interactionConv is where an interaction was used. A thread is named with
// its channel, from the channel object the interaction carries.
func (d *Driver) interactionConv(in wireInteraction) surface.Conversation {
	if isThread(in.Channel.Type) && in.Channel.ParentID != 0 {
		d.setParent(in.Channel)
		return surface.Conversation{Channel: in.Channel.ParentID.String(), Thread: in.ChannelID.String()}
	}
	return surface.Conversation{Channel: in.ChannelID.String()}
}

// Interaction and callback types the driver uses.
const (
	interactionComponent  = 3
	componentStringSelect = 3
	callbackDeferUpdate   = 6
)

// interaction handles a button press or the use of a command.
func (d *Driver) interaction(ctx context.Context, payload io.Reader, sink surface.Sink) error {
	var in wireInteraction
	if err := json.NewDecoder(payload).Decode(&in); err != nil {
		return err
	}
	switch in.Type {
	case interactionComponent:
		return d.press(ctx, in, sink)
	case interactionCommand:
		return d.command(ctx, in, sink)
	}
	return nil
}

// press handles a button press. It is acknowledged first, as a deferred
// update that changes nothing on screen, so that the service's 3-second
// deadline is met however long the sink takes. The press carries the
// choice's token in the message itself, so a button posted before a
// restart still works after it.
func (d *Driver) press(ctx context.Context, in wireInteraction, sink surface.Sink) error {
	actx, cancel := context.WithTimeout(ctx, ackWithin)
	err := d.client.Do(rest.CreateInteractionResponse.Compile(nil, in.ID, in.Token),
		map[string]any{"type": callbackDeferUpdate}, nil, rest.WithCtx(actx))
	cancel()
	if err != nil {
		d.log.Warn("discord: a button press was not acknowledged in time", "err", err)
	}
	u := in.user()
	if u == nil {
		return fmt.Errorf("discord: a press without a user")
	}
	if u.ID == d.botID {
		return nil
	}
	conv := d.interactionConv(in)
	sel := surface.Selection{
		Ref:    surface.MessageRef{Conv: conv, ID: in.Message.ID.String()},
		Author: surface.Author{Kind: surface.Person, ID: u.ID.String(), Name: u.name()},
		Token:  in.Data.CustomID,
		Tokens: []string{in.Data.CustomID},
	}
	if in.Data.ComponentType == componentStringSelect {
		if len(in.Data.Values) == 0 {
			return nil // nothing chosen
		}
		sel.Token, sel.Tokens = in.Data.Values[0], slices.Clone(in.Data.Values)
	}
	sink.Choice(sel)
	return nil
}
