package discord

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// A call (surface.Inbound.Call) is told from two things that must agree:
// what the service reports of the message (mentions, mention_roles with the
// role's tags, referenced_message, type), and where the mention stands in
// the text. The report alone is not enough: it may hold a mention in a
// quoted line as any other, and the author a reply pings, which the text
// does not mention.

var _ surface.Mentioner = (*Driver)(nil)

var (
	// codeText is what Discord shows as code, where nothing is a mention.
	codeText = regexp.MustCompile("(?s)```.*?```|``.+?``|`[^`]+`")
	// mentionText is a mention of a user, of a role or of everyone, escaped
	// or not.
	mentionText = regexp.MustCompile(`\\?(?:<@(!?|&)(\d+)>|@everyone|@here)`)
)

// MentionsOthers reports whether text mentions a user other than the bot,
// and neither the bot nor a role or everyone, which may hold it. One that
// is escaped or in code mentions nobody, but the bot's own there is taken
// for one all the same: what is in doubt is the bot's.
func (d *Driver) MentionsOthers(text string) bool {
	if d.mayMentionOwn(text) {
		return false
	}
	for _, m := range mentionText.FindAllString(codeText.ReplaceAllString(text, " "), -1) {
		if !strings.HasPrefix(m, `\`) {
			return true
		}
	}
	return false
}

// mayMentionOwn reports whether text mentions the bot, or a role or
// everyone, which may hold it, anywhere.
func (d *Driver) mayMentionOwn(text string) bool {
	for _, m := range mentionText.FindAllStringSubmatch(text, -1) {
		if m[2] == "" || m[1] == "&" || m[2] == d.botID.String() {
			return true
		}
	}
	return false
}

// refForward is the type of a message_reference that forwards the message
// it names.
const refForward = 1

// forwards reports whether m forwards another message.
func (m wireMessage) forwards() bool {
	return len(m.Snapshots) > 0 || m.MessageReference != nil && m.MessageReference.Type == refForward
}

// byPerson reports whether a person's account wrote m: no bot, no webhook
// and not the service.
func (m wireMessage) byPerson() bool {
	return m.WebhookID == 0 && !m.Author.Bot && !m.Author.System
}

// written reports whether somebody wrote m, as against a notice of the
// service.
func (m wireMessage) written() bool {
	return !m.Author.System && (m.Type == typeDefault || m.Type == typeReply)
}

// mayCall reports whether m can be a call at all, whatever it mentions.
func (m wireMessage) mayCall(direct bool) bool {
	return m.written() && m.byPerson() && !direct && m.Edited == nil && !m.forwards()
}

// toPostOf reports whether m replies to a post of the account id, and
// whether the service says whose post it is.
func (m wireMessage) toPostOf(id snowflake.ID) (own, known bool) {
	if m.Type != typeReply || m.Referenced == nil || m.Referenced.Author.ID == 0 {
		return false, false
	}
	return m.Referenced.Author.ID == id, true
}

// call sets what in says of whom m is written to.
func (d *Driver) call(m wireMessage, in *surface.Inbound) {
	if !m.written() {
		return
	}
	in.OwnMentions = d.ownMentions(m)
	reply := m.Type == typeReply
	own, known := m.toPostOf(d.botID)
	in.Call = m.mayCall(in.Direct) && (len(in.OwnMentions) > 0 || own)

	mayBeOwn := reply && (own || !known) || m.mentions(d.botID) ||
		len(m.MentionRoles) > 0 || m.MentionEveryone || d.mayMentionOwn(m.Content)
	in.OthersOnly = !mayBeOwn && (reply || slices.ContainsFunc(userMentions(m.Content), func(n mention) bool {
		return !n.role && m.mentions(n.id)
	}))
}

// mentions reports whether the service says that m mentions the account id.
func (m wireMessage) mentions(id snowflake.ID) bool {
	return slices.ContainsFunc(m.Mentions, func(u wireUser) bool { return u.ID == id })
}

// ownMentions are the places in m's text that mention the bot, or the role
// that is managed for it, and that the service reports as mentions.
func (d *Driver) ownMentions(m wireMessage) []surface.Span {
	var out []surface.Span
	for _, n := range userMentions(m.Content) {
		if n.role && slices.Contains(m.MentionRoles, n.id) && d.ownRole(n.id) ||
			!n.role && n.id == d.botID && m.mentions(n.id) {
			out = append(out, n.at)
		}
	}
	return out
}

// mention is a mention of a user or of a role in a text.
type mention struct {
	id   snowflake.ID
	role bool
	at   surface.Span
}

var userMention = regexp.MustCompile(`<@(!?|&)(\d+)>`)

// userMentions are the mentions of users and roles in text, but not those
// that are quoted, in code or escaped.
func userMentions(text string) []mention {
	var out []mention
	text = spoken(text)
	for _, m := range userMention.FindAllStringSubmatchIndex(text, -1) {
		id, err := snowflake.Parse(text[m[4]:m[5]])
		if err != nil {
			continue
		}
		out = append(out, mention{id: id, role: text[m[2]:m[3]] == "&", at: surface.Span{Start: m[0], End: m[1]}})
	}
	return out
}

// codeBlock is a code block that a text begins with. Its first line may name
// a language, and in it is at least one character that ends no line, so a
// fence straight after the first one is in the block.
var codeBlock = regexp.MustCompile("^(?is:```(?:[a-z0-9_+.#-]+?\n)?\n*[^\n].*?\n*```)")

var (
	// quoteLine is how a line of a quote begins.
	quoteLine = regexp.MustCompile("^ *> ")
	// maybeQuote is how a line begins that may be one of a quote: with other
	// spaces before the sign or none after it, or in a list or a heading.
	maybeQuote = regexp.MustCompile(`^(?:[\t\v\f\p{Z}\x{85}]|(?:[*+-]|[0-9]+[.)]|-?#{1,3})[\t\v\f\p{Z}\x{85}])*>(?:>>)?`)
)

// Where in a line spoken reads.
const (
	inLine = iota
	afterBlock
	lineStart
)

// spoken is text with spaces for what its author quoted (a line that begins
// with ">", and everything from one that begins with ">>>"), for code (also
// a block left open) and for escaped characters. What is left stands at the
// byte it stood at. A line that may be a quote is none of what is left, and
// the text after it is read both ways: code that opens in a quote ends with
// the quote, and any other runs on.
func spoken(text string) string {
	text = strings.ReplaceAll(text, "\r", "\n")
	out := []byte(text)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			out[i] = ' '
		}
	}
	read := map[int]bool{} // the starts read from
	for todo := []int{0}; len(todo) > 0; {
		i, at := todo[len(todo)-1], lineStart
		todo = todo[:len(todo)-1]
		for i < len(text) {
			if at != inLine {
				if read[i] {
					break
				}
				read[i] = true
				sign := maybeQuote.FindString(text[i:])
				if sign != "" {
					end, toEnd := len(text), strings.HasSuffix(sign, ">>>")
					if n := strings.IndexByte(text[i:], '\n'); n >= 0 && !toEnd {
						end = i + n
						todo = append(todo, end+1)
					}
					blank(i, end)
					if toEnd || at == lineStart && quoteLine.MatchString(text[i:]) {
						break
					}
				}
				at = inLine
			}
			switch {
			case text[i] == '\n':
				at = lineStart
				i++
			case text[i] == '\\' && i+1 < len(text) && strings.IndexByte("\\`<", text[i+1]) >= 0:
				blank(i, i+2)
				i += 2
			case strings.HasPrefix(text[i:], fenceMark):
				end := len(text)
				if block := codeBlock.FindString(text[i:]); block != "" {
					end = i + len(block)
				}
				blank(i, end)
				i, at = end, afterBlock
			case text[i] == '`':
				end := i + inlineCode(text[i:])
				blank(i, end)
				i = max(end, i+1)
			default:
				i++
			}
		}
	}
	return string(out)
}

// inlineCode is where the code ends that text, which begins with a
// backtick, begins with: a run of backticks ends at the next run as long.
// It is 0 when there is none, and a shorter part of the run may begin code.
func inlineCode(text string) int {
	ticks := func(s string) int { return len(s) - len(strings.TrimLeft(s, "`")) }
	run := ticks(text)
	for i := run; i < len(text); {
		n := ticks(text[i:])
		if n == run {
			return i + n
		}
		i += max(n, 1)
	}
	return 0
}

// wireRole is the part of a role the driver reads.
type wireRole struct {
	ID      snowflake.ID `json:"id"`
	Managed bool         `json:"managed"`
	Tags    struct {
		BotID snowflake.ID `json:"bot_id"`
	} `json:"tags"`
}

// keepRole keeps whether r is managed for the bot.
func (d *Driver) keepRole(r wireRole) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roles[r.ID] = r.Managed && r.Tags.BotID == d.botID
}

func (d *Driver) ownRole(id snowflake.ID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.roles[id]
}

// roleEvent keeps the roles a gateway event of type t carries: a server's,
// with Guild Create, and one that was made or changed. A role is managed
// for the bot from when it is made, and its id is never another's, so
// nothing is forgotten.
func (d *Driver) roleEvent(t string, payload []byte) error {
	switch t {
	case "GUILD_CREATE":
		var g struct {
			Roles []json.RawMessage `json:"roles"`
		}
		if err := json.Unmarshal(payload, &g); err != nil {
			return err
		}
		var bad []error
		for _, b := range g.Roles {
			var r wireRole
			if err := json.Unmarshal(b, &r); err != nil {
				bad = append(bad, err)
				continue
			}
			d.keepRole(r)
		}
		return errors.Join(bad...)
	case "GUILD_ROLE_CREATE", "GUILD_ROLE_UPDATE":
		var r struct {
			Role wireRole `json:"role"`
		}
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		d.keepRole(r.Role)
	}
	return nil
}

// errRoles is why a history cannot be read yet: a message in it mentions a
// role, and which roles the server has has not come yet.
var errRoles = errors.New("discord: the server's roles are not known yet")

// rolesWithin is how long after a session began its servers' roles are
// waited for.
const rolesWithin = time.Minute

// awaitsRoles reports whether m, posted in conv, may be a call by a role
// that the driver does not know yet. A server's roles come with its Guild
// Create, after the session is ready: a catch-up that began meanwhile is
// made again. Without them in time the role is nobody's, so that one
// message does not hold back the conversation.
func (d *Driver) awaitsRoles(m wireMessage, conv surface.Conversation, direct bool) bool {
	if !m.mayCall(direct) {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.session) > rolesWithin {
		return false
	}
	for _, n := range userMentions(m.Content) {
		if _, ok := d.roles[n.id]; !ok && n.role && slices.Contains(m.MentionRoles, n.id) && !d.rolesOf(conv) {
			return true
		}
	}
	return false
}

// rolesOf reports whether the roles of the server that conv is in came in
// this session. d.mu is held.
func (d *Driver) rolesOf(conv surface.Conversation) bool {
	ch, err := snowflake.Parse(conv.Channel)
	if err != nil {
		return false
	}
	for _, g := range d.guilds {
		if _, ok := g.channels[ch]; ok && g.current {
			return true
		}
	}
	return false
}
