package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// LoginRelay is the login relay as the front end uses it (relay.Relay).
type LoginRelay interface {
	// from is the "<client>:<id>" of who asks; the relay resolves it.
	Start(ctx context.Context, from, agentName string) error
	Code(from, agentName, code string) error
}

// maxListed is how many stopped threads the owner's message names.
const maxListed = 15

// The login alert (design, section 4.7): one direct message to the owner
// per outage, edited as threads stop and as the relay goes on. The message
// is posted and edited by the driver directly, not through the outbox, so
// that the sign-in link is never written down: it lives only in memory and
// in the owner's direct conversation. What is kept is the outage, its
// threads and the message's reference (table chat_logins).
type logins struct {
	mu    sync.Mutex
	flows map[string]*loginFlow // by agent: what the relay last said
	dirty chan struct{}
}

// loginFlow is the relay's state for an agent, in memory only.
type loginFlow struct {
	link, showCode string
	expires        time.Time
	waiting        bool // a code is awaited
	noCode         bool // the login takes no code back
	done           relay.Outcome
	detail         string
}

func (f *FrontEnd) loginState() *logins {
	f.loginOnce.Do(func() {
		f.login = &logins{flows: map[string]*loginFlow{}, dirty: make(chan struct{}, 1)}
	})
	return f.login
}

func (l *logins) poke() {
	select {
	case l.dirty <- struct{}{}:
	default:
	}
}

// LoginUpdate takes what the relay says about a flow (relay.Relay.Notify).
func (f *FrontEnd) LoginUpdate(u relay.Update) {
	l := f.loginState()
	l.mu.Lock()
	fl := l.flows[u.Agent]
	if fl == nil || u.Link != "" {
		fl = &loginFlow{}
		l.flows[u.Agent] = fl
	}
	reply := ""
	switch {
	case u.Link != "":
		fl.link, fl.showCode, fl.expires, fl.waiting, fl.noCode = u.Link, u.ShowCode, u.Expires, !u.NoCode, u.NoCode
	case u.Refused != "":
		reply = "The code was refused: " + u.Refused
		fl.waiting = true
	case u.Done != "":
		fl.done, fl.detail, fl.waiting, fl.link, fl.showCode = u.Done, u.Detail, false, "", ""
		switch u.Done {
		case relay.LoggedIn:
			// Nothing to keep: the message says so once the outage closes,
			// and the next outage starts afresh.
			delete(l.flows, u.Agent)
			reply = "Logged in again."
		case relay.Failed:
			reply = "The login failed: " + u.Detail
		}
	}
	l.mu.Unlock()
	// How the flow ended, without anything secret, for the message after
	// a restart; a new link clears it.
	ended := string(u.Done)
	if u.Link != "" || u.Done == relay.Expired || u.Done == relay.Failed {
		if u.Link != "" {
			ended = ""
		}
		f.Outbox.Store.DB.ExecContext(context.WithoutCancel(f.context()),
			`UPDATE chat_logins SET ended = ? WHERE client = ? AND agent = ? AND closed = 0`, ended, f.Name, u.Agent)
	}
	l.poke()
	if reply != "" {
		f.postDirect(f.context(), reply)
	}
}

// ownerConv opens the owner's direct conversation on this service, at the
// owner's first id here.
func (f *FrontEnd) ownerConv(ctx context.Context) (surface.Conversation, error) {
	ids := f.People.IDs(f.People.Owner(), f.Name)
	if len(ids) == 0 {
		return surface.Conversation{}, errNoOwnerID
	}
	return f.Driver.OpenDirect(ctx, ids[0])
}

var errNoOwnerID = errors.New("the owner has no id on this service")

// postDirect posts text in the owner's direct conversation.
func (f *FrontEnd) postDirect(ctx context.Context, text string) {
	conv, err := f.ownerConv(ctx)
	if err == nil {
		_, err = f.Driver.Post(ctx, conv, "", surface.Message{Text: text})
	}
	if err != nil {
		f.Log.Warn("chat: the owner could not be reached", "err", err)
	}
}

// direct handles a direct message: from the owner, a code for the relay.
func (f *FrontEnd) direct(in surface.Inbound) {
	if in.Author.Kind == surface.Bot || !f.owner(in.Author) {
		if in.Author.Kind != surface.Bot {
			f.Log.Warn("direct message ignored", "reason", "not from the owner", "author", in.Author.ID)
		}
		return
	}
	l := f.loginState()
	l.mu.Lock()
	var agent string
	for name, fl := range l.flows {
		if fl.waiting {
			agent = name
		}
	}
	l.mu.Unlock()
	code := strings.TrimSpace(in.Text)
	switch {
	case agent == "":
		f.postDirect(f.context(), "Not waiting for a code.")
		return
	case code == "" || strings.IndexFunc(code, func(c rune) bool { return unicode.IsSpace(c) || !unicode.IsPrint(c) }) >= 0:
		f.postDirect(f.context(), "That does not look like a code: a code is one word without spaces.")
		return
	}
	if f.Relay == nil {
		f.postDirect(f.context(), "Not waiting for a code.")
		return
	}
	// The relay tells how the code went (a refusal, or the end).
	if err := f.Relay.Code(f.from(in.Author), agent, code); err != nil {
		f.Log.Warn("chat: the relay took no code", "agent", agent, "err", err)
		f.postDirect(f.context(), "The code was not taken: "+err.Error())
	}
}

// Choice handles a button: a question's option, a turn's Stop, a blocked
// start's choices, a closed thread's Reopen, or the login message's "New
// link". A press that
// does nothing writes one log line.
func (f *FrontEnd) Choice(s surface.Selection) {
	if rest, ok := strings.CutPrefix(s.Token, askToken); ok {
		f.press(s, rest)
		return
	}
	if rest, ok := strings.CutPrefix(s.Token, stopToken); ok {
		f.pressStop(s, rest)
		return
	}
	if rest, ok := strings.CutPrefix(s.Token, reopenToken); ok {
		// A reopen may clone a repository again: the driver's other
		// messages and buttons do not wait for it. A second press meanwhile
		// does nothing (f.pressing).
		f.bg.Add(1)
		go func() { defer f.bg.Done(); f.pressReopen(s, rest) }()
		return
	}
	if rest, ok := strings.CutPrefix(s.Token, blockedToken); ok {
		f.pressBlocked(s, rest)
		return
	}
	agent, ok := strings.CutPrefix(s.Token, "login:")
	reason := ""
	switch {
	case !ok:
		reason = "no button of the front end's"
	case !f.owner(s.Author):
		reason = "not the owner's"
	case f.Relay == nil:
		reason = "the login relay is off"
	}
	if reason != "" {
		f.Log.Warn("button ignored", "reason", reason, "client", f.Name, "id", s.Author.ID,
			"channel", s.Ref.Conv.Channel, "message", s.Ref.ID)
		return
	}
	if err := f.Relay.Start(f.context(), f.from(s.Author), agent); err != nil {
		f.Log.Warn("chat: a new sign-in link failed", "agent", agent, "err", err)
		f.postDirect(f.context(), "No new link: "+err.Error())
	}
}

// outage is one row of chat_logins.
type outage struct {
	id      int64
	agent   string
	ended   string // how the last relay flow ended, as stored
	threads []control.ThreadID
	dm      surface.MessageRef
	closed  bool
}

// loginEvent follows the login events and the turns that fail as logged
// out, and keeps chat_logins. It returns the lines for the threads when a
// login is back.
//
// Which threads it names (design, section 5.2): the owner's message about
// an outage lists the stopped threads this front end shows, those with a
// link here, so linked tells whether e's thread has one. When the login is
// back, every link here of each stopped thread gets the line.
func (f *FrontEnd) loginEvent(ctx context.Context, e control.Event, linked bool) ([]Row, error) {
	var d struct {
		Agent   string
		Outage  int64
		Outcome string
		Reason  string
		Threads []json.RawMessage // numbers; {"scope", "key"} in an event from before thread numbers
	}
	switch e.Kind {
	case "turn_ended":
		if !linked {
			return nil, nil
		}
	case "login_lost", "login_restored":
	default:
		return nil, nil
	}
	json.Unmarshal(e.Data, &d)
	// The conversations of the stopped threads, looked up before the
	// transaction: the store has one connection, which it holds.
	var convs [][]surface.Conversation
	if e.Kind == "login_restored" {
		for _, raw := range d.Threads {
			cs, err := f.stoppedConvs(ctx, raw)
			if err != nil {
				return nil, err
			}
			convs = append(convs, cs)
		}
	}
	st := f.Outbox.Store
	var rows []Row
	err := st.Update(ctx, func(tx *store.Tx) error {
		switch e.Kind {
		case "login_lost":
			// The outage is the owner's to know of; the thread is listed
			// only when it is shown here.
			listed := []control.ThreadID{}
			if linked {
				listed = append(listed, e.Thread)
			}
			b, _ := json.Marshal(listed)
			_, err := tx.ExecContext(ctx, `INSERT INTO chat_logins (client, outage, agent, threads) VALUES (?, ?, ?, ?)
				ON CONFLICT DO NOTHING`, f.Name, d.Outage, d.Agent, string(b))
			return err
		case "turn_ended":
			if d.Outcome != "failure" || d.Reason != "login" {
				return nil
			}
			// A thread that stops while an outage is open joins it.
			os, err := openOutages(ctx, tx, f.Name)
			for _, o := range os {
				if !contains(o.threads, e.Thread) {
					b, _ := json.Marshal(append(o.threads, e.Thread))
					if _, err := tx.ExecContext(ctx, `UPDATE chat_logins SET threads = ? WHERE client = ? AND outage = ?`,
						string(b), f.Name, o.id); err != nil {
						return err
					}
				}
			}
			return err
		default: // login_restored
			// The outage is over, and with it what the relay said about it:
			// the next one starts afresh. (Not at login_lost: the relay may
			// have sent the new link before this reader gets there.)
			l := f.loginState()
			l.mu.Lock()
			delete(l.flows, d.Agent)
			l.mu.Unlock()
			if _, err := tx.ExecContext(ctx, `UPDATE chat_logins SET closed = 1 WHERE client = ? AND outage = ?`,
				f.Name, d.Outage); err != nil {
				return err
			}
			for i, cs := range convs {
				for _, conv := range cs {
					rows = append(rows, Row{Event: e.Pos, Part: i, Conv: conv, Message: surface.Message{
						Text: "The agent is logged in again. Send your last message again to go on."}})
				}
			}
			return nil
		}
	})
	if err == nil {
		f.loginState().poke()
	}
	return rows, err
}

// stoppedConvs returns the conversations, here, of one thread a
// login_restored event names: its number, or its scope and key in an event
// from before thread numbers.
// It is shown only through the thread's links here: none when it has none.
// Call it outside a store transaction.
func (f *FrontEnd) stoppedConvs(ctx context.Context, raw json.RawMessage) ([]surface.Conversation, error) {
	var id control.ThreadID
	if json.Unmarshal(raw, &id) != nil {
		var old struct{ Scope, Key string }
		if json.Unmarshal(raw, &old) != nil {
			return nil, nil
		}
		if _, ours := f.Scopes[old.Scope]; !ours {
			return nil, nil
		}
		err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT id FROM threads WHERE scope = ? AND key = ? ORDER BY id LIMIT 1`,
			old.Scope, old.Key).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return f.convsOf(ctx, id)
}

func contains(ts []control.ThreadID, t control.ThreadID) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func openOutages(ctx context.Context, q queryer, client string) ([]outage, error) {
	return outages(ctx, q, client, `AND closed = 0`)
}

func outages(ctx context.Context, q queryer, client, where string) ([]outage, error) {
	rows, err := q.QueryContext(ctx, `SELECT outage, agent, threads, dm_channel, dm_thread, dm_ref, closed, ended FROM chat_logins
		WHERE client = ? `+where+` ORDER BY outage`, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outage
	for rows.Next() {
		var o outage
		var threads string
		if err := rows.Scan(&o.id, &o.agent, &threads, &o.dm.Conv.Channel, &o.dm.Conv.Thread, &o.dm.ID, &o.closed, &o.ended); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(threads), &o.threads)
		out = append(out, o)
	}
	return out, rows.Err()
}

// runLogins keeps the owner's messages in step with the outages and the
// relay until ctx ends, trying again every retry after a failure.
func (f *FrontEnd) runLogins(ctx context.Context) {
	l := f.loginState()
	retry := f.Outbox.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	shown := map[int64]string{} // what each message shows now, as last edited
	unreachable := map[int64]bool{}
	for {
		failed := false
		os, err := outages(ctx, f.Outbox.Store.DB, f.Name, `AND (closed = 0 OR dm_ref = '' OR done = 0)`)
		if err != nil && ctx.Err() == nil {
			f.Log.Error("chat: reading login outages", "err", err)
			failed = true
		}
		for _, o := range os {
			m := f.loginMessage(ctx, o)
			if err := f.showOutage(ctx, &o, m, shown); err != nil {
				failed = true
				if !unreachable[o.id] {
					unreachable[o.id] = true
					f.Log.Warn("chat: the owner could not be reached", "outage", o.id, "err", err)
				}
				continue
			}
			delete(unreachable, o.id)
			if o.closed {
				f.Outbox.Store.DB.ExecContext(ctx, `UPDATE chat_logins SET done = 1 WHERE client = ? AND outage = ?`, f.Name, o.id)
			}
		}
		var again <-chan time.Time
		if failed {
			again = time.After(retry)
		}
		select {
		case <-ctx.Done():
			return
		case <-l.dirty:
		case <-again:
		}
	}
}

// showOutage posts the outage's message, once, or edits it to show m.
func (f *FrontEnd) showOutage(ctx context.Context, o *outage, m surface.Message, shown map[int64]string) error {
	key := m.Text + "\x00" + fmt.Sprint(len(m.Choices))
	if o.dm.ID == "" {
		conv, err := f.ownerConv(ctx)
		if err != nil {
			return err
		}
		ref, err := f.Driver.Post(ctx, conv, "login."+strconv.FormatInt(o.id, 10), m)
		if err != nil {
			return err
		}
		o.dm = ref
		shown[o.id] = key
		// Kept even when ctx ends now: the message is posted.
		_, err = f.Outbox.Store.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE chat_logins SET dm_channel = ?, dm_thread = ?, dm_ref = ?
			WHERE client = ? AND outage = ?`, ref.Conv.Channel, ref.Conv.Thread, ref.ID, f.Name, o.id)
		return err
	}
	if shown[o.id] == key {
		return nil
	}
	if err := f.Driver.Edit(ctx, o.dm, m); err != nil {
		return err
	}
	shown[o.id] = key
	return nil
}

// loginMessage is what the owner's message about an outage shows.
func (f *FrontEnd) loginMessage(ctx context.Context, o outage) surface.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "%s on %s is logged out.\n", o.agent, f.Instance)
	fmt.Fprintf(&b, "Stopped threads (%d):\n", len(o.threads))
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpRead})
	for i, t := range o.threads {
		if i == maxListed {
			fmt.Fprintf(&b, "…and %d more.\n", len(o.threads)-maxListed)
			break
		}
		name := fmt.Sprintf("#%d", t)
		if d, err := api.Thread(ctx, t, 0, 0); err == nil {
			name = fmt.Sprintf("#%d %s", d.ID, d.Title)
		}
		fmt.Fprintf(&b, "- %s\n", name)
	}
	m := surface.Message{}
	button := []surface.Choice{{Label: "New link", Token: "login:" + o.agent}}
	l := f.loginState()
	l.mu.Lock()
	fl := l.flows[o.agent]
	flow := loginFlow{done: relay.Outcome(o.ended)} // after a restart, what was stored
	if fl != nil {
		flow = *fl
	}
	l.mu.Unlock()
	switch {
	case o.closed:
		b.WriteString("Logged in again.")
	case f.Relay == nil:
		b.WriteString("Log the agent in again at the host.")
	case flow.link != "":
		b.WriteString("Sign in at " + flow.link + "\n")
		if flow.showCode != "" {
			b.WriteString("Code to enter there: " + flow.showCode + "\n")
		}
		if flow.noCode {
			b.WriteString("Nothing to send back here: the login ends by itself once you have signed in, until " + flow.expires.UTC().Format("15:04 UTC") + ".")
		} else {
			b.WriteString("Then send the code you get here, until " + flow.expires.UTC().Format("15:04 UTC") + ".")
		}
		m.Choices = button
	case flow.done == relay.Expired:
		b.WriteString("The sign-in link has expired. Press the button for a new one.")
		m.Choices = button
	case flow.done == relay.Failed:
		b.WriteString("The login failed: " + flow.detail + "\nPress the button to try again.")
		m.Choices = button
	default:
		b.WriteString("Press the button for a sign-in link.")
		m.Choices = button
	}
	m.Text = strings.TrimRight(b.String(), "\n")
	return m
}

// scopeList returns the scopes whose threads this front end may show: its
// own, and those whose show_in names one of its channels.
func (f *FrontEnd) scopeList() []string {
	var s []string
	for k := range f.Scopes {
		s = append(s, k)
	}
	for _, shown := range f.ShowIn {
		for _, k := range shown {
			if !slices.Contains(s, k) {
				s = append(s, k)
			}
		}
	}
	sort.Strings(s)
	return s
}

// scopesIn returns the scopes whose threads may be shown in channel: its
// own scope, if it is one of this front end's, and those whose show_in
// names it.
func (f *FrontEnd) scopesIn(channel string) []string {
	var s []string
	if _, own := f.Scopes[channel]; own {
		s = append(s, channel)
	}
	return append(s, f.ShowIn[channel]...)
}
