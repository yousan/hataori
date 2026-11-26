package discord

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
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
	const six = "- > a\n+ > a\n* > a\n1. > a\n# > a\n>a\n"
	const seven = six + " > a\n"
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
		{"after a quote with a fence, after a block", says("```a```\n> ```\n" + at), true, false, []string{at}},
		{"after a quote with a fence", says("> ```\n" + at), true, false, []string{at}},
		{"after code and between", says("`a` " + at + " ```b``` " + at + " ``c`"), true, false, []string{at, at}},
		{"after a lone backtick", says("it`s " + at), true, false, []string{at}},
		{"after an escaped backtick", says("\\`" + at + "`"), true, false, []string{at}},
		{"after an escaped quote", says("\\> " + at), true, false, []string{at}},
		{"after an escaped backslash", says(`\\` + at), true, false, []string{at}},
		{"after a greater-than sign in a line", says("a > b " + at), true, false, []string{at}},
		{"in other letters", says("é日 " + at + " 日"), true, false, []string{at}},
		{"after an arrow", says("-> " + at), true, false, []string{at}},
		{"after a number and a greater-than sign", says("1 > " + at), true, false, []string{at}},
		{"after a line that may be a quote", says(">a `b`\n" + at), true, false, []string{at}},
		{"after many lines that may be quotes", says(strings.Repeat(">\n", 500) + at), true, false, []string{at}},
		{"after a block quoted by hand", says(">```\npanic: x\n>```\n" + at), true, false, []string{at}},
		{"after a block in lists", says("- > ```\nx\n- > ```\n" + at), true, false, []string{at}},
		{"after a block in a list of numbers", says("9. > ```\nx\n10. > ```\n" + at), true, false, []string{at}},
		{"after code in lines that may be quotes", says(">a ``` b\n>c ``` d\n" + at), true, false, []string{at}},
		{"after a list with a lone backtick", says("- it`s fine\n" + at), true, false, []string{at}},
		{"after a list with code", says("- `a`\n- `b` " + at), true, false, []string{at}},
		{"after a list with a block", says("- ```\n  x\n  ```\n" + at), true, false, []string{at}},
		{"after a block in an item, between lines indented as the item is", says(" - ```\n a\n ``` " + at + " ```b```"), true, false, []string{at}},
		{"after a block in a list, of a line like a heading", says("- Run:\n  ```sh\n  # build\n  make\n  ```\n" + at + " check"), true, false, []string{at}},
		{"after a block in a list, with an empty line", says("- Run:\n  ```sh\n  make\n\n  make test\n  ```\n" + at + " check"), true, false, []string{at}},
		{"after a block that goes on after its item", says("1. Run: ```sh\nmake\n```\n" + at + " check"), true, false, []string{at}},
		{"after a block that goes on after its heading", says("# Log ```\nx\n```\n" + at + " check"), true, false, []string{at}},
		{"in an item after two empty lines, in what is no block either way", says("- a\n\n  type ``` to open\n\n  ```\n  " + at + " help\n  ```"), true, false, []string{at}},
		{"after a block over two lines of an item, after an empty line before it", says("hi\n\n- see ``` a\n  b ``` " + at + " ```c```"), true, false, []string{at}},
		{"after a small line and fences on either side", says("-# small\n  type ``` a\n``` " + at + " ```b```"), true, false, []string{at}},
		{"after lists in lists, between code", says("- a\n  - b\n    - c\n`x` " + at + " `y`"), true, false, []string{at}},
		{"after a list of many numbers with lone backticks", says("1. a`\n2. a`\n3. a`\n4. a`\n5. a`\n6. a`\n7. a`\n8. a`\n" + at), true, false, []string{at}},
		{"after a list of many indentations with lone backticks", says("       - a`\n      - a`\n     - a`\n    - a`\n   - a`\n  - a`\n - a`\n- a`\n" + at), true, false, []string{at}},
		{"between blocks, one with a line like a quote", says("```\n> build\n```\n" + at + " see\n```\nb\n```"), true, false, []string{at}},
		{"between blocks, one that opens in a line", says("try ```js\na()\n```\n" + at + " see\n```\nb\n```"), true, false, []string{at}},
		{"between a backtick and two", says("` " + at + " ``"), true, false, []string{at}},
		{"after code over two lines", says("`a\nb` " + at), true, false, []string{at}},
		{"before seven forms that may be quotes", says(at + " see\n <a\n >\n  >\n   >\n    >\n     >\n      >\n       >"), true, false, []string{at}},
		{"before an end of a line of its own and seven such forms", says(at + "\ra\n" + seven), true, false, []string{at}},
		{"before items with lone backticks and seven such forms", says(at + " see\n- don`t\n- don`t\n" + seven), true, false, []string{at}},
		{"before lone backticks and seven such forms", says(at + " don`t\ndon`t\n" + seven), true, false, []string{at}},
		{"before an item that may end at an empty line and seven such forms", says(at + " see\n- a\n\n  b\n" + seven), true, false, []string{at}},
		{"after a block on an item's line", says("1. ```x```\n" + at), true, false, []string{at}},
		{"after a list with code over two lines", says("- `a\n  b`\n" + at), true, false, []string{at}},
		{"after a heading with code", says("# `a`\n" + at + " `b`"), true, false, []string{at}},
		{"between code and a lone backtick, before a quote", says("`a` " + at + " ` b\n> c"), true, false, []string{at}},
		{"between code and a lone backtick, after a quote", says("> c\n`a` " + at + " ` b"), true, false, []string{at}},
		{"after a lone backtick and a quote with one", says("**don`t**\n> it`s\n" + at), true, false, []string{at}},
		{"after six forms that may be quotes", says(six + at), true, false, []string{at}},
		{"in a list, after a greater-than sign", says("- >50% of them fail " + at), true, false, []string{at}},
		{"in a list of numbers, after a greater-than sign", says("1. >=3 " + at), true, false, []string{at}},
		{"in a list of stars, after a greater-than sign", says("* >50% " + at), true, false, []string{at}},
		{"in a list of pluses, after a greater-than sign", says("+ >50% " + at), true, false, []string{at}},
		{"in a list of numbers in brackets, after a greater-than sign", says("1) >50% " + at), true, false, []string{at}},
		{"in a heading, after a greater-than sign", says("# >3 " + at), true, false, []string{at}},
		{"after four hashes and a greater-than sign", says("#### > " + at), true, false, []string{at}},
		{"after a line that may be a quote, with a fence", says(">```\n" + at), false, false, nil},
		{"after the end of a line of two characters", says("> a\r\n" + at), true, false, []string{at}},

		{"in a quote", says("> " + at), false, false, nil},
		{"in a quote without a space", says(">" + at), false, false, nil},
		{"in an indented quote", says("a\n \t> " + at), false, false, nil},
		{"in a quote after another line", says("hello\n> " + at + "\nhello"), false, false, nil},
		{"in a quote of many lines", says("hello\n>>> a\n\n" + at), false, false, nil},
		{"after seven forms that may be quotes", says(" > a\n" + six + at), false, false, nil},
		{"between seven forms that may be quotes", says("\t> a\n" + at + "\n" + six), false, false, nil},
		{"after an end of a line of its own, before seven such forms", says("a\r> " + at + "\n" + seven), false, false, nil},
		{"after a heading with a fence, before seven such forms", says("# ```x\n``` " + at + " ``\n" + seven), false, false, nil},
		{"after a lone backtick, before seven such forms", says("don`t ping\n`" + at + "`\n" + seven), false, false, nil},
		{"after an item that may end at an empty line, before seven such forms", says("- Run: ```\n\n  x\n  ```\n" + at + " ```b```\n" + seven), false, false, nil},
		{"in a quote after a byte order mark", says("\ufeff> " + at), false, false, nil},
		{"in a quote of many lines after a backslash", says("a\\\n>>> b\n" + at), false, false, nil},
		{"in a quote after a list with a lone backtick", says("- don`t\n> you said `x` " + at), false, false, nil},
		{"in a quote after a heading with a lone backtick", says("# don`t\n> you said `x` " + at), false, false, nil},
		{"in a quote of many lines after a heading with a lone backtick", says("# don`t\n>>> you said `x` " + at), false, false, nil},
		{"in a quote after a small line with a lone backtick", says("-# don`t\n> you said `x` " + at), false, false, nil},
		{"in a quote after bold text with a lone backtick", says("**don`t**\n> you said `x` " + at), false, false, nil},
		{"in a quote of many lines after bold text with a lone backtick", says("**don`t**\n>>> you said `x` " + at), false, false, nil},
		{"in a quote in a list with a lone backtick", says("- don`t\n  > you said `x` " + at), false, false, nil},
		{"in an indented quote without a space", says("  >" + at), false, false, nil},
		{"in a quote after a wide space", says("\u3000> " + at), false, false, nil},
		{"in a quote after a vertical tab", says("\v> " + at), false, false, nil},
		{"in a quote after a form feed", says("\f> " + at), false, false, nil},
		{"in a quote after a next line", says("\u0085> " + at), false, false, nil},
		{"in a quote before a wide space", says(">\u3000" + at), false, false, nil},
		{"in a quote after a block of code", says("```a```> " + at), false, false, nil},
		{"in a quote in a list", says("- > " + at), false, false, nil},
		{"in a quote in a list of stars", says("* > " + at), false, false, nil},
		{"in a quote in a list of numbers", says("a\n 10. > " + at), false, false, nil},
		{"in a quote in a list of pluses", says("+ > " + at), false, false, nil},
		{"in a quote in a list of numbers in brackets", says("1) > " + at), false, false, nil},
		{"in a quote in a list, after a wide space", says("-\u3000> " + at), false, false, nil},
		{"in a quote in a list, before a tab", says("- >\t" + at), false, false, nil},
		{"in a quote in a list, before a return", says("- >\r" + at), false, false, nil},
		{"in a quote in a list in a list", says("- - > " + at), false, false, nil},
		{"in a quote of many lines in a list", says("- >>> a\n" + at), false, false, nil},
		{"in a quote in a heading", says("## > " + at), false, false, nil},
		{"in a quote in a small line", says("-# > " + at), false, false, nil},
		{"in a quote after another end of a line", says("a\r> " + at), false, false, nil},
		{"in a quote that may go on after such an end", says("> a\r" + at), false, false, nil},
		{"in code", says("`" + at + "`"), false, false, nil},
		{"in code of two backticks", says("`` ` " + at + "``"), false, false, nil},
		{"in the second of two codes", says("`a` `" + at + "`"), false, false, nil},
		{"in code after a lone backtick", says("` ``" + at + "``"), false, false, nil},
		{"in the second code over a lone backtick", says("``a`` ` ``" + at + "``"), false, false, nil},
		{"in code after an item with a lone backtick", says("- don`t ping\n- `" + at + "` is its name"), false, false, nil},
		{"in code after a list with a lone backtick", says("- don`t ping\n`" + at + "` is its name"), false, false, nil},
		{"in code after a heading with a lone backtick", says("# Don`t ping\n`" + at + "`"), false, false, nil},
		{"in code after a heading in a list with a lone backtick", says("- # Don`t ping\n  `" + at + "`"), false, false, nil},
		{"in code after an indented list with a lone backtick", says(" - don`t ping\n`" + at + "` is its name"), false, false, nil},
		{"in code after a list with a list in it, with a lone backtick", says("- don`t ping\n  - `" + at + "` is its name"), false, false, nil},
		{"in code after a heading with a lone backtick and a list with one", says("# don`t\n- don`t\n" + at + "`"), false, false, nil},
		{"in code after a list with a lone backtick and a heading with one", says("- don`t\n# don`t\n" + at + "`"), false, false, nil},
		{"in code that a list opens, over its end", says("- `a\n" + at + "`"), false, false, nil},
		{"in code after a list right after a block, with a lone backtick", says("```a```- don`t ping\n`" + at + "` is its name"), false, false, nil},
		{"in code after a heading right after a block, with a lone backtick", says("```a```# don`t ping\n`" + at + "` is its name"), false, false, nil},
		{"in code after a list with a lone backtick and one right after a block", says("- don`t\n```a```- don`t\n" + at + "`"), false, false, nil},
		{"in code after a heading with a lone backtick and one right after a block", says("# don`t\n```a```# don`t\n" + at + "`"), false, false, nil},
		{"in code after a heading with a block, a list right after it and a lone backtick", says("# ```a```- don`t ping\n  `" + at + "` is its name"), false, false, nil},
		{"in a block in a list, of a line like a heading, after a fence in an item", says("- it opens with ```\n- then:\n  ```sh\n  # ask " + at + "\n  ```"), false, false, nil},
		{"in a block in a list, of a line like an item, after a fence in an item", says("- it opens with ```\n- then: ```yaml\n  - " + at + "\n  ```"), false, false, nil},
		{"in code over a list in a list, after a lone backtick", says("- don`t ping\n- `x\n  - " + at + "`"), false, false, nil},
		{"in a block after a fence in an item, and one in what may be an item", says("- it opens with ```\n1) ```\n" + at + "\n```"), false, false, nil},
		{"in code after an item with a lone backtick, in what may be an item", says("- don`t\n1) `a\n" + at + "`"), false, false, nil},
		{"in code after an item with a lone backtick after an empty line", says("1. first\n\n   don`t ping\n2. `" + at + "` is its name"), false, false, nil},
		{"in code after an item that begins as a heading, with a lone backtick", says("- # a\n  don`t\n- `" + at + "`"), false, false, nil},
		{"in code after an item with a heading and a lone backtick", says("- a\n  ## b\n  don`t\n- `" + at + "`"), false, false, nil},
		{"in code after an item that a line without indentation goes on", says("- a\ndon`t\n- `" + at + "`"), false, false, nil},
		{"in code after text with a lone backtick", says("Don`t ping it.\n\n`" + at + "` is its name"), false, false, nil},
		{"in code after text with a lone backtick, in a list", says("don`t ping\n- `" + at + "` is its name"), false, false, nil},
		{"in code after text with a lone backtick, in a block", says("don`t do it\n```\nuse `" + at + "` here\n```"), false, false, nil},
		{"in code after a quote after a lone backtick", says("**don`t**\n> a\n`" + at + "` is its name"), false, false, nil},
		{"in code between quotes after a lone backtick", says("**don`t**\n> a\n`" + at + "` is its name\n> b"), false, false, nil},
		{"in a block after a heading with a fence", says("# Use ``` for code\n```\n" + at + " help\n```"), false, false, nil},
		{"in a block after a list with a fence", says("- type ``` to open a block\n\n```\n" + at + " do x\n```"), false, false, nil},
		{"in a block after a list right after a block, with a fence", says("```a```- type ``` to open a block\n```\n" + at + " help\n```"), false, false, nil},
		{"in a block after a heading right after a block, with a fence", says("```a```# Use ``` for code\n```\n" + at + " help\n```"), false, false, nil},
		{"in an indented block after a heading with a fence", says("# Use ``` for code\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block after a heading with a fence, in an item", says("- # Use ``` for code\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block in an item after one with a fence", says(" - type ``` to open\n - then:\n   ```\n   " + at + " help\n   ```"), false, false, nil},
		{"in a block in an item after one with a fence after an empty line", says("1. first\n\n   type ``` to open\n2. then:\n   ```\n   " + at + " help\n   ```"), false, false, nil},
		{"in a block in an item after one with a heading and a fence", says("- a\n  # b\n  type ``` to open\n- then:\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block in an item after one that begins as a heading, with a fence", says("- # a\n  type ``` to open\n- then:\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block in a list in a list, after an item with a fence", says("- a\n  - type ``` to open\n  - then:\n    ```\n    " + at + " help\n    ```"), false, false, nil},
		{"in a block in a list, after a list in it with a fence that a line of the outer one ends", says("1. Ask for help:\n   - type ``` to open a block\n   like this:\n   ```\n   " + at + " help\n   ```"), false, false, nil},
		{"in a block after a list with a fence after an empty line", says("1. first\n\n   type ``` to open\n```\n" + at + " help\n```"), false, false, nil},
		{"in a block in an item after an empty line, up to the end", says("- type ``` to open\n\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block in an item after a line of white space, up to the end", says("- type ``` to open\n  \n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block after a list with a fence, with another end of a line", says("- type ``` to open\r```\r" + at + " help\r```"), false, false, nil},
		{"in a block in a list, after a list in it with a fence that a heading ends", says("- a\n  - type ``` to open\n  # Then\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block after a fence in an item, after a list in it that a heading ends", says("- a\n  1. b\n  # c\n  type ``` to open\n- then:\n  ```\n  " + at + " help\n  ```"), false, false, nil},
		{"in a block after an item with a fence and what may be an item, indented by a tab", says("- use ``` b\n\t- go ```sh\n" + at + " help\n```"), false, false, nil},
		{"in a block after an item with one, with an empty line", says("- Run:\n  ```sh\n  make\n\n  make test\n  ```\n" + at + " see\n```\nlog\n```"), false, false, nil},
		{"in a block after an item with one, indented by a tab", says("- Run: ```sh\n\tmake\n\t```\n" + at + " see\n```\nlog\n```"), false, false, nil},
		{"in a block after an item with one, indented by a wide space", says("- Run: ```sh\n\u3000make\n\u3000```\n" + at + " see\n```\nlog\n```"), false, false, nil},
		{"in a block in an item after one with a block and a fence", says("  - ```a```- see ``` x\n  - ```\n  " + at + "\n  ```"), false, false, nil},
		{"in code that the rest of a fence opens, after a heading with one", says("# ```x\n``` " + at + " ``"), false, false, nil},
		{"in code over two lines", says("`a\n" + at + "`"), false, false, nil},
		{"in code that a part of a run opens", says("``" + at + "` a"), false, false, nil},
		{"in a block of code", says("hello ```" + at + "```"), false, false, nil},
		{"in a block of code in a language", says("```go\n" + at + "\n```"), false, false, nil},
		{"in a block that begins with a fence", says("```\n```\n" + at + "\n```"), false, false, nil},
		{"in a block in a language that begins with a fence", says("```go\n```\n" + at + "\n```"), false, false, nil},
		{"in a block in a language of other letters", says("```C++\n```\n" + at + "\n```"), false, false, nil},
		{"in a block in a language with a hash", says("```c#\n```\n" + at + "\n```"), false, false, nil},
		{"in a block in a language, with other ends of lines", says("```go\r\n```\r\n" + at + "\r\n```"), false, false, nil},
		{"in a block that a line opens which may be a quote", says(">```\n" + at + "\n```"), false, false, nil},
		{"in a block in a language that such a line opens", says("\t> ```go\n" + at + "\n```"), false, false, nil},
		{"in a block that such a line opens after text", says(">_<```\n" + at + "```"), false, false, nil},
		{"in a block that such a line opens in a list", says("- > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that such a line opens after a block", says("```a```> ```\n" + at + "\n```"), false, false, nil},
		{"in code that such a line opens", says(">`a\n" + at + "`"), false, false, nil},
		{"in a block that an indented line of a list opens", says("- a\n  > ```\n  " + at + "\n  ```"), false, false, nil},
		{"in a block that such a line opens in a heading", says("# > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that one list opens and not another", says("- > ```\n+ > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that one list of numbers opens and not another", says("1. > ```\n2) > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that one white space opens and not another", says(">\t```\n>```\n" + at + "\n```"), false, false, nil},
		{"in a block that a small line opens and not a heading", says("# > ```\n-# > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that a list opens and not an indented line", says("  > ```\n- > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that a heading opens and not an indented line", says("  > ```\n# > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that an indented line opens and not one without a space", says(">```\n  > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that a line without a space opens and not one after a block", says("```a```> ```\n>```\n" + at + "\n```"), false, false, nil},
		{"in a block that an indented line opens and not one after a block", says("```a```> ```\n  > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that a line without a space opens and not the same after a block", says("```a```>```\n>```\n" + at + "\n# ```"), false, false, nil},
		{"in a block that a line with another space opens", says(">\t```\n" + at + "\n```"), false, false, nil},
		{"in a block that a list opens and not a heading", says("# > ```\n- > ```\n" + at + "\n```"), false, false, nil},
		{"in a block that a heading opens and not a list", says("- > ```\n# > ```\n" + at + "\n```"), false, false, nil},
		{"in a block left open", says("hello\n```\n" + at), false, false, nil},
		{"after a block of nothing", says("```\n\n```" + at), false, false, nil},
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
		{"another, in a quote", fields{"content": "> <@" + another + "> a\nb", "mentions": users(another)}, false, false, nil},
		{"another, in a quote of many lines", fields{"content": "a\n>>> <@" + another + ">", "mentions": users(another)}, false, false, nil},
		{"another, in a quote and outside", fields{"content": "> <@" + another + ">\n<@" + another + "> b", "mentions": users(another)}, false, true, nil},
		{"another, in a block that a line opens which may be a quote", fields{"content": ">```\n<@" + another + ">\n```", "mentions": users(another)}, false, false, nil},
		{"another, in a block left open", fields{"content": "a ```<@" + another + ">", "mentions": users(another)}, false, false, nil},
		{"one the service tells of in a quote, and one it does not outside", fields{"content": "> <@" + another + ">\n<@1000000000000000952>", "mentions": users(another)}, false, false, nil},
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
// gateway sent it and as REST read it. Where the service lists no mention
// of the account, the message is read once more as if it did, so that the
// driver's own reading is what keeps it from being a call.
func TestCapturedCalls(t *testing.T) {
	bot := snowflake.MustParse(id(payload(t, "calls/rest-me.json")["id"]))
	guild := payload(t, "calls/gw-guild.json")
	var role string
	for _, r := range guild["roles"].([]any) {
		if r := r.(map[string]any); r["managed"] == true {
			role = id(r["id"])
		}
	}
	names := strings.NewReplacer("{bot}", "<@"+bot.String()+">", "{role}", "<@&"+role+">")
	mention := regexp.MustCompile(`^<@&?\d+>$`)
	for _, c := range []struct {
		name         string
		call, others bool
		own          []string // each own mention and the rest of its line
		restOnly     bool     // the edit and the deletion that make the case come after the message
		listed       bool     // read again with the account in mentions
		listedOwn    []string // own mentions then
	}{
		{"mention", true, false, []string{"{bot} [hataori test] hello"}, false, false, nil},
		{"mention-role", true, false, []string{"{role} [hataori test] hello"}, false, false, nil},
		{"reply", true, false, nil, false, false, nil},
		{"reply-silent", true, false, nil, false, false, nil},
		{"quoted-and-not", true, false, []string{"{bot} b"}, false, false, nil},
		{"quoted", false, false, nil, false, false, nil},
		{"quoted-lines", false, false, nil, false, false, nil},
		{"in-code", false, false, nil, false, true, nil},
		{"in-block", false, false, nil, false, true, nil},
		{"in-open-block", false, false, nil, false, false, nil},
		{"everyone", false, false, nil, false, false, nil},
		{"here", false, false, nil, false, false, nil},
		{"other-role", false, false, nil, false, false, nil},
		{"webhook", false, false, nil, false, true, []string{"{bot} [hataori test] a webhook"}},
		{"forward", false, false, nil, false, false, nil},
		{"edited", false, false, []string{"{bot} [hataori test] hello"}, true, false, nil},
		{"reply-deleted", false, false, nil, true, false, nil},
		{"reply-other", false, true, nil, false, false, nil},
		{"other", false, true, nil, false, false, nil},
	} {
		for _, listed := range []bool{false, true} {
			if listed && !c.listed {
				continue
			}
			name := c.name
			if listed {
				name += ", listed"
			}
			t.Run(name, func(t *testing.T) {
				r, s := newFakeREST(t), newSink()
				d := testDriver(t, bot, r, nil)
				event(t, d, s, "GUILD_CREATE", guild)
				p := payload(t, "calls/rest-"+c.name+".json")
				if listed {
					p["mentions"] = users(bot.String())
				}
				r.serveJSON(t, "GET /channels/"+id(p["channel_id"])+"/messages", 200, []any{p})
				d.setDirect(snowflake.MustParse(id(p["channel_id"])), false)
				got, err := d.Since(context.Background(), surface.Conversation{Channel: id(p["channel_id"])}, "")
				if err != nil || len(got) != 1 {
					t.Fatalf("%+v, %v", got, err)
				}
				sent := []any{p["content"]} // what each message of got is read from
				if !c.restOnly {
					g := capture(t, "calls/gw-"+c.name+".json")
					if listed {
						g["d"].(map[string]any)["mentions"] = users(bot.String())
					}
					event(t, d, s, g["t"].(string), g["d"])
					if len(s.messages) != 1 {
						t.Fatalf("the gateway's message delivered %d", len(s.messages))
					}
					got = append(got, s.messages...)
					sent = append(sent, g["d"].(map[string]any)["content"])
				}
				var want []string
				own := c.own
				if listed {
					own = c.listedOwn
				}
				for _, o := range own {
					want = append(want, names.Replace(o))
				}
				for i, in := range got {
					if in.Text != sent[i] {
						t.Errorf("text %q, sent %q", in.Text, sent[i])
						continue
					}
					var own []string
					for _, m := range in.OwnMentions {
						if !mention.MatchString(in.Text[m.Start:m.End]) {
							t.Errorf("own mention %q", in.Text[m.Start:m.End])
						}
						line, _, _ := strings.Cut(in.Text[m.Start:], "\n")
						own = append(own, line)
					}
					if in.Call != c.call || in.OthersOnly != c.others || !reflect.DeepEqual(own, want) {
						t.Errorf("call %v, to others only %v, own mentions %q; want %v, %v, %q\n%q",
							in.Call, in.OthersOnly, own, c.call, c.others, want, in.Text)
					}
				}
			})
		}
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

// What a message costs to read stays small, whatever is in it: whoever can
// post chooses it.
func TestALongTextIsReadInTime(t *testing.T) {
	const ticks = "```"
	forms := ""
	for n := range 60 {
		forms += strings.Repeat(" ", n) + "> a\n"
	}
	for name, text := range map[string]string{
		"backticks": strings.Repeat(">\\``\n", 4000) + "` " + strings.Repeat("\\"+ticks, 40000) + "\n>",
		"forms":     forms,
		"items and lines": "- > a\n+ > a\n* > a\n1. > a\n# > a\n>a\n" +
			strings.Repeat(ticks+"a"+ticks+"-\u3000", 500) + strings.Repeat("\n ", 2000) + "\n>",
	} {
		began := time.Now()
		if got := spoken(text); strings.Contains(got, ">") {
			t.Errorf("%s: a quote is left of %d bytes", name, len(text))
		}
		if took := time.Since(began); took > 10*time.Second {
			t.Errorf("%s: %d bytes took %v", name, len(text), took)
		}
	}
}

// The role that is managed for the bot is learned from a server's Guild
// Create, and from its Guild Role Create or Update; a list that cannot be
// read takes nothing from the layout.
func TestTheManagedRoleIsLearned(t *testing.T) {
	bot := botID(t).String()
	role := guildRoles(bot)[0]
	unread := fields{"id": ownRole, "managed": true, "tags": fields{"bot_id": []any{}}}
	p := roleCall(t, ownRole)
	for name, c := range map[string]struct {
		typ     string
		payload any
		call    bool
		unread  bool // a part of it cannot be read
	}{
		"Guild Create":      {"GUILD_CREATE", fields{"id": p["guild_id"], "roles": guildRoles(bot)}, true, false},
		"Guild Role Create": {"GUILD_ROLE_CREATE", fields{"guild_id": p["guild_id"], "role": role}, true, false},
		"Guild Role Update": {"GUILD_ROLE_UPDATE", fields{"guild_id": p["guild_id"], "role": role}, true, false},
		"one of two":        {"GUILD_CREATE", fields{"id": p["guild_id"], "roles": append([]any{unread}, guildRoles(bot)...)}, true, true},
		"one read in part":  {"GUILD_CREATE", fields{"id": p["guild_id"], "roles": append(guildRoles(bot), unread)}, true, true},
		"another's role":    {"GUILD_ROLE_CREATE", fields{"guild_id": p["guild_id"], "role": fields{"id": ownRole, "managed": true, "tags": fields{"bot_id": another}}}, false, false},
		"no role":           {"GUILD_ROLE_UPDATE", fields{"guild_id": p["guild_id"]}, false, false},
		"not a list":        {"GUILD_CREATE", fields{"id": p["guild_id"], "roles": "none"}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := testDriver(t, botID(t), newFakeREST(t), nil).roleEvent(c.typ, raw(t, c.payload)); (err != nil) != c.unread {
				t.Errorf("%v, want an error: %v", err, c.unread)
			}
			// A driver that was told only by the gateway.
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
		{"a notice", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"type": 6}, false, false, false},
		{"a webhook's", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"webhook_id": "1000000000000000951"}, false, false, false},
		{"a quoted mention", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"content": "> <@&" + ownRole + ">"}, false, false, false},
		{"not told by the service", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"mention_roles": []any{}}, false, false, false},
		{"another told by the service", func(d *Driver, s *sink) { d.newSession(nil) }, fields{"mention_roles": []any{"1000000000000000929"}}, false, false, false},
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

// catchingUp is a sink that catches up as soon as a session is ready, as the
// front end does.
type catchingUp struct {
	*sink
	since func()
}

func (c catchingUp) Connected() { c.since() }

// Run begins the wait with each session: a catch-up that the session's start
// makes waits for the roles that come in it, also when those of the session
// before are known.
func TestASessionBeginsTheWaitForItsRoles(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	m := roleCall(t, ownRole)
	ch := id(m["channel_id"])
	r.serveJSON(t, "GET /channels/"+ch+"/messages", 200, []any{m})
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, botID(t), r, g)
	d.setDirect(snowflake.MustParse(ch), false)
	got := make(chan error, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx, catchingUp{newSink(), func() {
			_, err := d.Since(ctx, surface.Conversation{Channel: ch}, "")
			got <- err
		}})
	}()
	defer func() {
		cancel()
		<-done
	}()
	for _, session := range []string{"the first", "another"} {
		if session == "another" {
			d.mu.Lock()
			d.session = d.session.Add(-2 * rolesWithin) // the first wait is over
			d.mu.Unlock()
			g.mu.Lock()
			g.refuseResume = true
			g.mu.Unlock()
			g.drop()
		}
		select {
		case err := <-got:
			if !errors.Is(err, errRoles) {
				t.Errorf("%s: %v, want to wait for the roles", session, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: no session", session)
		}
		// The roles of this session come, without the bot's: they are known
		// for the rest of it.
		g.event("GUILD_CREATE", fields{"id": m["guild_id"], "roles": []any{}, "threads": []any{},
			"channels": []any{fields{"id": ch, "guild_id": m["guild_id"], "type": 0, "position": 0, "name": "c", "flags": 0}}})
		for start := time.Now(); ; time.Sleep(10 * time.Millisecond) {
			if _, err := d.Since(ctx, surface.Conversation{Channel: ch}, ""); err == nil {
				break
			} else if time.Since(start) > 15*time.Second {
				t.Fatalf("%s: %v", session, err)
			}
		}
	}
}
