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
	mentions := userMentions(m.Content)
	for _, n := range mentions {
		if n.role && slices.Contains(m.MentionRoles, n.id) && d.ownRole(n.id) ||
			!n.role && n.id == d.botID && m.mentions(n.id) {
			in.OwnMentions = append(in.OwnMentions, n.at)
		}
	}
	reply := m.Type == typeReply
	own, known := m.toPostOf(d.botID)
	in.Call = m.mayCall(in.Direct) && (len(in.OwnMentions) > 0 || own)

	mayBeOwn := reply && (own || !known) || m.mentions(d.botID) ||
		len(m.MentionRoles) > 0 || m.MentionEveryone || d.mayMentionOwn(m.Content)
	in.OthersOnly = !mayBeOwn && (reply || slices.ContainsFunc(mentions, func(n mention) bool {
		return !n.role && m.mentions(n.id)
	}))
}

// mentions reports whether the service says that m mentions the account id.
func (m wireMessage) mentions(id snowflake.ID) bool {
	return slices.ContainsFunc(m.Mentions, func(u wireUser) bool { return u.ID == id })
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
	if !userMention.MatchString(text) {
		return nil
	}
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

const (
	// white is white space that ends no line.
	white = `[ \t\v\f\r\p{Z}\x{85}\x{feff}]`
	// item begins an item of a list, a heading or small text.
	item = `(?:[*+-]|[0-9]+[.)]|-?#{1,3})` + white
)

var (
	// quoteSign is how a line begins that is one of a quote, or may be: what
	// stands before the sign, the two signs more of a quote to the end of the
	// text, and the space after the sign.
	quoteSign = regexp.MustCompile("^((?:" + white + "|" + item + ")*)>(>>)?(" + white + "?)")
	itemSign  = regexp.MustCompile("^" + white + "*(?:" + item + ")+")
	indented  = regexp.MustCompile("^" + white)
	number    = regexp.MustCompile("[0-9]+")
)

// mostDoubts is how many doubts a text may raise. One that raises more is
// taken for no one's own words.
const mostDoubts = 6

// spoken is text with spaces for what its author quoted (a line that begins
// with ">", and everything from one that begins with ">>>"), for code (also
// a block left open) and for escaped characters. What is left stands at the
// byte it stood at.
//
// Where the client may show a text in either of two ways, it is read in both,
// each doubt taken one way for the whole text, and what is left is what every
// reading leaves.
func spoken(text string) string {
	out := []byte(text)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			out[i] = ' '
		}
	}
	var doubts []string
	for taken := 0; taken < 1<<len(doubts); taken++ { // a doubt met on the way is not taken yet
		doubt := func(what string) bool {
			n := slices.Index(doubts, what)
			if n < 0 {
				n, doubts = len(doubts), append(doubts, what)
			}
			return taken&(1<<n) != 0
		}
		text := text
		if strings.Contains(text, "\r") && doubt("\r") {
			text = strings.ReplaceAll(text, "\r", "\n")
		}
		read(text, doubt, blank)
		if len(doubts) > mostDoubts {
			return strings.Repeat(" ", len(text))
		}
	}
	return string(out)
}

// read calls blank with each part of text that is quoted, in code or
// escaped. doubt reports whether a doubt is taken in this reading:
//   - whether lines that begin in one way are quotes, for each way but "> "
//     and ">>> " where a line begins;
//   - whether code ends at the latest with the heading or with the item of a
//     list that it opens in;
//   - whether inline code ends at the latest before a line that begins as one
//     of a quote does.
func read(text string, doubt func(string) bool, blank func(from, to int)) {
	var quotes []int // the lines that begin as one of a quote does
	for i := 0; i < len(text); i = lineEnd(text, i) + 1 {
		if _, _, ok := quote(text[i:]); ok {
			quotes = append(quotes, i)
		}
	}
	start, block := 0, ""        // where a quote may begin, and whether a block ends there
	in, inEnd := "", 0           // an item or a heading, and its end
	var next [len(fenceMark)]int // where a run of so many backticks is next
	for i := 0; i < len(text); {
		for len(quotes) > 0 && quotes[0] <= i {
			quotes = quotes[1:]
		}
		if i == start {
			if form, toEnd, ok := quote(text[i:]); ok && (block+form == "" || doubt(block+form)) {
				end := len(text)
				if !toEnd {
					end = lineEnd(text, i)
				}
				blank(i, end)
				i, start, block = end+1, end+1, ""
				continue
			}
			if block == "" {
				if sign := itemSign.FindString(text[i:]); sign != "" {
					in, inEnd = "#", lineEnd(text, i)
					if !strings.Contains(sign, in) {
						in, inEnd = "-", itemEnd(text, i)
					}
				}
			}
		}
		switch {
		case text[i] == '\n':
			i++
			start, block = i, ""
		case text[i] == '\\' && i+1 < len(text) && strings.IndexByte("\\`<", text[i+1]) >= 0:
			blank(i, i+2)
			i += 2
		case strings.HasPrefix(text[i:], fenceMark):
			end := len(text)
			if code := codeBlock.FindString(text[i:]); code != "" {
				end = i + len(code)
			}
			if i < inEnd && inEnd < end && doubt(in) {
				end = inEnd
			}
			blank(i, end)
			i, start, block = end, end, fenceMark
		case text[i] == '`':
			run := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
			if next[run] < i+run {
				next[run] = i + run + ticks(text[i+run:], run)
			}
			end := next[run]
			if end == len(text) || i < inEnd && inEnd < end && doubt(in) ||
				len(quotes) > 0 && quotes[0] < end && doubt("`") {
				i++ // a shorter part of the run may begin code
				continue
			}
			blank(i, end+run)
			i = end + run
		default:
			i++
		}
	}
}

// quote reports whether line may begin as one of a quote does, how it begins
// when that leaves it in doubt, and whether the quote has all that follows.
func quote(line string) (form string, toEnd, ok bool) {
	m := quoteSign.FindStringSubmatch(line)
	if m == nil {
		return "", false, false
	}
	before, more, after := m[1], m[2], m[3]
	if after == "" && strings.ContainsAny(before, "#*+-.)") {
		return "", false, false // "- >50%"
	}
	if before != "" || after != " " {
		form = number.ReplaceAllString(m[0], "1")
	}
	return form, more != "", true
}

// lineEnd is where the line of text ends that i is in.
func lineEnd(text string, i int) int {
	if n := strings.IndexByte(text[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(text)
}

// itemEnd is where the item of a list ends that begins at i: before a line
// that is not indented, or begins an item itself.
func itemEnd(text string, i int) int {
	end := lineEnd(text, i)
	for end < len(text) && indented.MatchString(text[end+1:]) && !itemSign.MatchString(text[end+1:]) {
		end = lineEnd(text, end+1)
	}
	return end
}

// ticks is where in text the first run of just n backticks begins, or its
// length when there is none.
func ticks(text string, n int) int {
	for i := 0; i < len(text); {
		run := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
		if run == n {
			return i
		}
		i += max(run, 1)
	}
	return len(text)
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
	if len(m.MentionRoles) == 0 || !m.mayCall(direct) {
		return false
	}
	mentions := userMentions(m.Content)
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.session) > rolesWithin {
		return false
	}
	for _, n := range mentions {
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
