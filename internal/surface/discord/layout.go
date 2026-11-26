package discord

import (
	order "cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// The layout (surface.Layouter) is kept from the gateway alone, so keeping
// it costs no REST request: Guild Create carries a server's channels and
// its active threads, and Channel Create, Update and Delete, Thread Create,
// Update and Delete, and Thread List Sync keep them current. All of them
// come with the GUILDS intent. A channel the bot may not view comes
// obfuscated, with the CHANNEL_OBFUSCATED flag, and is left out; a
// category is shown when it holds a channel that is shown, as Discord's
// own client does. Discord sends only the threads the bot may view.

// Discord's channel types the layout tells apart.
const (
	typeCategory = 4
	typeVoice    = 2
	typeStage    = 13
)

// flagObfuscated is CHANNEL_OBFUSCATED: the bot may not view the channel,
// and only its id, type, position and parent_id are real.
const flagObfuscated = 1 << 17

// layoutChannel is the part of a channel or a thread the layout reads.
type layoutChannel struct {
	ID       snowflake.ID `json:"id"`
	GuildID  snowflake.ID `json:"guild_id"`
	Type     int          `json:"type"`
	Position int          `json:"position"`
	ParentID snowflake.ID `json:"parent_id"`
	Name     string       `json:"name"`
	Flags    int          `json:"flags"`
	Thread   *struct {
		Archived bool `json:"archived"`
	} `json:"thread_metadata"`
}

// guildLayout is what the driver keeps of one server.
type guildLayout struct {
	channels map[snowflake.ID]layoutChannel // channels and categories, obfuscated ones too
	threads  map[snowflake.ID]layoutChannel // active threads
	current  bool                           // from a Guild Create of this session
}

func newGuildLayout() *guildLayout {
	return &guildLayout{channels: map[snowflake.ID]layoutChannel{}, threads: map[snowflake.ID]layoutChannel{}}
}

// guild is the layout of server id, made when it is not kept yet. d.mu is
// held.
func (d *Driver) guild(id snowflake.ID) *guildLayout {
	g := d.guilds[id]
	if g == nil {
		g = newGuildLayout()
		d.guilds[id] = g
	}
	return g
}

// putThread keeps an active thread and forgets an archived one. d.mu is
// held.
func (g *guildLayout) putThread(c layoutChannel) {
	if c.Thread != nil && c.Thread.Archived {
		delete(g.threads, c.ID)
		return
	}
	g.threads[c.ID] = c
}

// layoutEvent changes the layout for one gateway event of type t, and
// reports whether t is one the layout reads. A server that is not available
// is forgotten until it is.
func (d *Driver) layoutEvent(t string, payload []byte) (bool, error) {
	switch t {
	case "GUILD_CREATE":
		var g struct {
			ID          snowflake.ID    `json:"id"`
			Unavailable bool            `json:"unavailable"`
			Channels    []layoutChannel `json:"channels"`
			Threads     []layoutChannel `json:"threads"`
		}
		if err := json.Unmarshal(payload, &g); err != nil {
			return true, err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if g.Unavailable {
			delete(d.guilds, g.ID)
			return true, nil
		}
		gl := newGuildLayout()
		gl.current = true
		for _, c := range g.Channels {
			gl.channels[c.ID] = c
		}
		for _, c := range g.Threads {
			gl.putThread(c)
		}
		d.guilds[g.ID] = gl
	case "GUILD_DELETE":
		var g struct {
			ID snowflake.ID `json:"id"`
		}
		if err := json.Unmarshal(payload, &g); err != nil {
			return true, err
		}
		d.mu.Lock()
		delete(d.guilds, g.ID)
		d.mu.Unlock()
	case "CHANNEL_CREATE", "CHANNEL_UPDATE", "CHANNEL_DELETE", "THREAD_CREATE", "THREAD_UPDATE", "THREAD_DELETE":
		var c layoutChannel
		if err := json.Unmarshal(payload, &c); err != nil {
			return true, err
		}
		if c.GuildID == 0 {
			return false, nil // a direct conversation
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		g := d.guild(c.GuildID)
		switch {
		case t == "THREAD_DELETE":
			delete(g.threads, c.ID)
		case isThread(c.Type):
			g.putThread(c)
		case t == "CHANNEL_DELETE":
			delete(g.channels, c.ID)
			for id, th := range g.threads {
				if th.ParentID == c.ID {
					delete(g.threads, id)
				}
			}
		default:
			g.channels[c.ID] = c
		}
	case "THREAD_LIST_SYNC":
		var s struct {
			GuildID    snowflake.ID    `json:"guild_id"`
			ChannelIDs *[]snowflake.ID `json:"channel_ids"`
			Threads    []layoutChannel `json:"threads"`
		}
		if err := json.Unmarshal(payload, &s); err != nil {
			return true, err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		g := d.guild(s.GuildID)
		// Without channel_ids the whole server's threads are synced; with
		// them, those channels' threads, also of a channel that has none.
		for id, th := range g.threads {
			if s.ChannelIDs == nil || slices.Contains(*s.ChannelIDs, th.ParentID) {
				delete(g.threads, id)
			}
		}
		for _, c := range s.Threads {
			g.putThread(c)
		}
	default:
		return false, nil
	}
	return true, nil
}

// newSession forgets the layout of every server that a new session's READY
// does not list: the bot may have left it while no session ran, and no
// Guild Delete comes for it then. The servers it lists stay until their
// Guild Create replaces them, so the layout does not empty in between.
func (d *Driver) newSession(guilds []discord.UnavailableGuild) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.session = time.Now()
	for id, l := range d.guilds {
		l.current = false
		if !slices.ContainsFunc(guilds, func(g discord.UnavailableGuild) bool { return g.ID == id }) {
			delete(d.guilds, id)
		}
	}
}

// layoutChanged tells sink that the layout changed, when it wants to know.
func layoutChanged(sink surface.Sink) {
	if w, ok := sink.(surface.LayoutWatcher); ok {
		w.LayoutChanged()
	}
}

// readAll is the bytes of a gateway event's payload.
func readAll(payload io.Reader) ([]byte, error) {
	b, err := io.ReadAll(payload)
	if err != nil {
		return nil, fmt.Errorf("discord: read the event: %w", err)
	}
	return b, nil
}

var _ surface.Layouter = (*Driver)(nil)

// Layout is the servers the bot is in, as their members see them in
// Discord's client: per server, first the channels in no category, then each
// category, ordered by position and then by id; in each, the text
// channels (text, announcement, forum, media) before the voice ones (voice,
// stage), each ordered by position and then by id; and under each channel
// its active threads, the newest first, as Discord lists active threads.
// Servers come in the order of their ids. A channel the bot may not view
// is left out, and so is a category that holds none it may.
func (d *Driver) Layout(context.Context) (surface.Layout, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out surface.Layout
	guilds := make([]snowflake.ID, 0, len(d.guilds))
	for id := range d.guilds {
		guilds = append(guilds, id)
	}
	slices.Sort(guilds)
	for _, gid := range guilds {
		g := d.guilds[gid]
		threads := map[snowflake.ID][]layoutChannel{} // by channel
		for _, th := range g.threads {
			threads[th.ParentID] = append(threads[th.ParentID], th)
		}
		var categories []layoutChannel
		children := map[snowflake.ID][]layoutChannel{} // by category; 0 for none
		for _, c := range g.channels {
			switch {
			case c.Type == typeCategory:
				categories = append(categories, c)
			case c.Flags&flagObfuscated == 0 && !isThread(c.Type):
				children[c.ParentID] = append(children[c.ParentID], c)
			}
		}
		group := func(id snowflake.ID, name string) {
			cs := children[id]
			if len(cs) == 0 {
				return
			}
			slices.SortFunc(cs, func(x, y layoutChannel) int {
				return order.Or(order.Compare(voice(x.Type), voice(y.Type)), order.Compare(x.Position, y.Position), order.Compare(x.ID, y.ID))
			})
			lg := surface.LayoutGroup{Name: name}
			if id != 0 {
				lg.ID = id.String()
			}
			for _, c := range cs {
				lc := surface.LayoutChannel{ID: c.ID.String(), Name: "#" + c.Name, URL: channelURL(gid, c.ID)}
				ts := threads[c.ID]
				slices.SortFunc(ts, func(x, y layoutChannel) int { return order.Compare(y.ID, x.ID) })
				for _, th := range ts {
					lc.Threads = append(lc.Threads, surface.LayoutThread{ID: th.ID.String(), Name: th.Name, URL: channelURL(gid, th.ID)})
				}
				lg.Channels = append(lg.Channels, lc)
			}
			out.Groups = append(out.Groups, lg)
		}
		group(0, "")
		slices.SortFunc(categories, func(x, y layoutChannel) int {
			return order.Or(order.Compare(x.Position, y.Position), order.Compare(x.ID, y.ID))
		})
		for _, c := range categories {
			name := c.Name
			if c.Flags&flagObfuscated != 0 {
				name = "" // its name is not real; it holds a channel the bot may view
			}
			group(c.ID, name)
		}
	}
	return out, nil
}

// voice is 1 for a voice or stage channel, which Discord's client lists
// after the text channels of a category, else 0.
func voice(t int) int {
	if t == typeVoice || t == typeStage {
		return 1
	}
	return 0
}

// channelURL opens a channel or a thread in Discord's client.
func channelURL(guild, channel snowflake.ID) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s", guild, channel)
}
