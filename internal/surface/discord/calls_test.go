package discord

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// Ids of what the captures have none of.
const (
	ownRole   = "1000000000000000921" // managed for the bot
	otherRole = "1000000000000000922"
	botRole   = "1000000000000000923" // managed for another bot
	boostRole = "1000000000000000924" // managed, for no bot
	namedRole = "1000000000000000925" // names the bot, and is not managed
	another   = "1000000000000000931" // an account
	aPost     = "1000000000000000941"
)

// guildRoles are a server's roles, the bot's managed one among them.
func guildRoles(bot string) []any {
	return []any{
		map[string]any{"id": ownRole, "name": "hataori-test", "managed": true, "tags": map[string]any{"bot_id": bot}},
		map[string]any{"id": otherRole, "name": "hataori-test", "managed": false},
		map[string]any{"id": botRole, "managed": true, "tags": map[string]any{"bot_id": another}},
		map[string]any{"id": boostRole, "managed": true, "tags": map[string]any{"premium_subscriber": nil}},
		map[string]any{"id": namedRole, "managed": false, "tags": map[string]any{"bot_id": bot}},
	}
}

// calledBot is a driver that knows the captured server's channel and roles.
func calledBot(t *testing.T) (*Driver, *fakeREST, *sink) {
	r := newFakeREST(t)
	d, s := testDriver(t, botID(t), r, nil), newSink()
	p := payload(t, "person/gw-08-MESSAGE_CREATE.json")
	event(t, d, s, "GUILD_CREATE", map[string]any{"id": p["guild_id"], "roles": guildRoles(botID(t).String()),
		"channels": []any{map[string]any{"id": p["channel_id"], "guild_id": p["guild_id"], "type": 0, "position": 0, "name": "c", "flags": 0}},
		"threads":  []any{}})
	return d, r, s
}

// users is a message's mentions.
func users(ids ...string) []any {
	out := []any{}
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "username": "name-1"})
	}
	return out
}

type fields = map[string]any

// The captured message of a person, changed to what Discord's
// documentation says of each case: no capture of them exists yet. Every
// case is read as the gateway sends it and as a catch-up reads it.
func TestCalls(t *testing.T) {
	bot := botID(t).String()
	at, role := "<@"+bot+">", "<@&"+ownRole+">"
	// says is a message that the service says mentions the bot.
	says := func(text string) fields { return fields{"content": text, "mentions": users(bot)} }
	roles := func(text string, ids ...any) fields { return fields{"content": text, "mention_roles": ids} }
	// reply is a reply to a post of the account id.
	reply := func(id, text string, mentions ...string) fields {
		return fields{"type": 19, "content": text, "mentions": users(mentions...),
			"message_reference":  fields{"type": 0, "message_id": aPost, "channel_id": "1000000000000000005"},
			"referenced_message": fields{"id": aPost, "author": fields{"id": id}}}
	}
	with := func(f fields, more fields) fields {
		for k, v := range more {
			f[k] = v
		}
		return f
	}
	without := func(f fields, key string) fields { delete(f, key); return f }
	author := func(more fields) fields { return with(fields{"id": another, "username": "name-9"}, more) }

	for _, c := range []struct {
		name   string
		set    fields
		call   bool
		others bool
		own    []string // what OwnMentions stand for
	}{
		{"nobody", fields{}, false, false, nil},
		{"the account", says(at + " hello"), true, false, []string{at}},
		{"the account, as it was written before", says("<@!" + bot + "> hello"), true, false, []string{"<@!" + bot + ">"}},
		{"the account, twice", says(at + " hello " + at), true, false, []string{at, at}},
		{"the account and another", fields{"content": at + " <@" + another + ">", "mentions": users(bot, another)}, true, false, []string{at}},
		{"the managed role", roles(role+" hello", ownRole), true, false, []string{role}},
		{"the managed role and another", roles("<@&"+otherRole+"> "+role, otherRole, ownRole), true, false, []string{role}},
		{"a reply to its post", reply(bot, "hello", bot), true, false, nil},
		{"a reply without a ping", reply(bot, "hello"), true, false, nil},
		{"a reply to its answer to a command", with(reply(bot, "hello"), fields{"referenced_message": fields{"id": aPost, "webhook_id": "1000000000000000951", "author": fields{"id": bot, "bot": true}}}), true, false, nil},
		{"a reply to another that mentions it", reply(another, at+" see this", another, bot), true, false, []string{at}},
		{"a reply to its post that mentions another", reply(bot, "<@"+another+"> see this", another), true, false, nil},

		{"in a quote and outside", says("> " + at + "\n" + at + " yes"), true, false, []string{at}},
		{"after a quote with a fence", says("> ```\n" + at), true, false, []string{at}},
		{"after code and between", says("`a` " + at + " ```b``` " + at + " ``c`"), true, false, []string{at, at}},
		{"after a lone backtick", says("it`s " + at), true, false, []string{at}},
		{"after an escaped backtick", says("\\`" + at + "`"), true, false, []string{at}},
		{"after an escaped quote", says("\\> " + at), true, false, []string{at}},
		{"after an escaped backslash", says(`\\` + at), true, false, []string{at}},
		{"after a greater-than sign in a line", says("a > b " + at), true, false, []string{at}},
		{"in other letters", says("é日 " + at + " 日"), true, false, []string{at}},

		{"in a quote", says("> " + at), false, false, nil},
		{"in a quote without a space", says(">" + at), false, false, nil},
		{"in an indented quote", says("a\n \t> " + at), false, false, nil},
		{"in a quote after another line", says("hello\n> " + at + "\nhello"), false, false, nil},
		{"in a quote of many lines", says("hello\n>>> a\n\n" + at), false, false, nil},
		{"in a quote after a block of code", says("```a```> " + at), false, false, nil},
		{"in code", says("`" + at + "`"), false, false, nil},
		{"in code of two backticks", says("`` ` " + at + "``"), false, false, nil},
		{"in code over two lines", says("`a\n" + at + "`"), false, false, nil},
		{"in code that a part of a run opens", says("``" + at + "` a"), false, false, nil},
		{"in a block of code", says("hello ```" + at + "```"), false, false, nil},
		{"in a block of code in a language", says("```go\n" + at + "\n```"), false, false, nil},
		{"in a block that begins with a fence", says("```\n```\n" + at + "\n```"), false, false, nil},
		{"in a block in a language that begins with a fence", says("```go\n```\n" + at + "\n```"), false, false, nil},
		{"in a block left open", says("hello\n```\n" + at), false, false, nil},
		{"in a block left open after a closed one", says("```a``` ```" + at), false, false, nil},
		{"after six backticks", says("``````" + at), false, false, nil},
		{"escaped", says(`\` + at), false, false, nil},
		{"the role in a quote", roles("> "+role, ownRole), false, false, nil},
		{"its name", fields{"content": "hataori, @hataori: hello"}, false, false, nil},
		{"in the text alone", fields{"content": at + " " + role}, false, false, nil},
		{"everyone", fields{"content": "@everyone hello", "mention_everyone": true}, false, false, nil},
		{"those online", fields{"content": "@here hello", "mention_everyone": true}, false, false, nil},
		{"another role", roles("<@&"+otherRole+">", otherRole), false, false, nil},
		{"another bot's role", roles("<@&"+botRole+">", botRole), false, false, nil},
		{"a managed role of no bot", roles("<@&"+boostRole+">", boostRole), false, false, nil},
		{"a role that is not managed", roles("<@&"+namedRole+">", namedRole), false, false, nil},
		{"a role nobody told of", roles("<@&1000000000000000929>", "1000000000000000929"), false, false, nil},
		{"a bot", with(says(at), fields{"author": author(fields{"bot": true})}), false, false, []string{at}},
		{"a bot that replies", with(reply(bot, "hello"), fields{"author": author(fields{"bot": true})}), false, false, nil},
		{"a webhook", with(says(at), fields{"webhook_id": "1000000000000000951", "author": author(fields{"bot": true})}), false, false, []string{at}},
		{"a webhook that says it is no bot", with(says(at), fields{"webhook_id": "1000000000000000951"}), false, false, []string{at}},
		{"the service", with(says(at), fields{"author": author(fields{"system": true})}), false, false, nil},
		{"a notice", with(says(at), fields{"type": 6}), false, false, nil},
		{"a thread's starter", with(reply(bot, ""), fields{"type": 21}), false, false, nil},
		{"a forward", with(says(at), fields{"message_reference": fields{"type": 1, "message_id": aPost, "channel_id": "1000000000000000005"}}), false, false, []string{at}},
		{"what a forward holds", with(says(at), fields{"message_snapshots": []any{fields{"message": says(at)}}}), false, false, []string{at}},
		{"an edit", with(says(at), fields{"edited_timestamp": "2026-11-17T02:36:03.612000+00:00"}), false, false, []string{at}},
		{"a reply that was edited", with(reply(bot, "hello"), fields{"edited_timestamp": "2026-11-17T02:36:03.612000+00:00"}), false, false, nil},
		{"a reply to a post that is not told", without(reply(bot, "hello", bot), "referenced_message"), false, false, nil},
		{"a reply to a post that is gone", with(reply(bot, "hello", bot), fields{"referenced_message": nil}), false, false, nil},
		{"a reply to a post of nobody", with(reply(bot, "hello"), fields{"referenced_message": fields{"id": aPost}}), false, false, nil},
		{"a message that is no reply, with a post", with(reply(bot, "hello"), fields{"type": 0}), false, false, nil},

		{"another", fields{"content": "<@" + another + "> hello", "mentions": users(another)}, false, true, nil},
		{"a reply to another", reply(another, "hello", another), false, true, nil},
		{"a reply to another without a ping", reply(another, "hello"), false, true, nil},
		{"another by a bot", fields{"content": "<@" + another + ">", "mentions": users(another), "author": author(fields{"bot": true})}, false, true, nil},
		{"another, in the text alone", fields{"content": "<@" + another + "> hello"}, false, false, nil},
		{"another, in code", fields{"content": "`<@" + another + ">`", "mentions": users(another)}, false, false, nil},
		{"another and the bot in a quote", fields{"content": "<@" + another + ">\n> " + at, "mentions": users(another, bot)}, false, false, nil},
		{"a reply to another, and the bot in code", reply(another, "`"+at+"`", another), false, false, nil},
		{"another and the bot in code", fields{"content": "<@" + another + "> `" + at + "`", "mentions": users(another)}, false, false, nil},
		{"another, and the bot by the service alone", fields{"content": "<@" + another + ">", "mentions": users(another, bot)}, false, false, nil},
		{"another and a role", fields{"content": "<@" + another + "> <@&" + otherRole + ">", "mentions": users(another), "mention_roles": []any{otherRole}}, false, false, nil},
		{"a reply to another, and a role by the service alone", with(reply(another, "hello", another), fields{"mention_roles": []any{otherRole}}), false, false, nil},
		{"another and everyone", fields{"content": "<@" + another + "> @everyone", "mentions": users(another), "mention_everyone": true}, false, false, nil},
		{"a reply to another, and everyone by the service alone", with(reply(another, "hello", another), fields{"mention_everyone": true}), false, false, nil},
		{"another, in a reply to a post that is not told", without(reply(bot, "<@"+another+">", another), "referenced_message"), false, false, nil},
		{"another, in a reply to the bot", reply(bot, "<@"+another+"> yes", another, bot), true, false, nil},
		{"another, in a notice", fields{"type": 6, "content": "<@" + another + ">", "mentions": users(another)}, false, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, r, s := calledBot(t)
			p := person(t, "hello", 0)
			for k, v := range c.set {
				p[k] = v
			}
			event(t, d, s, "MESSAGE_CREATE", p)
			if len(s.messages) != 1 {
				t.Fatalf("%d messages delivered", len(s.messages))
			}
			r.serveJSON(t, "GET /channels/"+id(p["channel_id"])+"/messages", 200, []any{without(p, "guild_id")})
			caught, err := d.Since(context.Background(), s.messages[0].Ref.Conv, "")
			if err != nil || len(caught) != 1 {
				t.Fatalf("caught up %d, %v", len(caught), err)
			}
			for how, in := range map[string]surface.Inbound{"live": s.messages[0], "caught up": caught[0]} {
				var own []string
				for _, m := range in.OwnMentions {
					own = append(own, in.Text[m.Start:m.End])
				}
				if in.Call != c.call || in.OthersOnly != c.others || !reflect.DeepEqual(own, c.own) {
					t.Errorf("%s: call %v, to others only %v, own mentions %q; want %v, %v, %q\n%q",
						how, in.Call, in.OthersOnly, own, c.call, c.others, c.own, in.Text)
				}
			}
		})
	}
}

// A direct message is no call, whatever it mentions, live and at a catch-up.
func TestADirectMessageIsNoCall(t *testing.T) {
	d, r, s := calledBot(t)
	p := payload(t, "person/gw-07-MESSAGE_CREATE.json") // no guild_id
	p["content"], p["mentions"] = "<@"+botID(t).String()+">", users(botID(t).String())
	event(t, d, s, "MESSAGE_CREATE", p)
	r.serveJSON(t, "GET /channels/"+id(p["channel_id"])+"/messages", 200, []any{p})
	got, err := d.Since(context.Background(), s.messages[0].Ref.Conv, "")
	if err != nil || len(got) != 1 || !got[0].Direct || !s.messages[0].Direct {
		t.Fatalf("%+v, %v", got, err)
	}
	if got[0].Call || s.messages[0].Call {
		t.Error("a direct message is a call")
	}
}

// An edit of a message delivers nothing.
func TestAnEditDeliversNothing(t *testing.T) {
	d, _, s := calledBot(t)
	p := person(t, "<@"+botID(t).String()+">", 0)
	p["mentions"], p["edited_timestamp"] = users(botID(t).String()), "2026-11-17T02:36:03.612000+00:00"
	event(t, d, s, "MESSAGE_UPDATE", p)
	deliver(t, testDriver(t, 1, newFakeREST(t), nil), s, "bot/gw-14-MESSAGE_UPDATE.json")
	if len(s.messages) != 0 {
		t.Errorf("an edit delivered %+v", s.messages)
	}
}

// No captured message is a call, and none is to others only: none of a
// person mentions anybody or is a reply.
func TestNoCallInTheCaptures(t *testing.T) {
	files, err := filepath.Glob("testdata/v10/*/gw-*-MESSAGE_CREATE.json")
	if err != nil || len(files) < 10 {
		t.Fatalf("%d captures, %v", len(files), err)
	}
	for _, bot := range []snowflake.ID{botID(t), 1} {
		r, s := newFakeREST(t), newSink()
		d := testDriver(t, bot, r, nil)
		deliver(t, d, s, "bot/gw-05-THREAD_CREATE.json")
		for _, f := range files {
			deliver(t, d, s, strings.TrimPrefix(f, "testdata/v10/"))
		}
		for _, in := range s.messages {
			if in.Call || in.OthersOnly || len(in.OwnMentions) > 0 {
				t.Errorf("%+v", in)
			}
		}
	}
}

// What capture.py's calls run recorded: each of a person's messages, as the
// gateway sent it and as REST read it.
func TestCapturedCalls(t *testing.T) {
	if _, err := os.Stat("testdata/v10/calls"); errors.Is(err, os.ErrNotExist) {
		t.Skip("not captured yet (#869)")
	}
	bot := snowflake.MustParse(id(payload(t, "calls/rest-me.json")["id"]))
	guild := payload(t, "calls/gw-guild.json")
	for _, c := range []struct {
		name         string
		call, others bool
		restOnly     bool // the edit and the deletion that make the case come after the message
	}{
		{"mention", true, false, false},
		{"mention-role", true, false, false},
		{"reply", true, false, false},
		{"reply-silent", true, false, false},
		{"quoted-and-not", true, false, false},
		{"quoted", false, false, false},
		{"quoted-lines", false, false, false},
		{"in-code", false, false, false},
		{"in-block", false, false, false},
		{"in-open-block", false, false, false},
		{"everyone", false, false, false},
		{"here", false, false, false},
		{"other-role", false, false, false},
		{"webhook", false, false, false},
		{"forward", false, false, false},
		{"edited", false, false, true},
		{"reply-deleted", false, false, true},
		{"reply-other", false, true, false},
		{"other", false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, s := newFakeREST(t), newSink()
			d := testDriver(t, bot, r, nil)
			event(t, d, s, "GUILD_CREATE", guild)
			p := payload(t, "calls/rest-"+c.name+".json")
			r.serveJSON(t, "GET /channels/"+id(p["channel_id"])+"/messages", 200, []any{p})
			d.setDirect(snowflake.MustParse(id(p["channel_id"])), false)
			got, err := d.Since(context.Background(), surface.Conversation{Channel: id(p["channel_id"])}, "")
			if err != nil || len(got) != 1 {
				t.Fatalf("%+v, %v", got, err)
			}
			if !c.restOnly {
				deliver(t, d, s, "calls/gw-"+c.name+".json")
				got = append(got, s.messages...)
			}
			for _, in := range got {
				if in.Call != c.call || in.OthersOnly != c.others {
					t.Errorf("call %v, to others only %v: %+v", in.Call, in.OthersOnly, in)
				}
			}
		})
	}
	s := newSink()
	deliver(t, testDriver(t, bot, newFakeREST(t), nil), s, "calls/gw-edited-update.json")
	if len(s.messages) != 0 {
		t.Errorf("an edit delivered %+v", s.messages)
	}
	// A bot's message, read by the account it mentions.
	p := payload(t, "calls/rest-post-by-bot.json")
	d := testDriver(t, snowflake.MustParse(id(p["mentions"].([]any)[0].(map[string]any)["id"])), newFakeREST(t), nil)
	p["guild_id"] = guild["id"]
	event(t, d, s, "MESSAGE_CREATE", p)
	if len(s.messages) != 1 || s.messages[0].Call || len(s.messages[0].OwnMentions) != 1 {
		t.Errorf("%+v", s.messages)
	}
}

// readyGuilds are the servers that the captured READY lists.
func readyGuilds(t *testing.T) []discord.UnavailableGuild {
	t.Helper()
	var ready gateway.EventReady
	if err := json.Unmarshal(raw(t, payload(t, "bot/gw-01-READY.json")), &ready); err != nil {
		t.Fatal(err)
	}
	return ready.Guilds
}

// roleCall is a person's message that mentions role, in the captured channel.
func roleCall(t *testing.T, role string) map[string]any {
	p := person(t, "<@&"+role+"> hello", 0)
	p["mention_roles"] = []any{role}
	return p
}

// The role that is managed for the bot is learned from a server's Guild
// Create, and from its Guild Role Create or Update; a list that cannot be
// read takes nothing from the layout.
func TestTheManagedRoleIsLearned(t *testing.T) {
	bot := botID(t).String()
	role := guildRoles(bot)[0]
	p := roleCall(t, ownRole)
	for name, c := range map[string]struct {
		typ     string
		payload any
		call    bool
	}{
		"Guild Create":      {"GUILD_CREATE", fields{"id": p["guild_id"], "roles": guildRoles(bot)}, true},
		"Guild Role Create": {"GUILD_ROLE_CREATE", fields{"guild_id": p["guild_id"], "role": role}, true},
		"Guild Role Update": {"GUILD_ROLE_UPDATE", fields{"guild_id": p["guild_id"], "role": role}, true},
		"another's role":    {"GUILD_ROLE_CREATE", fields{"guild_id": p["guild_id"], "role": fields{"id": ownRole, "managed": true, "tags": fields{"bot_id": another}}}, false},
		"no role":           {"GUILD_ROLE_UPDATE", fields{"guild_id": p["guild_id"]}, false},
		"not a list":        {"GUILD_CREATE", fields{"id": p["guild_id"], "roles": "none"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			d, s := testDriver(t, botID(t), newFakeREST(t), nil), newSink()
			event(t, d, s, c.typ, c.payload)
			event(t, d, s, "MESSAGE_CREATE", p)
			if len(s.messages) != 1 || s.messages[0].Call != c.call {
				t.Errorf("%+v, want a call: %v", s.messages, c.call)
			}
			if d.mu.Lock(); c.typ == "GUILD_CREATE" && len(d.guilds) != 1 {
				t.Error("the server's layout is not kept")
			}
			d.mu.Unlock()
		})
	}
}

// A catch-up that meets a role before the server's roles came is told to
// come again, for a minute from when the session began. It does not wait for
// a message that is no call whatever the role is, nor for a role that a
// server's roles do not hold, nor for one known from an earlier session.
func TestACatchUpWaitsForTheRoles(t *testing.T) {
	p := roleCall(t, ownRole)
	ch := id(p["channel_id"])
	guild := func(roles []any) fields {
		return fields{"id": p["guild_id"], "roles": roles, "threads": []any{},
			"channels": []any{fields{"id": ch, "guild_id": p["guild_id"], "type": 0, "position": 0, "name": "c", "flags": 0}}}
	}
	for _, c := range []struct {
		name   string
		before func(d *Driver, s *sink)
		set    fields
		thread bool
		waits  bool
		call   bool
	}{
		{"nothing came yet", func(d *Driver, s *sink) { d.newSession(nil) }, nil, false, true, false},
		{"in a thread, nothing came yet", func(d *Driver, s *sink) { d.newSession(nil) }, nil, true, true, false},
		{"another server's came", func(d *Driver, s *sink) {
			d.newSession(nil)
			event(t, d, s, "GUILD_CREATE", fields{"id": "1000000000000000990", "channels": []any{}, "threads": []any{}, "roles": []any{}})
		}, nil, false, true, false},
		{"the roles came", func(d *Driver, s *sink) {
			d.newSession(nil)
			event(t, d, s, "GUILD_CREATE", guild(guildRoles(botID(t).String())))
		}, nil, false, false, true},
		{"in a thread, the roles came", func(d *Driver, s *sink) {
			d.newSession(nil)
			event(t, d, s, "GUILD_CREATE", guild(guildRoles(botID(t).String())))
		}, nil, true, false, true},
		{"the roles came without it", func(d *Driver, s *sink) {
			d.newSession(nil)
			event(t, d, s, "GUILD_CREATE", guild(nil))
		}, nil, false, false, false},
		{"in a new session", func(d *Driver, s *sink) {
			event(t, d, s, "GUILD_CREATE", guild(nil))
			d.newSession(readyGuilds(t))
		}, nil, false, true, false},
		{"known, in a new session", func(d *Driver, s *sink) {
			event(t, d, s, "GUILD_CREATE", guild(guildRoles(botID(t).String())))
			d.newSession(readyGuilds(t))
		}, nil, false, false, true},
		{"for too long", func(d *Driver, s *sink) {
			d.newSession(nil)
			d.session = time.Now().Add(-2 * rolesWithin)
		}, nil, false, false, false},
		{"an edited message", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"edited_timestamp": "2026-11-17T02:36:03.612000+00:00"}, false, false, false},
		{"a webhook's", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"webhook_id": "1000000000000000951"}, false, false, false},
		{"a quoted mention", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"content": "> <@&" + ownRole + ">"}, false, false, false},
		{"not told by the service", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"mention_roles": []any{}}, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, s := newFakeREST(t), newSink()
			d := testDriver(t, botID(t), r, nil)
			c.before(d, s)
			m := roleCall(t, ownRole)
			for k, v := range c.set {
				m[k] = v
			}
			conv := surface.Conversation{Channel: ch}
			d.setDirect(snowflake.MustParse(ch), false)
			at := ch
			if c.thread {
				conv.Thread = "1000000000000000961"
				at = conv.Thread
			}
			r.serveJSON(t, "GET /channels/"+at+"/messages", 200, []any{m})
			got, err := d.Since(context.Background(), conv, "")
			if c.waits {
				if !errors.Is(err, errRoles) || len(got) != 0 {
					t.Fatalf("%+v, %v; want to wait for the roles", got, err)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0].Call != c.call {
				t.Fatalf("%+v, %v; want a call: %v", got, err, c.call)
			}
		})
	}
}
