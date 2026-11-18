package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// ErrCursor is what Since returns, wrapped, when the service cannot serve
// the conversation from the cursor: the cursor is not a message id, or the
// conversation is out of the bot's reach. It is not "no messages": that is
// an empty result and no error. A conversation that no longer exists is
// surface.ErrGone instead.
var ErrCursor = errors.New("discord: the service cannot serve messages from this cursor")

// Discord's JSON error codes for a conversation that is gone or hidden.
const (
	codeUnknownChannel = 10003
	codeMissingAccess  = 50001
	codeMissingPerms   = 50013
	codeInvalidForm    = 50035
)

// Since returns the messages in conv after cursor, oldest first, across as
// many pages as there are. An empty cursor means from the start. Messages
// posted through a webhook are included; the bot's own are not. Every
// message carries its own id as its cursor.
func (d *Driver) Since(ctx context.Context, conv surface.Conversation, cursor string) ([]surface.Inbound, error) {
	ch, err := target(conv)
	if err != nil {
		return nil, err
	}
	var after snowflake.ID
	if cursor != "" {
		if after, err = snowflake.Parse(cursor); err != nil || after == 0 {
			return nil, fmt.Errorf("%w: %q is not a message id", ErrCursor, cursor)
		}
	}
	direct := false
	if conv.Thread == "" {
		if direct, err = d.isDirect(ctx, ch); err != nil {
			return nil, cursorError(err)
		}
	}
	var out []surface.Inbound
	for first := true; ; first = false {
		var page []wireMessage
		q := discord.QueryValues{"after": after.String(), "limit": d.pageSize}
		if err := d.client.Do(rest.GetMessages.Compile(q, ch), nil, &page, rest.WithCtx(ctx)); err != nil {
			return nil, cursorError(wrap("read the history", err))
		}
		// A page fetched with after= comes newest first.
		slices.SortFunc(page, func(a, b wireMessage) int { return cmp(a.ID, b.ID) })
		for _, m := range page {
			if m.ID > after {
				after = m.ID
			}
			if m.Author.ID == d.botID && m.WebhookID == 0 {
				continue
			}
			out = append(out, m.inbound(conv, direct))
		}
		if first && len(page) == 0 && cursor != "" {
			// Without Read Message History the service answers an empty page,
			// not an error. Reading the cursor's own message needs that
			// permission, so it tells "nothing new" from "cannot read".
			if err := d.canRead(ctx, ch, after); err != nil {
				return nil, err
			}
		}
		if len(page) < d.pageSize {
			return out, nil
		}
	}
}

func cmp(a, b snowflake.ID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// canRead checks that the history of ch can be read, by reading the
// message msg in it. A message that is gone is no sign either way.
func (d *Driver) canRead(ctx context.Context, ch, msg snowflake.ID) error {
	err := wrap("read the history", d.client.Do(rest.GetMessage.Compile(nil, ch, msg), nil, nil, rest.WithCtx(ctx)))
	var e *Error
	if errors.As(err, &e) && e.Code == codeUnknownMessage {
		return nil
	}
	return cursorError(err)
}

// cursorError marks a refusal that means the conversation cannot be read
// from the cursor, and Unknown Channel as a conversation that is gone.
func cursorError(err error) error {
	var e *Error
	if errors.As(err, &e) && e.Code == codeUnknownChannel {
		return fmt.Errorf("%w: %w", surface.ErrGone, err)
	}
	if errors.As(err, &e) && (e.Code == codeMissingAccess || e.Code == codeMissingPerms ||
		e.Code == codeInvalidForm || e.Status == http.StatusNotFound) {
		return fmt.Errorf("%w: %w", ErrCursor, err)
	}
	return err
}

func (d *Driver) setDirect(ch snowflake.ID, direct bool) {
	d.mu.Lock()
	d.direct[ch] = direct
	d.mu.Unlock()
}

// isDirect reports whether ch is a direct conversation: from what the
// driver saw, or from the service once.
func (d *Driver) isDirect(ctx context.Context, ch snowflake.ID) (bool, error) {
	d.mu.Lock()
	direct, ok := d.direct[ch]
	d.mu.Unlock()
	if ok {
		return direct, nil
	}
	var c wireChannel
	if err := d.client.Do(rest.GetChannel.Compile(nil, ch), nil, &c, rest.WithCtx(ctx)); err != nil {
		return false, wrap("look up the conversation", err)
	}
	direct = c.Type == 1 || c.Type == 3 // a direct or group direct conversation
	d.setDirect(ch, direct)
	return direct, nil
}
