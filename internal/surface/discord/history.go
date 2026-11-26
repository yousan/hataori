package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/yousan/hataori/internal/surface"
)

var (
	_ surface.ThreadLister  = (*Driver)(nil)
	_ surface.MessageReader = (*Driver)(nil)
)

type historyChannel struct {
	wireChannel
	GuildID       snowflake.ID `json:"guild_id"`
	LastMessageID snowflake.ID `json:"last_message_id"`
	Metadata      *struct {
		Archived  bool      `json:"archived"`
		Timestamp time.Time `json:"archive_timestamp"`
	} `json:"thread_metadata"`
}

func threadType(t int) surface.ThreadType {
	switch t {
	case 10, 11:
		return surface.PublicThread
	case 12:
		return surface.PrivateThread
	}
	return ""
}

func readLimit(n int) error {
	if n < 1 || n > 100 {
		return fmt.Errorf("discord: read limit must be between 1 and 100")
	}
	return nil
}

func messageID(s string) (snowflake.ID, error) {
	id, err := snowflake.Parse(s)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("discord: %q is not a message id", s)
	}
	return id, nil
}

func (d *Driver) historyChannel(ctx context.Context, conv surface.Conversation) (historyChannel, error) {
	ch, err := target(conv)
	if err != nil {
		return historyChannel{}, err
	}
	var c historyChannel
	if err := d.client.Do(rest.GetChannel.Compile(nil, ch), nil, &c, rest.WithCtx(ctx)); err != nil {
		return c, cursorError(wrap("read the conversation", err))
	}
	if c.ID != ch {
		return c, fmt.Errorf("discord: the service returned another conversation")
	}
	if conv.Thread != "" && (!isThread(c.Type) || c.ParentID.String() != conv.Channel) {
		return c, fmt.Errorf("discord: the thread is not in the requested channel")
	}
	if conv.Thread == "" && isThread(c.Type) {
		return c, fmt.Errorf("discord: a thread needs its parent channel")
	}
	return c, nil
}

// ListThreads reads one page of public threads. Discord's active endpoint
// is not paged; the driver filters it to the channel and pages it by id.
// Archived pages use the service's archive_timestamp cursor instead.
func (d *Driver) ListThreads(ctx context.Context, channel string, q surface.ThreadQuery) (surface.ThreadPage, error) {
	var out surface.ThreadPage
	if err := readLimit(q.Limit); err != nil {
		return out, err
	}
	var before snowflake.ID
	if q.Before != "" {
		if q.Archived {
			if _, err := time.Parse(time.RFC3339Nano, q.Before); err != nil {
				return out, fmt.Errorf("discord: invalid archive cursor: %w", err)
			}
		} else {
			var err error
			before, err = messageID(q.Before)
			if err != nil {
				return out, err
			}
		}
	}
	c, err := d.historyChannel(ctx, surface.Conversation{Channel: channel})
	if err != nil {
		return out, err
	}
	if c.GuildID == 0 || isThread(c.Type) {
		return out, fmt.Errorf("discord: conversation is not a server channel")
	}
	var page struct {
		Threads []historyChannel `json:"threads"`
		More    bool             `json:"has_more"`
	}
	if q.Archived {
		query := discord.QueryValues{"limit": q.Limit}
		if q.Before != "" {
			query["before"] = q.Before
		}
		err = d.client.Do(rest.GetPublicArchivedThreads.Compile(query, c.ID), nil, &page, rest.WithCtx(ctx))
	} else {
		err = d.client.Do(rest.GetActiveGuildThreads.Compile(nil, c.GuildID), nil, &page, rest.WithCtx(ctx))
	}
	if err != nil {
		return out, cursorError(wrap("list chat threads", err))
	}
	if !q.Archived {
		slices.SortFunc(page.Threads, func(a, b historyChannel) int { return cmp(b.ID, a.ID) })
	}
	for _, t := range page.Threads {
		if t.ParentID != c.ID || threadType(t.Type) != surface.PublicThread {
			continue
		}
		if !q.Archived && before != 0 && t.ID >= before {
			continue
		}
		if len(out.Threads) == q.Limit {
			out.Next = out.Threads[len(out.Threads)-1].Conv.Thread
			break
		}
		out.Threads = append(out.Threads, surface.ChatThread{
			Conv: surface.Conversation{Channel: channel, Thread: t.ID.String()}, Name: t.Name, Type: surface.PublicThread, Archived: q.Archived,
		})
	}
	if q.Archived && page.More {
		if len(page.Threads) == 0 {
			return surface.ThreadPage{}, fmt.Errorf("discord: archive page has more but no cursor")
		}
		last := page.Threads[len(page.Threads)-1]
		if last.Metadata == nil || last.Metadata.Timestamp.IsZero() {
			return surface.ThreadPage{}, fmt.Errorf("discord: archive page has no timestamp")
		}
		out.Next = last.Metadata.Timestamp.Format(time.RFC3339Nano)
		if out.Next == q.Before {
			return surface.ThreadPage{}, fmt.Errorf("discord: archive cursor did not advance")
		}
	}
	return out, nil
}

type historyWireMessage struct {
	wireMessage
	Attachments []historyAttachment `json:"attachments"`
}

type historyAttachment struct {
	ID          snowflake.ID `json:"id"`
	Name        string       `json:"filename"`
	Size        int64        `json:"size"`
	ContentType string       `json:"content_type"`
	URL         string       `json:"url"`
}

func remoteFiles(files []historyAttachment) []surface.RemoteFile {
	var out []surface.RemoteFile
	for _, f := range files {
		out = append(out, surface.RemoteFile{ID: f.ID.String(), Name: f.Name, Size: f.Size, ContentType: f.ContentType, URL: f.URL})
	}
	return out
}

func (d *Driver) historyMessage(m historyWireMessage, conv surface.Conversation, typ surface.ThreadType) (surface.HistoryMessage, error) {
	// inbound supplies author and reply metadata. Reading history never calls
	// an agent; call detection and role availability do not gate these reads.
	in := d.inbound(m.wireMessage, conv, false)
	out := surface.HistoryMessage{Ref: in.Ref, Author: in.Author, Time: in.Time, Text: in.Text, ReplyTo: in.ReplyTo,
		Files: remoteFiles(m.Attachments), Own: m.Author.ID == d.botID, Type: typ}
	if m.MessageReference != nil && m.MessageReference.Type == 1 {
		for _, raw := range m.Snapshots {
			var s struct {
				Message struct {
					Content     string              `json:"content"`
					Attachments []historyAttachment `json:"attachments"`
				} `json:"message"`
			}
			if err := json.Unmarshal(raw, &s); err != nil {
				return surface.HistoryMessage{}, fmt.Errorf("discord: read forwarded message: %w", err)
			}
			out.Forwarded = append(out.Forwarded, surface.ForwardedMessage{Text: s.Message.Content, Files: remoteFiles(s.Message.Attachments)})
		}
	}
	return out, nil
}

// Application command responses and followups are user-visible posts too.
// Keep this separate from written: those posts may never be live calls.
func historyWritten(m wireMessage) bool {
	return !m.Author.System && (m.Type == typeDefault || m.Type == typeReply || m.Type == 20 || m.Type == 23)
}

// ReadMessages includes the bot's own messages and leaves out system
// messages. Pagination advances across raw messages, including notices.
func (d *Driver) ReadMessages(ctx context.Context, conv surface.Conversation, q surface.MessageQuery) (surface.MessagePage, error) {
	var out surface.MessagePage
	if err := readLimit(q.Limit); err != nil {
		return out, err
	}
	if q.Before != "" && q.After != "" {
		return out, fmt.Errorf("discord: before and after are mutually exclusive")
	}
	query := discord.QueryValues{"limit": q.Limit}
	for key, value := range map[string]string{"before": q.Before, "after": q.After} {
		if value != "" {
			if _, err := messageID(value); err != nil {
				return out, err
			}
			query[key] = value
		}
	}
	c, err := d.historyChannel(ctx, conv)
	if err != nil {
		return out, err
	}
	out.Type = threadType(c.Type)
	var page []historyWireMessage
	if err := d.client.Do(rest.GetMessages.Compile(query, c.ID), nil, &page, rest.WithCtx(ctx)); err != nil {
		return surface.MessagePage{}, cursorError(wrap("read chat messages", err))
	}
	if len(page) == 0 {
		// The list endpoint silently returns [] without Read Message History.
		// A successful single-message read proves the permission. If no message
		// can be named, refuse rather than mistake an inaccessible history for
		// an empty one. Unknown Message is not proof of permission either.
		var probe snowflake.ID
		if q.Before != "" {
			probe, _ = messageID(q.Before)
		}
		if probe == 0 && q.After != "" {
			probe, _ = messageID(q.After)
		}
		if probe == 0 {
			probe = c.LastMessageID
		}
		if probe == 0 {
			return surface.MessagePage{}, fmt.Errorf("%w: discord: cannot verify history access in an empty conversation", surface.ErrRefused)
		}
		if err := d.client.Do(rest.GetMessage.Compile(nil, c.ID, probe), nil, nil, rest.WithCtx(ctx)); err != nil {
			return surface.MessagePage{}, cursorError(wrap("verify history access", err))
		}
		return out, nil
	}
	slices.SortFunc(page, func(a, b historyWireMessage) int { return cmp(a.ID, b.ID) })
	if len(page) == q.Limit {
		out.Before = page[0].ID.String()
		out.After = page[len(page)-1].ID.String()
	}
	for _, m := range page {
		if m.ChannelID != c.ID {
			return surface.MessagePage{}, fmt.Errorf("discord: message is in another conversation")
		}
		if !historyWritten(m.wireMessage) {
			continue
		}
		v, err := d.historyMessage(m, conv, out.Type)
		if err != nil {
			return surface.MessagePage{}, err
		}
		out.Messages = append(out.Messages, v)
	}
	return out, nil
}

// ReadMessage returns ErrRefused for service notices, not a blank message.
func (d *Driver) ReadMessage(ctx context.Context, ref surface.MessageRef) (surface.HistoryMessage, error) {
	id, err := messageID(ref.ID)
	if err != nil {
		return surface.HistoryMessage{}, err
	}
	c, err := d.historyChannel(ctx, ref.Conv)
	if err != nil {
		return surface.HistoryMessage{}, err
	}
	var m historyWireMessage
	if err := d.client.Do(rest.GetMessage.Compile(nil, c.ID, id), nil, &m, rest.WithCtx(ctx)); err != nil {
		return surface.HistoryMessage{}, cursorError(wrap("read chat message", err))
	}
	if m.ID != id || m.ChannelID != c.ID {
		return surface.HistoryMessage{}, fmt.Errorf("discord: the service returned another message")
	}
	if !historyWritten(m.wireMessage) {
		return surface.HistoryMessage{}, fmt.Errorf("%w: discord: message is a service notice", surface.ErrRefused)
	}
	return d.historyMessage(m, ref.Conv, threadType(c.Type))
}
