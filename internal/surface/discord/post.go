package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// Discord's limits (research note, section 5).
const (
	maxText      = 2000 // characters of one message's content
	maxFiles     = 10   // attachments of one message
	maxTitle     = 100  // characters of a thread name
	archiveAfter = 10080
)

// Error is a failed call to the service. Temporary reports whether trying
// the same call again later can succeed: true for a rate limit that outlasted
// the retries, a server error or a network failure; false for a request the
// service refused, such as a message that is too long or a missing
// permission.
type Error struct {
	Op      string
	Status  int    // the HTTP status, 0 when no response came
	Code    int    // Discord's JSON error code, 0 when none
	Message string // the service's message
	// Local is a failure before the request reached the service, such as
	// a file that cannot be read: trying again does not help.
	Local bool
	Err   error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("discord: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("discord: %s: %d %s (code %d)", e.Op, e.Status, e.Message, e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

// Temporary reports whether a retry can help.
func (e *Error) Temporary() bool {
	return !e.Local && (e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500)
}

// Discord's JSON error code for a message that does not exist.
const codeUnknownMessage = 10008

// Discord's JSON error code for opening a thread from a message that has one
// (400, "A thread has already been created for this message", seen on
// 2026-11-17).
const codeThreadExists = 160004

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	e := &Error{Op: op, Err: err}
	var re *rest.Error
	if errors.As(err, &re) {
		if re.Response != nil {
			e.Status = re.Response.StatusCode
		}
		e.Code = int(re.Code)
		e.Message = re.Message
		return e
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	e.Local = true
	return e
}

func parseID(what, s string) (snowflake.ID, error) {
	id, err := snowflake.Parse(s)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("discord: %s %q is not an id", what, s)
	}
	return id, nil
}

// target is the channel that messages of conv are posted to: the thread
// when there is one.
func target(conv surface.Conversation) (snowflake.ID, error) {
	if conv.Thread != "" {
		return parseID("thread", conv.Thread)
	}
	return parseID("channel", conv.Channel)
}

// OpenThread opens a public thread in parent.Channel. When parent.Thread is
// set it names a message in that channel, and the thread is started from
// it; on Discord the thread then has the message's id. Otherwise the thread
// is started without a message, and Discord posts a notice of it (message
// type 18, by the bot) in the channel. The title is cut to 100 characters.
//
// When the message has a thread already, as when the answer to an earlier
// call was lost, the service refuses (code 160004) and OpenThread returns
// the thread the message names instead.
func (d *Driver) OpenThread(ctx context.Context, parent surface.Conversation, title string) (surface.Conversation, error) {
	ch, err := parseID("channel", parent.Channel)
	if err != nil {
		return surface.Conversation{}, err
	}
	name := cutRunes(strings.TrimSpace(title), maxTitle)
	if name == "" {
		name = "thread"
	}
	var c wireChannel
	if parent.Thread != "" {
		msg, err := parseID("message", parent.Thread)
		if err != nil {
			return surface.Conversation{}, err
		}
		body := map[string]any{"name": name, "auto_archive_duration": archiveAfter}
		err = d.client.Do(rest.CreateThreadWithMessage.Compile(nil, ch, msg), body, &c, rest.WithCtx(ctx))
		if err != nil {
			err = wrap("open a thread", err)
			var e *Error
			if !errors.As(err, &e) || e.Code != codeThreadExists {
				return surface.Conversation{}, err
			}
			var m struct {
				Thread *wireChannel `json:"thread"`
			}
			if err := d.client.Do(rest.GetMessage.Compile(nil, ch, msg), nil, &m, rest.WithCtx(ctx)); err != nil {
				return surface.Conversation{}, wrap("look up the message's thread", err)
			}
			if m.Thread == nil || m.Thread.ID == 0 {
				return surface.Conversation{}, err
			}
			c = *m.Thread
		}
	} else {
		body := map[string]any{"name": name, "type": 11, "auto_archive_duration": archiveAfter}
		if err := d.client.Do(rest.CreateThread.Compile(nil, ch), body, &c, rest.WithCtx(ctx)); err != nil {
			return surface.Conversation{}, wrap("open a thread", err)
		}
	}
	d.setParent(c)
	return surface.Conversation{Channel: parent.Channel, Thread: c.ID.String()}, nil
}

// Post posts m, split into several messages in order when its text is longer
// than one message may be; its files go on the last one. A code block cut by
// the split is closed in one message and opened again in the next. It
// returns the first message; with choices, the last one, which carries
// them, so that an Edit of what it returns can remove them.
//
// key is sent as each message's nonce, with enforce_nonce: Discord then
// returns the earlier message instead of posting again when the same nonce
// comes from the bot within a few minutes. Past that window a repeated key
// posts again; the caller's outbox has to know what it already posted.
// When the message posted with the key was deleted meanwhile, the service
// answers 404 Unknown Message (seen on 2026-11-17), which comes back as an
// Error that is not temporary.
//
// A Post whose ReplyTo is a command's Reply answers that command in place
// instead (it cannot carry files). Its first part replaces the service's
// "thinking" note under the command and later parts are follow-ups; they are
// private when the command's spec says so. A private answer can be neither
// edited nor deleted afterwards. The service takes answers for 15 minutes
// after the command; after that, or after a restart, the Post fails with an
// Error that is not temporary.
func (d *Driver) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	if m.ReplyTo != nil && strings.HasPrefix(m.ReplyTo.ID, commandReplyPrefix) {
		return d.postAnswer(ctx, conv, key, m)
	}
	ch, err := target(conv)
	if err != nil {
		return surface.MessageRef{}, err
	}
	buttons, err := components(m.Choices, m.Multi)
	if err != nil {
		return surface.MessageRef{}, err
	}
	if len(m.Choices) > 0 && len(m.Files) > 0 {
		return surface.MessageRef{}, errors.New("discord: a post with choices cannot carry files")
	}
	if len(m.Files) > maxFiles {
		return surface.MessageRef{}, fmt.Errorf("discord: %d files, at most %d fit in one message", len(m.Files), maxFiles)
	}
	text := m.Text
	if len(m.Notify) > 0 {
		var b strings.Builder
		for _, who := range m.Notify {
			fmt.Fprintf(&b, "<@%s> ", who)
		}
		text = b.String() + text
	}
	if text == "" && len(m.Files) == 0 {
		return surface.MessageRef{}, errors.New("discord: nothing to post")
	}
	// The files are opened before anything is posted, so that a file that
	// cannot be read does not leave half a post behind.
	files, err := openFiles(m.Files)
	if err != nil {
		return surface.MessageRef{}, err
	}
	defer func() {
		for _, f := range files {
			f.r.Close()
		}
	}()
	parts := split(text, maxText)
	var first surface.MessageRef
	for i, part := range parts {
		mc := discord.MessageCreate{
			Content:         part,
			AllowedMentions: &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}},
		}
		// The pings are at the start of the first part; a later part
		// pings nobody, even when its text names someone.
		if i == 0 && len(m.Notify) > 0 {
			for _, who := range m.Notify {
				if id, err := snowflake.Parse(who); err == nil {
					mc.AllowedMentions.Users = append(mc.AllowedMentions.Users, id)
				}
			}
		}
		if key != "" {
			mc.Nonce, mc.EnforceNonce = nonce(key, i), true
		}
		if m.Quiet {
			mc.Flags = discord.MessageFlagSuppressNotifications
		}
		if i == 0 && m.ReplyTo != nil {
			if id, err := snowflake.Parse(m.ReplyTo.ID); err == nil {
				mc.MessageReference = &discord.MessageReference{MessageID: &id, FailIfNotExists: false}
			}
		}
		if i == len(parts)-1 {
			for _, f := range files {
				mc.Files = append(mc.Files, discord.NewFile(f.name, "", f.r))
			}
		}
		body, err := mc.ToBody()
		var out wireMessage
		if err != nil {
			return first, &Error{Op: "post", Local: true, Err: err}
		}
		if i == len(parts)-1 && len(m.Choices) > 0 {
			// The buttons go on the last part, added as plain JSON rather
			// than through disgo's component types.
			var plain map[string]any
			raw, err := json.Marshal(body)
			if err == nil {
				err = json.Unmarshal(raw, &plain)
			}
			if err != nil {
				return first, &Error{Op: "post", Local: true, Err: err}
			}
			plain["components"] = buttons
			body = plain
		}
		err = d.client.Do(rest.CreateMessage.Compile(nil, ch), body, &out, rest.WithCtx(ctx))
		if err != nil {
			return first, wrap("post", err)
		}
		if i == 0 {
			first = surface.MessageRef{Conv: conv, ID: out.ID.String()}
		}
		if i == len(parts)-1 && len(m.Choices) > 0 {
			first = surface.MessageRef{Conv: conv, ID: out.ID.String()}
		}
	}
	return first, nil
}

type openFile struct {
	name string
	r    *os.File
}

func openFiles(fs []surface.File) ([]openFile, error) {
	var out []openFile
	for _, f := range fs {
		r, err := os.Open(f.Path)
		if err == nil {
			var st os.FileInfo
			if st, err = r.Stat(); err == nil && !st.Mode().IsRegular() {
				err = errors.New("not a regular file")
			}
			if err != nil {
				r.Close()
			}
		}
		if err != nil {
			for _, o := range out {
				o.r.Close()
			}
			return nil, &Error{Op: "post", Local: true, Err: fmt.Errorf("file %q: %w", f.Name, err)}
		}
		out = append(out, openFile{name: f.Name, r: r})
	}
	return out, nil
}

// nonce is the idempotency key of part i of a post with key: at most 25
// characters, as Discord allows.
func nonce(key string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", key, i)))
	return hex.EncodeToString(sum[:])[:25]
}

// Edit replaces the text of a posted message. A text longer than one
// message is refused: an edit cannot become several messages.
func (d *Driver) Edit(ctx context.Context, ref surface.MessageRef, m surface.Message) error {
	ch, err := target(ref.Conv)
	if err != nil {
		return err
	}
	id, err := parseID("message", ref.ID)
	if err != nil {
		return err
	}
	if n := length(m.Text); n > maxText {
		return &Error{Op: "edit", Status: http.StatusBadRequest, Message: fmt.Sprintf("%d characters, at most %d fit", n, maxText)}
	}
	buttons, err := components(m.Choices, m.Multi)
	if err != nil {
		return err
	}
	// An edit replaces the message: no choices remove its buttons and its
	// menu.
	body := map[string]any{"content": m.Text, "components": buttons}
	return wrap("edit", d.client.Do(rest.UpdateMessage.Compile(nil, ch, id), body, nil, rest.WithCtx(ctx)))
}

// Delete deletes a posted message. A message that is already gone counts as
// deleted.
func (d *Driver) Delete(ctx context.Context, ref surface.MessageRef) error {
	ch, err := target(ref.Conv)
	if err != nil {
		return err
	}
	id, err := parseID("message", ref.ID)
	if err != nil {
		return err
	}
	err = wrap("delete", d.client.Do(rest.DeleteMessage.Compile(nil, ch, id), nil, nil, rest.WithCtx(ctx)))
	var e *Error
	if errors.As(err, &e) && e.Code == codeUnknownMessage {
		return nil
	}
	return err
}

// SetThread sets a thread's title and archived flag. The bot may do this
// to a thread it opened without Manage Threads. It never sets locked, so a
// person's message un-archives an archived thread.
func (d *Driver) SetThread(ctx context.Context, conv surface.Conversation, t surface.ThreadMeta) error {
	th, err := parseID("thread", conv.Thread)
	if err != nil {
		return err
	}
	body := map[string]any{"archived": t.Archived}
	if name := cutRunes(strings.TrimSpace(t.Title), maxTitle); name != "" {
		body["name"] = name
	}
	return wrap("set the thread", d.client.Do(rest.UpdateChannel.Compile(nil, th), body, nil, rest.WithCtx(ctx)))
}

var _ surface.Namer = (*Driver)(nil)

var _ surface.Linker = (*Driver)(nil)

// Link returns the mention of a thread or a channel, which Discord shows
// as a link to it and which notifies nobody.
func (d *Driver) Link(conv surface.Conversation) string {
	id, err := target(conv)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("<#%d>", id)
}

// Name is the name Discord shows for conv: "#" and the channel's name for
// a channel, its own name for a thread, and none for a direct conversation.
func (d *Driver) Name(ctx context.Context, conv surface.Conversation) (string, error) {
	what, v := "channel", conv.Channel
	if conv.Thread != "" {
		what, v = "thread", conv.Thread
	}
	ch, err := parseID(what, v)
	if err != nil {
		return "", err
	}
	var c wireChannel
	if err := d.client.Do(rest.GetChannel.Compile(nil, ch), nil, &c, rest.WithCtx(ctx)); err != nil {
		return "", wrap("look up the conversation's name", err)
	}
	switch {
	case c.Type == 1 || c.Type == 3: // a direct or group direct conversation
		return "", nil
	case isThread(c.Type):
		d.setParent(c)
		return c.Name, nil
	case c.Name == "":
		return "", nil
	}
	return "#" + c.Name, nil
}

// Limits reports what the driver can do.
func (d *Driver) Limits() surface.Limits {
	return surface.Limits{
		MaxText:     maxText,
		MaxChoices:  maxChoices,
		MaxFiles:    maxFiles,
		Edit:        true,
		Delete:      true,
		SetThread:   true,
		SetStatus:   true,
		Commands:    true,
		MultiSelect: true,
	}
}

// length counts s as Discord's limit is checked here: in UTF-16 code units,
// which is never fewer than the code points. Which of the two Discord counts
// is unverified (research note, section 5), so the stricter one is used.
func length(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func cutRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

const fenceMark = "```"

// maxOpener bounds the opening line of a code block that a part repeats:
// the fence and its language, cut so that a long opening line cannot crowd
// out the text.
const maxOpener = 32

// split cuts text into parts of at most limit characters (see length). It
// cuts after a line where it can, else after a space, else anywhere, and
// drops nothing: the parts, without the fences split adds, are the text.
// When a part ends inside a code block, the part is closed with a fence
// and the next part opens the block again.
func split(text string, limit int) []string {
	if length(text) <= limit {
		return []string{text}
	}
	var parts []string
	open := "" // the opening line of a code block the previous part left open
	rest := text
	for rest != "" {
		prefix := ""
		if open != "" {
			prefix = open + "\n"
		}
		if length(prefix+rest) <= limit {
			parts = append(parts, prefix+rest)
			break
		}
		budget := limit - length(prefix) - length("\n"+fenceMark)
		cut := cutAt(rest, budget)
		chunk := rest[:cut]
		rest = rest[cut:]
		open = fenceAfter(open, chunk)
		part := prefix + chunk
		if open != "" {
			if !strings.HasSuffix(part, "\n") {
				part += "\n"
			}
			part += fenceMark
		}
		parts = append(parts, part)
	}
	return parts
}

// cutAt returns where to cut s so that s[:cut] has at most budget
// characters: after the last newline or space in the second half of what
// fits, else where the budget ends. It always takes at least one character.
func cutAt(s string, budget int) int {
	n, end := 0, len(s)
	for i, r := range s {
		w := utf16.RuneLen(r)
		if n+w > budget {
			end = i
			break
		}
		n += w
	}
	if end == 0 {
		_, size := utf8.DecodeRuneInString(s)
		return size
	}
	head := s[:end]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 && i+1 > len(head)/2 {
		return i + 1
	}
	if i := strings.LastIndexByte(head, ' '); i >= 0 && i+1 > len(head)/2 {
		return i + 1
	}
	return end
}

// fenceAfter tells whether a code block is open after chunk, given the
// opening line of the block open before it: the block's opening line, cut
// to maxOpener characters, or "".
func fenceAfter(open, chunk string) string {
	for _, line := range strings.Split(chunk, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, fenceMark) {
			continue
		}
		if open == "" {
			lang, _, _ := strings.Cut(strings.TrimLeft(t, "`"), " ")
			open = fenceMark + cutRunes(lang, maxOpener-len(fenceMark))
		} else {
			open = ""
		}
	}
	return open
}
