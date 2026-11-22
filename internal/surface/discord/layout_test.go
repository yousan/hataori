package discord

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

// event feeds the driver one gateway event of type t with payload d.
func event(t *testing.T, d *Driver, s *sink, typ string, payload any) {
	t.Helper()
	if err := d.dispatch(context.Background(), typ, bytes.NewReader(raw(t, payload)), s); err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
}

// The layout is kept from the gateway alone, without one REST request: the
// channels in no category first, then the categories by position; in
// each, text channels before voice ones, by position; a channel the bot
// may not view left out, and a category that holds none it may; each
// channel's active threads, the newest first, following a rename, an
// archive, a delete and a thread list sync. Each change is told.
//
// The channel, the threads, the rename and the archive are captures. Guild
// Create, Channel Update and Delete, Thread Delete, Thread List Sync and
// the obfuscated channels have no capture yet (#613): their payloads are
// shaped as Discord's documentation gives them, around the captured
// objects.
func TestLayoutFromTheGateway(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, 1, r, nil)
	s := newSink()
	channel := payload(t, "third/rest-get-channel.json") // a text channel in no category
	first := payload(t, "bot/gw-05-THREAD_CREATE.json")
	second := payload(t, "second/rest-thread-without-message.json") // a newer thread in the same channel
	guild, ch := id(channel["guild_id"]), id(channel["id"])
	text := func(id, parent string, pos int, name string) map[string]any {
		return map[string]any{"id": id, "guild_id": guild, "type": 0, "position": pos, "parent_id": parent, "name": name, "flags": 0}
	}
	voice := text("1000000000000000907", "1000000000000000901", 0, "talk")
	voice["type"] = 2
	hidden := text("1000000000000000908", "1000000000000000901", 3, "___hidden___")
	hidden["flags"] = 1 << 17
	hiddenCategory := map[string]any{"id": "1000000000000000909", "guild_id": guild, "type": 4, "position": 2, "name": "___hidden___", "flags": 1 << 17}
	hiddenChild := text("1000000000000000910", "1000000000000000909", 0, "___hidden___")
	hiddenChild["flags"] = 1 << 17
	other := map[string]any{"id": "1000000000000000911", "guild_id": guild, "type": 11, "parent_id": "1000000000000000905",
		"name": "release notes", "flags": 0, "thread_metadata": map[string]any{"archived": false}} // in #deploys
	event(t, d, s, "GUILD_CREATE", map[string]any{
		"id": guild,
		"channels": []any{
			channel,
			map[string]any{"id": "1000000000000000901", "guild_id": guild, "type": 4, "position": 1, "name": "Work", "flags": 0},
			map[string]any{"id": "1000000000000000902", "guild_id": guild, "type": 4, "position": 0, "name": "Info", "flags": 0},
			text("1000000000000000905", "1000000000000000901", 2, "deploys"),
			text("1000000000000000904", "1000000000000000901", 1, "builds"),
			voice, hidden, hiddenCategory, hiddenChild,
			text("1000000000000000906", "1000000000000000902", 0, "rules"),
		},
		"threads": []any{first, second, other},
	})
	url := func(c string) string { return "https://discord.com/channels/" + guild + "/" + c }
	thread := func(p map[string]any) surface.LayoutThread {
		return surface.LayoutThread{ID: id(p["id"]), Name: p["name"].(string), URL: url(id(p["id"]))}
	}
	lobby := surface.LayoutChannel{ID: ch, Name: "#" + channel["name"].(string), URL: url(ch), Threads: []surface.LayoutThread{thread(second), thread(first)}}
	work := surface.LayoutGroup{ID: "1000000000000000901", Name: "Work", Channels: []surface.LayoutChannel{
		{ID: "1000000000000000904", Name: "#builds", URL: url("1000000000000000904")},
		{ID: "1000000000000000905", Name: "#deploys", URL: url("1000000000000000905"), Threads: []surface.LayoutThread{thread(other)}},
		{ID: "1000000000000000907", Name: "#talk", URL: url("1000000000000000907")}, // voice after text, whatever its position
	}}
	info := surface.LayoutGroup{ID: "1000000000000000902", Name: "Info", Channels: []surface.LayoutChannel{
		{ID: "1000000000000000906", Name: "#rules", URL: url("1000000000000000906")},
	}}
	check := func(what string, told int, want ...surface.LayoutGroup) {
		t.Helper()
		got, err := d.Layout(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Groups, want) {
			t.Errorf("%s: the layout is\n%+v\nwant\n%+v", what, got.Groups, want)
		}
		if s.layouts != told {
			t.Errorf("%s: told %d changes, want %d", what, s.layouts, told)
		}
	}
	check("after Guild Create", 1, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, info, work)

	deliver(t, d, s, "bot/gw-17-THREAD_UPDATE.json") // the first thread renamed
	renamed := payload(t, "bot/gw-17-THREAD_UPDATE.json")
	lobby.Threads = []surface.LayoutThread{thread(second), thread(renamed)}
	check("after a rename", 2, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, info, work)

	event(t, d, s, "THREAD_UPDATE", payload(t, "second/rest-set-thread-archived.json")) // the second thread archived
	lobby.Threads = []surface.LayoutThread{thread(renamed)}
	check("after an archive", 3, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, info, work)

	event(t, d, s, "THREAD_UPDATE", second) // unarchived: Discord sends the whole thread
	lobby.Threads = []surface.LayoutThread{thread(second), thread(renamed)}
	check("after an unarchive", 4, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, info, work)

	event(t, d, s, "THREAD_DELETE", map[string]any{"id": id(second["id"]), "guild_id": guild, "parent_id": ch, "type": 11})
	lobby.Threads = []surface.LayoutThread{thread(renamed)}
	check("after a delete", 5, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, info, work)

	// A sync of one channel leaves the threads of the others.
	event(t, d, s, "THREAD_LIST_SYNC", map[string]any{"guild_id": guild, "channel_ids": []string{ch}, "threads": []any{second}})
	lobby.Threads = []surface.LayoutThread{thread(second)}
	check("after a thread list sync", 6, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, info, work)

	moved := text("1000000000000000906", "1000000000000000901", 0, "rules-v2") // renamed and moved into Work, first
	event(t, d, s, "CHANNEL_UPDATE", moved)
	work.Channels = append([]surface.LayoutChannel{{ID: "1000000000000000906", Name: "#rules-v2", URL: url("1000000000000000906")}}, work.Channels...)
	check("after a channel moved", 7, surface.LayoutGroup{Channels: []surface.LayoutChannel{lobby}}, work) // Info holds none now

	hiddenBuilds := text("1000000000000000904", "1000000000000000901", 1, "___hidden___")
	hiddenBuilds["flags"] = 1 << 17
	event(t, d, s, "CHANNEL_UPDATE", hiddenBuilds) // the bot may no longer view #builds
	event(t, d, s, "CHANNEL_DELETE", channel)
	work.Channels = []surface.LayoutChannel{work.Channels[0], work.Channels[2], work.Channels[3]}
	check("after a channel hidden and one deleted", 9, work)

	event(t, d, s, "GUILD_DELETE", map[string]any{"id": guild, "unavailable": true})
	check("after the server became unavailable", 10)

	if len(r.calls) != 0 {
		t.Errorf("keeping the layout made REST requests: %v", r.calls)
	}
}
