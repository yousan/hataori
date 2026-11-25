package discord

import (
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

// What a message mentions is read from its text: the captured posts mention
// the bot itself, then everyone and a role, then, as a reply, nobody; the
// first one, read by another bot, mentions another account (#799).
func TestMentionsOthers(t *testing.T) {
	text := func(name string) string { return payload(t, name)["content"].(string) }
	self, err := snowflake.Parse(id(payload(t, "lamps/rest-post-notify.json")["author"].(map[string]any)["id"]))
	if err != nil {
		t.Fatal(err)
	}
	one := text("lamps/rest-post-notify.json") // "<@...1> ..."
	for _, c := range []struct {
		name string
		bot  snowflake.ID
		text string
		want bool
	}{
		{"the bot", self, one, false},
		{"another account", self + 5, one, true},
		{"everyone and a role", self + 5, text("lamps/rest-post-everyone.json"), false},
		{"another account and everyone", self + 5, one + text("lamps/rest-post-everyone.json"), false},
		{"another account and the bot", self + 5, one + " <@!" + (self + 5).String() + ">", false},
		{"nobody, a reply", self + 5, text("bot/gw-07-MESSAGE_CREATE.json"), false},
		{"in a block of code", self + 5, "```\n" + one + "\n```", false},
		{"in code", self + 5, "`" + one + "` and ``" + one + "``", false},
		{"escaped", self + 5, `\` + one, false},
		{"in code and out of it", self + 5, "`" + one + "`" + one, true},
		{"the bot in code", self + 5, one + "`<@" + (self + 5).String() + ">`", false},
	} {
		d := newDriver(c.bot, "t", "", "", nil)
		if got := d.MentionsOthers(c.text); got != c.want {
			t.Errorf("%s: MentionsOthers(%q) = %v", c.name, c.text, got)
		}
	}
}
