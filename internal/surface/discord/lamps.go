package discord

import (
	"context"
	"fmt"
	"net/url"

	"github.com/disgoorg/disgo/rest"

	"github.com/yousan/hataori/internal/surface"
)

// lamps are the reactions that show a status. Waiting has its own, so
// that a question waiting for an answer does not look like work going on.
var lamps = map[surface.Status]string{
	surface.Working: "⏳", // hourglass
	surface.Waiting: "❔", // white question mark
	surface.Done:    "✅", // check mark
	surface.Failed:  "❌", // cross mark
}

// SetStatus shows s on a message as the bot's reaction. It adds the new
// lamp first and then removes the bot's other lamps, so the message is not
// left without one in between; NoStatus removes them all. Only the bot's
// own reactions are removed: other people's are left as they are.
//
// A call the service refuses, such as on a message that is gone (404) or
// without Add Reactions (403), returns an Error and is not tried again
// here: the outbox decides.
func (d *Driver) SetStatus(ctx context.Context, ref surface.MessageRef, s surface.Status) error {
	ch, err := target(ref.Conv)
	if err != nil {
		return err
	}
	id, err := parseID("message", ref.ID)
	if err != nil {
		return err
	}
	want, ok := lamps[s]
	if !ok && s != surface.NoStatus {
		return fmt.Errorf("discord: no lamp for status %d", s)
	}
	var m struct {
		Reactions []struct {
			Me    bool `json:"me"`
			Emoji struct {
				ID   *string `json:"id"`
				Name string  `json:"name"`
			} `json:"emoji"`
		} `json:"reactions"`
	}
	if err := d.client.Do(rest.GetMessage.Compile(nil, ch, id), nil, &m, rest.WithCtx(ctx)); err != nil {
		return wrap("set the status", err)
	}
	has := map[string]bool{}
	for _, r := range m.Reactions {
		if r.Me && r.Emoji.ID == nil {
			has[r.Emoji.Name] = true
		}
	}
	if want != "" && !has[want] {
		err := d.client.Do(rest.AddReaction.Compile(nil, ch, id, url.PathEscape(want)), nil, nil, rest.WithCtx(ctx))
		if err != nil {
			return wrap("set the status", err)
		}
	}
	for _, s := range []surface.Status{surface.Working, surface.Waiting, surface.Done, surface.Failed} {
		e := lamps[s]
		if e == want || !has[e] {
			continue
		}
		err := d.client.Do(rest.RemoveOwnReaction.Compile(nil, ch, id, url.PathEscape(e)), nil, nil, rest.WithCtx(ctx))
		if err != nil {
			return wrap("set the status", err)
		}
	}
	return nil
}
