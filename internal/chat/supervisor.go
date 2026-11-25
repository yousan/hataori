package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// A supervisor's thread (design, section 7.1, "What it posts") is a
// conversation, so the front end posts in it differently from a worker
// thread: a completed turn pings nobody (#639); a turn that a wake started
// posts its final answer only, and nothing for "(quiet)"; neither does a
// turn that answers messages to others only (#799); and a person is
// mentioned only where the agent wrote "@<name>" (#651). The front end
// tells such a thread by its being confined: only a supervisor's thread is
// started confined, and a thread's confinement never changes.

// svThread is what the front end looked up about a thread.
type svThread struct {
	sv    bool   // a supervisor's thread
	scope string // its scope
}

// svThread returns whether thread is a supervisor's, and its scope,
// remembering the answer in r. A thread the store does not have is none.
func (f *FrontEnd) svThread(ctx context.Context, r *reader, thread control.ThreadID) (svThread, error) {
	if t, ok := r.sv[thread]; ok {
		return t, nil
	}
	var t svThread
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT confined, scope FROM threads WHERE id = ?`, int64(thread)).Scan(&t.sv, &t.scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return svThread{}, fmt.Errorf("chat: whether thread %d is a supervisor's: %w", thread, err)
	}
	if r.sv == nil {
		r.sv = map[control.ThreadID]svThread{}
	}
	r.sv[thread] = t
	return t, nil
}

// wakeKeyPrefix starts the key of every wake the daemon gives a
// supervisor's thread, a reminder's included (supervisor.WakeKeyPrefix).
const wakeKeyPrefix = "hataori-wake-"

// quietAnswer is the final answer that posts nothing, of a wake's turn and
// of one that answers messages to others only.
const quietAnswer = "(quiet)"

// woke reports whether a turn with key was started by a wake.
func woke(key string) bool { return strings.HasPrefix(key, wakeKeyPrefix) }

// heard is what the open turn of a supervisor's thread answers.
type heard struct {
	input bool // it was given one
	// others is whether each input it was given mentions an account, and
	// not hataori's own (toOthers).
	others bool
	// texts are the texts of it that wait for its final answer, which says
	// whether they are posted.
	texts []control.Event
}

// hear follows e, an event of a supervisor's thread, for r.heard. What it
// keeps comes from the events alone, so it is the same after a restart.
func (r *reader) hear(e control.Event) {
	if !r.sv[e.Thread].sv {
		return
	}
	h := r.heard[e.Thread]
	switch {
	case e.Kind == "turn_started":
		r.heard[e.Thread] = &heard{}
	case h == nil:
	case e.Kind == "turn_ended":
		delete(r.heard, e.Thread)
	default:
		var d eventData
		json.Unmarshal(e.Data, &d)
		switch {
		case e.Kind == "input", e.Kind == "answer":
			h.others = (h.others || !h.input) && r.toOthers != nil && r.toOthers(d.Link, d.Text)
			h.input = true
			if !h.others {
				h.texts = nil // posted now
			}
		case e.Kind == "text" && strings.TrimSpace(d.Text) != "":
			if h.others && !d.Final {
				h.texts = append(h.texts, e)
			} else {
				h.texts = nil // posted with it, or not at all
			}
		case e.Kind == "request" && !d.Approval:
			h.texts = nil // posted before it
		}
	}
}

// toOthers reports whether text, of an input that came through l, mentions
// an account and not hataori's own there. What came through no link, such
// as the dashboard's, mentions nobody.
func (f *FrontEnd) toOthers(l *control.LinkRef, text string) bool {
	if l == nil {
		return false
	}
	d := f.Driver
	if l.Client != f.Name {
		if f.Drivers == nil {
			return false
		}
		d = f.Drivers(l.Client)
	}
	m, ok := d.(surface.Mentioner)
	return ok && m.MentionsOthers(text)
}

// rowsOf returns the rows of e as rowsFor does, except where it is part of
// a supervisor's turn that answers messages to others only (#799): what
// such a turn says before its final answer waits for that, and is posted
// before it, or neither is when that answer is "(quiet)". A turn that ends
// without one, as with a question, posts what waited.
func (f *FrontEnd) rowsOf(ctx context.Context, r *reader, e control.Event, convs []surface.Conversation) ([]Row, error) {
	sv, err := f.svThread(ctx, r, e.Thread)
	if err != nil {
		return nil, err
	}
	if !sv.sv {
		return f.rowsFor(ctx, r, e, convs)
	}
	switch e.Kind {
	case "input", "answer", "text", "request", "turn_ended":
		// The turn's earlier events, when this reader has not read them.
		if _, _, err := r.turnKey(ctx, e); err != nil {
			return nil, err
		}
	}
	h := r.heard[e.Thread]
	var waited []control.Event
	if h != nil {
		waited = h.texts
	}
	r.hear(e)
	if h != nil && h.others && e.Kind == "text" {
		var d eventData
		json.Unmarshal(e.Data, &d)
		if !d.Final || strings.TrimSpace(d.Text) == quietAnswer {
			return nil, nil
		}
	}
	if r.heard[e.Thread] == h && h != nil && len(h.texts) > 0 {
		waited = nil // it waits on
	}
	var rows []Row
	for _, t := range waited {
		var d eventData
		json.Unmarshal(t.Data, &d)
		notify, _, err := f.svPost(ctx, r, t, sv, false, d.Text)
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{Event: t.Pos, Conv: convs[0], Message: surface.Message{Text: d.Text, Quiet: len(notify) == 0, Notify: notify}})
	}
	own, err := f.rowsFor(ctx, r, e, convs)
	return append(rows, own...), err
}

// named returns the names of the people that text names with "@<name>",
// where name is a person who may act in scope: the owner, or one of the
// scope's people, in the order they are first named. A name that is no
// such person names nobody.
func (f *FrontEnd) named(scope, text string) []string {
	if !strings.Contains(text, "@") {
		return nil
	}
	names := f.People.Of(scope)
	// The longest name that fits wins: "@ann.smith" names ann.smith, not
	// also ann.
	slices.SortStableFunc(names, func(a, b string) int { return len(b) - len(a) })
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if before, _ := utf8.DecodeLastRuneInString(text[:i]); i > 0 && nameRune(before) {
			continue // as in an email address
		}
		for _, name := range names {
			end := i + 1 + len(name)
			if !strings.HasPrefix(text[i+1:], name) {
				continue
			}
			if after, _ := utf8.DecodeRuneInString(text[end:]); end < len(text) && nameRune(after) {
				continue // a longer word
			}
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
			break
		}
	}
	return out
}

// ids returns the ids, on this front end's service, of the people names;
// a person with none here is mentioned nowhere.
func (f *FrontEnd) ids(names []string) []string {
	var out []string
	for _, name := range names {
		for _, id := range f.People.IDs(name, f.Name) {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// nameRune reports whether r can be part of a name next to "@<name>".
func nameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// Posts is what the front end does with the posts of a scope's supervisor
// (design, section 7.1, "What it posts"; #657), from the keys of
// scopes[].supervisor. The zero Posts posts every wake's answer and
// mentions whoever is named.
type Posts struct {
	// Report is which final answers of wakes are posted: "every" (also
	// ""), "needed" (those that name a person) or "digest" (those that
	// name a person at once, the others together once per Every).
	Report string
	Every  time.Duration
	// Never makes no post mention anyone.
	Never bool
	// Quiet is the daily span during which no post mentions anyone; nil
	// for none.
	Quiet *QuietHours
}

// QuietHours is a daily span: From and To are minutes after midnight in
// Zone, and a span with From after To goes across midnight.
type QuietHours struct {
	From, To int
	Zone     *time.Location
}

// in reports whether t is in the span.
func (q *QuietHours) in(t time.Time) bool {
	if q == nil {
		return false
	}
	l := t.In(q.Zone)
	m := l.Hour()*60 + l.Minute()
	if q.From < q.To {
		return m >= q.From && m < q.To
	}
	return m >= q.From || m < q.To
}

// end returns when the span that holds t ends: the first minute after t
// that in does not hold. It goes by in, a minute at a time, so that it
// agrees with in also where a clock change skips or repeats the wall
// time of To.
func (q *QuietHours) end(t time.Time) time.Time {
	e := t.Truncate(time.Minute)
	for i := 0; i < 2*24*60; i++ {
		if e = e.Add(time.Minute); !q.in(e) {
			return e
		}
	}
	return e
}

// postsOf returns the keys of scope's supervisor; the zero Posts for a
// scope this front end does not own.
func (f *FrontEnd) postsOf(scope string) Posts { return f.Supervisors[scope] }

// heldPost is a wake's final answer held for a digest; namedPost is a
// person named during quiet hours.
type heldPost struct {
	thread control.ThreadID
	pos    uint64
	at     time.Time
	text   string
}

type namedPost struct {
	thread control.ThreadID
	end    time.Time
	name   string
	pos    uint64
}

// svPost decides a post of e in supervisor thread sv, with text: whom it
// mentions, and whether it is posted now. answer marks a wake's final
// answer, which the scope's report may hold back: under "needed" one
// that names nobody is not posted, under "digest" it is held for the next
// digest. The person a post names is mentioned unless mention is "never";
// during quiet hours nobody is, and the name is counted for the message at
// their end. Quiet hours go by the front end's clock when it writes the
// post, not by the event's time, which a restart may have left behind.
func (f *FrontEnd) svPost(ctx context.Context, r *reader, e control.Event, sv svThread, answer bool, text string) ([]string, bool, error) {
	p := f.postsOf(sv.scope)
	names := f.named(sv.scope, text)
	if answer && len(names) == 0 {
		switch p.Report {
		case "needed":
			return nil, false, nil
		case "digest":
			r.held = append(r.held, heldPost{thread: e.Thread, pos: e.Pos, at: e.Time, text: text})
			return nil, false, nil
		}
	}
	switch {
	case p.Never || len(names) == 0:
		return nil, true, nil
	case p.Quiet.in(f.now()):
		end := p.Quiet.end(f.now())
		for _, n := range names {
			r.named = append(r.named, namedPost{thread: e.Thread, end: end, name: n, pos: e.Pos})
		}
		return nil, true, nil
	}
	return f.ids(names), true, nil
}

// quietNow reports whether no post of supervisor thread sv may mention
// anyone at t: mention is "never", or t is in its quiet hours.
func (f *FrontEnd) quietNow(sv svThread, t time.Time) bool {
	p := f.postsOf(sv.scope)
	return p.Never || p.Quiet.in(t)
}

// holdTx writes what svPost held back of the event just read, in the
// transaction that moves the cursor past it.
func (f *FrontEnd) holdTx(ctx context.Context, tx *store.Tx, r *reader) error {
	for _, h := range r.held {
		// The answer joins the digest its thread has waiting, if that is
		// not yet due by the answer's time: after a restart, an answer
		// past it starts a digest of its own.
		due := h.at.Add(f.postsOf(r.sv[h.thread].scope).every()).UnixMilli()
		var open sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(due_at) FROM chat_held WHERE client = ? AND thread = ? AND due_at > ?`,
			f.Name, int64(h.thread), h.at.UnixMilli()).Scan(&open); err != nil {
			return err
		}
		if open.Valid {
			due = open.Int64
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_held (client, thread, due_at, pos, at, text) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`, f.Name, int64(h.thread), due, int64(h.pos), h.at.UnixMilli(), h.text); err != nil {
			return err
		}
	}
	for _, n := range r.named {
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_named (client, thread, end_at, name, times, pos) VALUES (?, ?, ?, ?, 1, ?)
			ON CONFLICT (client, thread, end_at, name) DO UPDATE SET times = times + 1, pos = max(pos, excluded.pos)`,
			f.Name, int64(n.thread), n.end.UnixMilli(), n.name, int64(n.pos)); err != nil {
			return err
		}
	}
	return nil
}

func (p Posts) every() time.Duration {
	if p.Every > 0 {
		return p.Every
	}
	return time.Hour
}

// The parts of the rows that post a digest and the message at the end of
// quiet hours, far from the parts of what an event shows otherwise.
const (
	digestPart   = lampPart + 3
	quietEndPart = lampPart + 4
)

// flushHeld posts what is due at now: each digest, as one message of the
// answers it holds, and the message at the end of quiet hours that
// mentions each person named during them. Each is written with the
// removal of what it posts, so it is posted once, also across restarts.
// It runs in the show loop, which alone writes what it reads, so nothing
// is held between the look-up below and the transaction.
func (f *FrontEnd) flushHeld(ctx context.Context, now time.Time) error {
	at := now.UnixMilli()
	// The conversations of the threads with something due, looked up
	// before the transaction, which the look-up would wait on.
	rs, err := f.Outbox.Store.DB.QueryContext(ctx, `SELECT thread FROM chat_held WHERE client = ? AND due_at <= ?
		UNION SELECT thread FROM chat_named WHERE client = ? AND end_at <= ?`, f.Name, at, f.Name, at)
	if err != nil {
		return err
	}
	var threads []control.ThreadID
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return err
		}
		threads = append(threads, control.ThreadID(id))
	}
	if err := rs.Close(); err != nil || len(threads) == 0 {
		return err
	}
	convs := map[control.ThreadID][]surface.Conversation{}
	scopes := map[control.ThreadID]string{}
	for _, id := range threads {
		if convs[id], err = f.convsOf(ctx, id); err != nil {
			return err
		}
		var scope string
		err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT scope FROM threads WHERE id = ?`, int64(id)).Scan(&scope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		scopes[id] = scope
	}
	// One row for each conversation, the first first; the copies mention
	// nobody (everyLink). A thread shown nowhere here gets nothing.
	every := func(id control.ThreadID, row Row) []Row {
		if len(convs[id]) == 0 {
			return nil
		}
		row.Conv = convs[id][0]
		return everyLink([]Row{row}, convs[id])
	}
	var rows []Row
	err = f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
		rows = nil
		digests, err := f.dueDigests(ctx, tx, at)
		if err != nil {
			return err
		}
		for _, d := range digests {
			rows = append(rows, every(d.thread, Row{Event: d.pos, Part: digestPart, Message: surface.Message{Text: d.text}})...)
		}
		ends, err := f.dueQuietEnds(ctx, tx, at)
		if err != nil {
			return err
		}
		for _, q := range ends {
			// Told at the end of quiet hours, also late after a restart, but
			// not during the next ones: then at their end. Under mention
			// "never" it mentions nobody.
			p := f.postsOf(scopes[q.thread])
			if p.Quiet.in(now) {
				continue
			}
			if p.Never {
				q.notify = nil
			}
			rows = append(rows, every(q.thread, Row{Event: q.pos, Part: quietEndPart, Message: surface.Message{Text: q.text, Notify: q.notify}})...)
			if _, err := tx.ExecContext(ctx, `DELETE FROM chat_named WHERE client = ? AND thread = ? AND end_at = ?`,
				f.Name, int64(q.thread), q.end); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_held WHERE client = ? AND due_at <= ?`, f.Name, at); err != nil {
			return err
		}
		return f.Outbox.WriteTx(ctx, tx, rows...)
	})
	if err == nil && len(rows) > 0 {
		f.Outbox.Wake()
	}
	return err
}

type dueDigest struct {
	thread control.ThreadID
	pos    uint64
	text   string
}

// dueDigests returns the digests due by now, each one message.
func (f *FrontEnd) dueDigests(ctx context.Context, tx *store.Tx, now int64) ([]dueDigest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT thread, due_at, pos, at, text FROM chat_held WHERE client = ? AND due_at <= ?
		ORDER BY thread, due_at, pos`, f.Name, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		thread int64
		due    int64
	}
	var out []dueDigest
	var answers [][]string
	var last key
	for rows.Next() {
		var k key
		var pos, at int64
		var text string
		if err := rows.Scan(&k.thread, &k.due, &pos, &at, &text); err != nil {
			return nil, err
		}
		if len(out) == 0 || k != last {
			out = append(out, dueDigest{thread: control.ThreadID(k.thread)})
			answers = append(answers, nil)
			last = k
		}
		out[len(out)-1].pos = uint64(pos)
		answers[len(answers)-1] = append(answers[len(answers)-1], time.UnixMilli(at).In(f.zone()).Format("15:04")+"\n"+text)
	}
	for i := range out {
		n := len(answers[i])
		head := fmt.Sprintf("%d reports held since the last digest:", n)
		if n == 1 {
			head = "1 report held since the last digest:"
		}
		out[i].text = head + "\n\n" + strings.Join(answers[i], "\n\n")
	}
	return out, rows.Err()
}

type dueQuietEnd struct {
	thread control.ThreadID
	end    int64 // unix milliseconds
	pos    uint64
	text   string
	notify []string
}

// dueQuietEnds returns the messages at the ends of quiet hours due by now:
// one per thread and end, naming each person named during them, with how
// many times, and mentioning them.
func (f *FrontEnd) dueQuietEnds(ctx context.Context, tx *store.Tx, now int64) ([]dueQuietEnd, error) {
	rows, err := tx.QueryContext(ctx, `SELECT thread, end_at, name, times, pos FROM chat_named WHERE client = ? AND end_at <= ?
		ORDER BY thread, end_at, name`, f.Name, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		thread int64
		end    int64
	}
	var out []dueQuietEnd
	var parts [][]string
	var names [][]string
	var last key
	for rows.Next() {
		var k key
		var name string
		var times, pos int64
		if err := rows.Scan(&k.thread, &k.end, &name, &times, &pos); err != nil {
			return nil, err
		}
		if len(out) == 0 || k != last {
			out = append(out, dueQuietEnd{thread: control.ThreadID(k.thread), end: k.end})
			parts, names = append(parts, nil), append(names, nil)
			last = k
		}
		i := len(out) - 1
		out[i].pos = max(out[i].pos, uint64(pos))
		n := "once"
		if times > 1 {
			n = fmt.Sprintf("%d times", times)
		}
		parts[i] = append(parts[i], fmt.Sprintf("@%s %s", name, n))
		names[i] = append(names[i], name)
	}
	for i := range out {
		out[i].text = "Quiet hours are over. Named during them: " + strings.Join(parts[i], ", ") + "."
		out[i].notify = f.ids(names[i])
	}
	return out, rows.Err()
}

// zone is the time zone of the front end's clock.
func (f *FrontEnd) zone() *time.Location { return f.now().Location() }
