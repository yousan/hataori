package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// Discord's limits on a button (message components reference).
const (
	maxChoices     = 1 // the driver posts one button; menus come after phase 1
	maxCustomID    = 100
	maxButtonLabel = 80
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

// components renders choices as one row of buttons; nil choices render
// none, which removes the buttons of an edited message.
func components(choices []surface.Choice) ([]map[string]any, error) {
	if len(choices) > maxChoices {
		return nil, fmt.Errorf("discord: %d choices, at most %d fit in one message", len(choices), maxChoices)
	}
	if len(choices) == 0 {
		return []map[string]any{}, nil
	}
	var buttons []map[string]any
	for _, c := range choices {
		if c.Token == "" || len(c.Token) > maxCustomID {
			return nil, fmt.Errorf("discord: a choice's token must have 1 to %d bytes", maxCustomID)
		}
		label := c.Label
		if utf8.RuneCountInString(label) > maxButtonLabel {
			label = cutRunes(label, maxButtonLabel)
		}
		buttons = append(buttons, map[string]any{"type": 2, "style": 1, "label": label, "custom_id": c.Token})
	}
	return []map[string]any{{"type": 1, "components": buttons}}, nil
}

// wireInteraction is the part of an interaction the driver reads.
type wireInteraction struct {
	ID        snowflake.ID `json:"id"`
	Type      int          `json:"type"`
	Token     string       `json:"token"`
	ChannelID snowflake.ID `json:"channel_id"`
	GuildID   snowflake.ID `json:"guild_id"`
	Data      struct {
		ComponentType int    `json:"component_type"`
		CustomID      string `json:"custom_id"`
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
}

// Interaction and callback types the driver uses.
const (
	interactionComponent = 3
	callbackDeferUpdate  = 6
)

// press handles a button press. It is acknowledged first, as a deferred
// update that changes nothing on screen, so that the service's 3-second
// deadline is met however long the sink takes. The press carries the
// choice's token in the message itself, so a button posted before a
// restart still works after it.
func (d *Driver) press(ctx context.Context, payload io.Reader, sink surface.Sink) error {
	var in wireInteraction
	if err := json.NewDecoder(payload).Decode(&in); err != nil {
		return err
	}
	if in.Type != interactionComponent {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, ackWithin)
	err := d.client.Do(rest.CreateInteractionResponse.Compile(nil, in.ID, in.Token),
		map[string]any{"type": callbackDeferUpdate}, nil, rest.WithCtx(actx))
	cancel()
	if err != nil {
		d.log.Warn("discord: a button press was not acknowledged in time", "err", err)
	}
	u := in.User
	if u == nil && in.Member != nil {
		u = &in.Member.User
	}
	if u == nil {
		return fmt.Errorf("discord: a press without a user")
	}
	if u.ID == d.botID {
		return nil
	}
	name := u.GlobalName
	if name == "" {
		name = u.Username
	}
	conv := surface.Conversation{Channel: in.ChannelID.String()}
	if in.GuildID != 0 {
		if c, err := d.conversation(ctx, wireMessage{ChannelID: in.ChannelID}); err == nil {
			conv = c
		}
	}
	sink.Choice(surface.Selection{
		Ref:    surface.MessageRef{Conv: conv, ID: in.Message.ID.String()},
		Author: surface.Author{Kind: surface.Person, ID: u.ID.String(), Name: name},
		Token:  in.Data.CustomID,
	})
	return nil
}
