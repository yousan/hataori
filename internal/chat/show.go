package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// reader is what the front end remembers about threads while it reads
// events. It is rebuilt from the threads' events when it is missing.
type reader struct {
	api   control.API
	first map[control.ThreadID]uint64 // position of each thread's first event
	// session is each thread's first session, with first.
	session map[control.ThreadID]int64
	turn    map[control.ThreadID]string               // key of the call that started the open turn
	by      map[control.ThreadID]string               // who started the open turn: caller, agent or terminal
	via     map[control.ThreadID]*control.LinkRef     // the link the open turn's first input came through; nil for none
	quiet   map[control.ThreadID]*quiet               // open turns, for the progress line
	gone    map[control.ThreadID]*RowKey              // the last delete of a progress line
	known   map[control.ThreadID]bool                 // threads whose earlier events were read
	subs    map[control.ThreadID]map[string]*subagent // running subagents, by id
	// lamp is each thread's lamps (lamps.go); lampKnown, the threads whose
	// earlier turn events were read for it.
	lamp      map[control.ThreadID]*lampState
	lampKnown map[control.ThreadID]bool
	// sv holds whether each thread looked up is a supervisor's
	// (FrontEnd.supervisors); made when first needed.
	sv map[control.ThreadID]svThread
	// heard is what each supervisor's thread's open turn answers (hear).
	heard map[control.ThreadID]*heard
	// toOthers reports whether the text of an input through a link
	// mentions others only (FrontEnd.toOthers).
	toOthers func(l *control.LinkRef, text string) bool
	// held and named are what svPost held back of the event being read,
	// written with the cursor (holdTx).
	held  []heldPost
	named []namedPost
	// outbox and ctx are the front end's, for looking up rows.
	outbox *Outbox
	ctx    context.Context
	// quietAfter is FrontEnd.QuietAfter.
	quietAfter time.Duration
	// conv is the conversation a thread is shown in here (FrontEnd.convOf),
	// looked up before the thread's events are read.
	conv func(control.ThreadID) surface.Conversation
}

// show reads the events of the front end's scopes, from where it stopped
// before, and writes what to post into the outbox, until ctx ends.
func (f *FrontEnd) show(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the subscription when reading stops on an error
	scopes := f.scopeList()
	r := &reader{
		api:      f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: scopes, May: control.OpRead}),
		first:    map[control.ThreadID]uint64{},
		session:  map[control.ThreadID]int64{},
		turn:     map[control.ThreadID]string{},
		by:       map[control.ThreadID]string{},
		via:      map[control.ThreadID]*control.LinkRef{},
		quiet:    map[control.ThreadID]*quiet{},
		gone:     map[control.ThreadID]*RowKey{},
		known:    map[control.ThreadID]bool{},
		heard:    map[control.ThreadID]*heard{},
		toOthers: f.toOthers,
		subs:     map[control.ThreadID]map[string]*subagent{},

		lamp:      map[control.ThreadID]*lampState{},
		lampKnown: map[control.ThreadID]bool{},

		outbox: f.Outbox,
		ctx:    ctx,

		quietAfter: f.quietAfter(),
		conv: func(id control.ThreadID) surface.Conversation {
			c, _, _ := f.convOf(ctx, id)
			return c
		},
	}
	var after uint64
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT pos FROM chat_cursors WHERE client = ?`, f.Name).Scan(&after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("chat: reading the cursor: %w", err)
	}
	// The open turns, for their progress lines, as their events up to the
	// cursor tell; the events after it are read below.
	threads, err := r.api.Threads(ctx, control.Filter{})
	if err != nil {
		return err
	}
	for _, t := range threads {
		if len(f.linkConvs(t)) > 0 && t.State == control.Running {
			if _, _, err := f.convOf(ctx, t.ID); err != nil {
				return err
			}
			if err := r.catchUp(ctx, t.ID, after+1); err != nil {
				return err
			}
		}
	}
	evs, err := r.api.Events(ctx, control.Filter{}, after)
	if err != nil {
		return err
	}
	defer stopEvents(cancel, evs)
	every := f.Tick
	if every <= 0 {
		every = time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	caughtUp := false // lines are due only once the stored events are read
	for {
		var e control.Event
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if !caughtUp {
				continue
			}
			if rows := f.tick(ctx, r, f.now()); len(rows) > 0 {
				if err := f.Outbox.Write(ctx, rows...); err != nil {
					return err
				}
			}
			if err := f.flushHeld(ctx, f.now()); err != nil {
				return err
			}
			continue
		case ev, ok := <-evs:
			if !ok {
				return ctx.Err()
			}
			e = ev
		}
		if e.Kind == control.CaughtUp {
			caughtUp = true
			continue
		}
		if e.Thread == 0 {
			continue
		}
		// Shown in the conversations of the thread's links here, each its
		// own rows; a thread with none is not shown by this front end.
		convs, err := f.convsOf(ctx, e.Thread)
		if err != nil {
			return err
		}
		linked := len(convs) > 0
		var rows []Row
		if linked {
			if rows, err = f.rowsOf(ctx, r, e, convs); err != nil {
				return err
			}
			// A thread seen for the first time since the start: what its
			// earlier events tell about the progress line, first.
			if err := r.catchUp(ctx, e.Thread, e.Pos); err != nil {
				return err
			}
			rows = everyLink(append(append(r.track(e), rows...), r.subagents(e)...), convs)
			rows = append(rows, f.mirrorRows(e, convs)...)
			ar, err := f.archiveRows(ctx, e, convs)
			if err != nil {
				return err
			}
			rows = append(rows, ar...)
			wr, err := f.waitsRows(ctx, e, convs)
			if err != nil {
				return err
			}
			lr, err := f.lampRows(ctx, r, e, convs)
			if err != nil {
				return err
			}
			rows = append(append(rows, wr...), lr...)
		}
		lr, err := f.loginEvent(ctx, e, linked)
		if err != nil {
			return err
		}
		rows = append(rows, lr...)
		err = f.Outbox.Store.Update(ctx, func(tx *store.Tx) error {
			if err := f.Outbox.WriteTx(ctx, tx, rows...); err != nil {
				return err
			}
			if err := f.holdTx(ctx, tx, r); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO chat_cursors (client, pos) VALUES (?, ?)
				ON CONFLICT (client) DO UPDATE SET pos = excluded.pos`, f.Name, e.Pos)
			return err
		})
		if err != nil {
			return err
		}
		r.held, r.named = nil, nil
		if len(rows) > 0 {
			f.Outbox.Wake()
		}
	}
}

// eventData holds the fields of the event kinds the front end shows.
type eventData struct {
	State     string   `json:"state"`
	Machine   string   `json:"machine"`
	Pending   bool     `json:"pending"`
	Text      string   `json:"text"`
	Target    int64    `json:"target"`
	Principal string   `json:"principal"`
	Final     bool     `json:"final"`
	Approval  bool     `json:"approval"`
	Choices   []string `json:"choices"`
	Outcome   string   `json:"outcome"`
	Reason    string   `json:"reason"`
	Detail    string   `json:"detail"`
	Key       string   `json:"key"`
	Session   int64    `json:"session"`
	Turn      int64    `json:"turn"` // turn_started, turn_ended
	File      string   `json:"file"`
	More      []string `json:"more"`
	Caption   string   `json:"caption"`
	By        string   `json:"by"` // turn_started: who started the turn
	// input, answer: the link it came through, since 3.6
	Link *control.LinkRef `json:"link"`
	// request, answer, request_withdrawn: the request; answer since 3.8
	Request string `json:"request"`
	Multi   bool   `json:"multi"` // request: several choices at once, since 3.8
	// proposal_made, proposal_answered: since 3.26
	Number   int64  `json:"number"`
	Deferred int64  `json:"deferred"`
	Action   string `json:"action"`
	Repo     string `json:"repo"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Question string `json:"question"`
	Research bool   `json:"research"` // proposal_made: since 3.34
}

// proposalText is how a supervisor's proposal is put to a person: what it
// would do, where, with which model and effort, and its text.
func proposalText(d eventData) string {
	var with []string
	if d.Model != "" {
		with = append(with, "model "+d.Model)
	}
	if d.Effort != "" {
		with = append(with, "effort "+d.Effort)
	}
	if d.Machine != "" {
		with = append(with, "machine "+d.Machine)
	}
	what := fmt.Sprintf("start a thread in %s", d.Repo)
	if d.Research {
		what = fmt.Sprintf("start a read-only research thread, without the repository's settings, in %s", d.Repo)
	}
	if d.Action == "send" {
		what = fmt.Sprintf("send to thread %d", d.Target)
		if d.Deferred != 0 {
			what = fmt.Sprintf("queue deferred instruction %d for the same worker of thread %d", d.Deferred, d.Target)
		}
		if d.Question != "" {
			what += ", as the answer to its open question"
		}
	}
	if len(with) > 0 {
		what += " (" + strings.Join(with, ", ") + ")"
	}
	return strings.TrimSpace(fmt.Sprintf("Proposal %d of the supervisor: %s?\n%s", d.Number, what, d.Text))
}

// proposalDone is the line that says how a proposal was decided, and what
// came of it.
func proposalDone(d eventData) string {
	if d.Deferred != 0 {
		if d.Outcome == "run" {
			return fmt.Sprintf("Approved by %s: deferred instruction %d is queued for the same worker of thread %d; delivery is not yet confirmed.", d.Principal, d.Deferred, d.Target)
		}
		return fmt.Sprintf("Deferred instruction %d was not queued: proposal %d outcome %s. %s", d.Deferred, d.Number, d.Outcome, d.Reason)
	}
	not := "was not started"
	if d.Action == "send" {
		not = "was not sent"
	}
	switch d.Outcome {
	case "run":
		if d.Action == "send" {
			return fmt.Sprintf("Approved by %s: proposal %d was sent to thread %d.", d.Principal, d.Number, d.Target)
		}
		return fmt.Sprintf("Approved by %s: proposal %d started thread %d.", d.Principal, d.Number, d.Target)
	case "refused":
		return fmt.Sprintf("Refused by %s: proposal %d %s.", d.Principal, d.Number, not)
	case "busy":
		return fmt.Sprintf("Approved by %s, but thread %d was running a turn or waiting on a question: proposal %d %s.", d.Principal, d.Target, d.Number, not)
	case "ended":
		return fmt.Sprintf("Approved by %s, but the question of thread %d it answered was no longer open: proposal %d %s.", d.Principal, d.Target, d.Number, not)
	case "closed":
		return fmt.Sprintf("Approved by %s, but thread %d is closed: proposal %d %s.", d.Principal, d.Target, d.Number, not)
	}
	return fmt.Sprintf("Approved by %s, but proposal %d %s: %s.", d.Principal, d.Number, not, d.Reason)
}

// typedAtTerminal is the line that goes before the final answer of a turn a
// person typed at the agent's terminal (`hataori attach`), so that the chat
// thread shows the whole conversation and says where that turn came from.
const typedAtTerminal = "(typed at the terminal)"

// rowsFor returns the rows that show e. A kind it does not show gives none.
//
// The rows are for the thread's first conversation here, convs[0], and
// everyLink copies them to the others; a final answer's is for the
// conversation its turn's input came through, when that is one of convs.
func (f *FrontEnd) rowsFor(ctx context.Context, r *reader, e control.Event, convs []surface.Conversation) ([]Row, error) {
	conv := convs[0]
	switch e.Kind {
	case "input", "answer":
		var d eventData
		json.Unmarshal(e.Data, &d)
		if _, seen := r.via[e.Thread]; !seen && r.turn[e.Thread] != "" {
			r.via[e.Thread] = d.Link // the turn's first input or answer
		}
		if e.Kind == "answer" {
			return f.questionDone(ctx, r, e, conv, d.Request)
		}
		return nil, nil
	case "request_withdrawn":
		var d eventData
		json.Unmarshal(e.Data, &d)
		return f.questionDone(ctx, r, e, conv, d.Request)
	case "reopened":
		var d struct{ Workspace, Conversation string }
		json.Unmarshal(e.Data, &d)
		return []Row{{Event: e.Pos, Conv: conv, Message: surface.Message{Text: reopenedText(d.Workspace, d.Conversation)}}}, nil
	case "start_chosen":
		return f.chosenRows(ctx, r, e, conv)
	case "session_replaced":
		line, err := r.replacedLine(ctx, e)
		if err != nil || line == "" {
			return nil, err
		}
		return []Row{{Event: e.Pos, Conv: conv, Message: surface.Message{Text: line}}}, nil
	case "state":
		// A session that ended before it started, whenever that was: also
		// after the Start that made it returned (#589).
		sf, ok := startFailure(e)
		if !ok {
			return nil, nil
		}
		// The new session and its event are stored in the transaction of the
		// failure, so they are there when the failure is read.
		replaced := false
		err := r.scan(ctx, e.Thread, math.MaxUint64, func(ev control.Event) { replaced = replaced || sf.replacedBy(ev) })
		if err != nil || replaced {
			return nil, err
		}
		first, err := r.firstPos(ctx, e.Thread)
		if err != nil {
			return nil, err
		}
		if sf.session != r.session[e.Thread] {
			first = 0 // a later session's: the prompt is long posted
		} else if err := f.awaitPrompt(ctx, conv, first, e.Time); err != nil {
			return nil, err
		}
		repo, err := r.repoOf(ctx, e.Thread)
		if err != nil {
			return nil, err
		}
		return []Row{f.failedRow(sf, conv, first, repo)}, nil
	case "thread_created", "turn_started", "text", "request", "turn_ended", "file_out", "approval_asked", "approval_answered",
		"proposal_made", "proposal_answered", "input_failed", "input_waiting", "placement", "machine_action", "machine_availability":
	default:
		return nil, nil // a kind the front end does not show, or does not know
	}
	var d eventData
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &d); err != nil {
			f.Log.Warn("chat: an event that cannot be read", "pos", e.Pos, "kind", e.Kind, "err", err)
			return nil, nil
		}
	}
	row := func(m surface.Message) []Row { return []Row{{Event: e.Pos, Conv: conv, Message: m}} }
	switch e.Kind {
	case "input_waiting":
		return row(surface.Message{Text: "The message is waiting: " + d.Reason + "."}), nil
	case "input_failed":
		m := surface.Message{Text: "The input was not delivered: " + d.Reason + "."}
		if d.Text != "" {
			m.Text += "\n" + d.Text
		}
		if l := d.Link; l != nil && l.Client == f.Name && d.Key != "" && l.Channel == conv.Channel && l.Thread == conv.Thread {
			m.ReplyTo = &surface.MessageRef{Conv: conv, ID: d.Key}
		}
		return row(m), nil
	case "approval_asked":
		text := strings.TrimSpace(fmt.Sprintf("Interrupt thread %d? %s", d.Target, d.Reason))
		choices := []string{"Approve", "Refuse"}
		if !f.withButtons(choices, false) {
			return row(surface.Message{Text: text + "\nThis chat service shows no buttons, so this cannot be approved here."}), nil
		}
		return row(f.questionMessage(d.Request, text, choices, false)), nil
	case "approval_answered":
		var text string
		switch d.Outcome {
		case "interrupted":
			text = fmt.Sprintf("Approved by %s: thread %d's turn was interrupted, and it got the reason.", d.Principal, d.Target)
		case "ended":
			text = fmt.Sprintf("Approved by %s, but thread %d's turn had already ended: nothing was interrupted.", d.Principal, d.Target)
		case "refused":
			text = fmt.Sprintf("Refused by %s: thread %d was not interrupted.", d.Principal, d.Target)
		}
		return row(surface.Message{Text: text}), nil
	case "proposal_made":
		text := proposalText(d)
		choices := []string{"Approve", "Refuse"}
		if !f.withButtons(choices, false) {
			return row(surface.Message{Text: text + "\nThis chat service shows no buttons, so this cannot be approved here."}), nil
		}
		return row(f.questionMessage(d.Request, text, choices, false)), nil
	case "proposal_answered":
		return row(surface.Message{Text: proposalDone(d)}), nil
	case "placement":
		if d.Pending {
			return row(surface.Message{Text: "Waiting for machine placement: " + d.Reason}), nil
		}
		if d.Reason != "" {
			return row(surface.Message{Text: d.Reason}), nil
		}
		return row(surface.Message{Text: "Placed on machine " + d.Machine + "."}), nil
	case "machine_action":
		if d.State == "pending" {
			return row(surface.Message{Text: d.Reason}), nil
		}
		if d.State == "refused" {
			return row(surface.Message{Text: "Queued machine action refused: " + d.Reason}), nil
		}
		return nil, nil
	case "machine_availability":
		return nil, nil // one held-action notice per thread/absence reports waiting
	case "thread_created":
		r.first[e.Thread] = e.Pos
		r.session[e.Thread] = sessionOf(e)
	case "turn_started":
		r.turn[e.Thread] = d.Key
		r.by[e.Thread] = d.By
		delete(r.via, e.Thread)
	case "text":
		if strings.TrimSpace(d.Text) == "" {
			return nil, nil
		}
		sv, err := f.svThread(ctx, r, e.Thread)
		if err != nil {
			return nil, err
		}
		var notify []string
		if sv.sv {
			// A supervisor's thread: a wake's turn posts its final answer
			// only, and nothing for "(quiet)"; "@<name>" mentions (#651).
			key, _, err := r.turnKey(ctx, e)
			if err != nil {
				return nil, err
			}
			if woke(key) && (!d.Final || strings.TrimSpace(d.Text) == quietAnswer) {
				return nil, nil
			}
			// What the scope's keys make of it (#657): a wake's answer
			// may be held back, and a mention may not be made.
			var post bool
			if notify, post, err = f.svPost(ctx, r, e, sv, woke(key) && d.Final, d.Text); err != nil || !post {
				return nil, err
			}
		}
		if !d.Final {
			return row(surface.Message{Text: d.Text, Quiet: len(notify) == 0, Notify: notify}), nil
		}
		key, by, err := r.turnKey(ctx, e)
		if err != nil {
			return nil, err
		}
		// The reply goes where the turn's input was written; a link of
		// another client, or one this front end no longer has, gets none,
		// and so does input that came through no link, such as the
		// dashboard's, whose key is no message of the chat (#605).
		replies := true
		if l, seen := r.via[e.Thread]; seen && l == nil {
			replies = false
		} else if l != nil {
			c := surface.Conversation{Channel: l.Channel, Thread: l.Thread}
			if l.Client == f.Name && slices.Contains(convs, c) {
				conv = c
			} else {
				replies = false
			}
		}
		row = func(m surface.Message) []Row { return []Row{{Event: e.Pos, Conv: conv, Message: m}} }
		if by == "terminal" {
			return row(surface.Message{Text: typedAtTerminal + "\n" + d.Text, Notify: notify}), nil
		}
		rows := row(surface.Message{Text: d.Text, Notify: notify})
		if key == "" || !replies {
			return rows, nil // the agent started the turn by itself, or it came through no link here
		}
		trigger, err := f.isTrigger(ctx, conv.Channel, key)
		if err != nil {
			return nil, err
		}
		if !trigger {
			rows[0].Message.ReplyTo = &surface.MessageRef{Conv: conv, ID: key}
			return rows, nil
		}
		// The trigger is in the channel; in the thread its prompt stands
		// for it.
		first, err := r.firstPos(ctx, e.Thread)
		if err != nil {
			return nil, err
		}
		rows[0].ReplyToRow = &RowKey{Event: first, Part: 0}
		return rows, nil
	case "request":
		if d.Approval {
			return nil, nil // allowed by the core in phase 1; nothing to answer
		}
		m := f.questionMessage(d.Request, d.Text, d.Choices, d.Multi)
		sv, err := f.svThread(ctx, r, e.Thread)
		if err != nil {
			return nil, err
		}
		if sv.sv {
			if m.Notify, _, err = f.svPost(ctx, r, e, sv, false, d.Text); err != nil {
				return nil, err
			}
		}
		return row(m), nil
	case "file_out":
		return f.fileRows(ctx, e, conv, d.Session, append([]string{d.File}, d.More...), d.Caption), nil
	case "turn_ended":
		delete(r.turn, e.Thread)
		delete(r.by, e.Thread)
		delete(r.via, e.Thread)
		if line, handled, err := f.supervisorAvailability(ctx, r, e, d); err != nil || handled {
			if err != nil || line == "" {
				return nil, err
			}
			return row(surface.Message{Text: line}), nil
		}
		if d.Outcome == "failure" {
			who, err := f.stoppedBy(ctx, d.Outcome, d.Reason, d.Turn)
			if err != nil {
				return nil, err
			}
			if who != "" {
				return row(surface.Message{Text: "Stopped by " + who + "."}), nil
			}
			return row(surface.Message{Text: failureDetail(failureLine(d.Reason), d.Detail)}), nil
		}
	}
	return nil, nil
}

// promptWait bounds how long after a start failure the event reader
// waits for the trigger's start to write the prompt row.
var promptWait = 30 * time.Second

// awaitPrompt waits, while conv is the chat thread of a trigger or a
// command of this front end whose start has not yet written the prompt row
// at first, for that row: a failure read before it is posted after it. The
// cursor stays before the event meanwhile, so a restart reads it again.
// Past promptWait after the event at, the prompt is taken as lost, as after
// a daemon that died before writing it, and nothing is waited for.
func (f *FrontEnd) awaitPrompt(ctx context.Context, conv surface.Conversation, first uint64, at time.Time) error {
	var n int
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_triggers WHERE client = ? AND channel = ? AND thread = ?`,
		f.Name, conv.Channel, conv.Thread).Scan(&n)
	if err != nil || n == 0 {
		return err
	}
	for {
		state, _, err := f.Outbox.state(ctx, conv, RowKey{Event: first, Part: 0})
		if err != nil || state != "" || time.Since(at) > promptWait {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// question shows a question with its options numbered; the next message
// in the thread answers it.
func question(text string, choices []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	for i, c := range choices {
		if i == 0 && b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("\n" + strconv.Itoa(i+1) + ". " + c)
	}
	return b.String()
}

// stoppedBy returns the person whose Stop ended a turn that ended with
// outcome and reason: the stop interrupted it, or stopped its session when
// the interrupt did not end it in time. It is "" for every other end, a
// failure of the agent's own too, and for a turn no Stop was made for.
func (f *FrontEnd) stoppedBy(ctx context.Context, outcome, reason string, turn int64) (string, error) {
	if outcome != "failure" || (reason != "interrupted" && reason != "stopped") || turn == 0 {
		return "", nil
	}
	var who string
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT stopped_by FROM turns WHERE id = ?`, turn).Scan(&who)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return who, err
}

// failureLine is the one line a failed turn gets.
func failureLine(reason string) string {
	switch reason {
	case "unavailable":
		return "The turn failed: the provider is unavailable."
	case "limit":
		return "The turn failed: the usage limit was reached."
	case "login":
		return "The turn failed: the agent is not logged in."
	case "crashed":
		return "The turn failed: the agent crashed."
	case "":
		return "The turn failed: error."
	}
	return "The turn failed: error (" + reason + ")."
}

// isTrigger reports whether key is a trigger message this front end
// received in channel.
func (f *FrontEnd) isTrigger(ctx context.Context, channel, key string) (bool, error) {
	_, ok, err := f.stored(ctx, surface.MessageRef{Conv: surface.Conversation{Channel: channel}, ID: key})
	return ok, err
}

// turnKey returns the key of the call that started the turn e is in, and
// who started it.
func (r *reader) turnKey(ctx context.Context, e control.Event) (key, by string, err error) {
	if k, ok := r.turn[e.Thread]; ok {
		return k, r.by[e.Thread], nil
	}
	err = r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
		r.hear(ev)
		switch ev.Kind {
		case "turn_started":
			var d eventData
			json.Unmarshal(ev.Data, &d)
			r.turn[e.Thread] = d.Key
			r.by[e.Thread] = d.By
			delete(r.via, e.Thread)
		case "input", "answer":
			var d eventData
			json.Unmarshal(ev.Data, &d)
			if _, seen := r.via[e.Thread]; !seen {
				r.via[e.Thread] = d.Link
			}
		case "turn_ended":
			delete(r.turn, e.Thread)
			delete(r.by, e.Thread)
			delete(r.via, e.Thread)
		}
	})
	return r.turn[e.Thread], r.by[e.Thread], err
}

// replacedLine is the line e, a session_replaced event, gets: ReplacedLine,
// or, when a choice started the thread again after files blocked its start
// (#590), StartedAgainLine if the session blocked had run a turn, and so had
// a conversation, and no line if it had not: there is nothing to say beyond
// what chosenRows says.
func (r *reader) replacedLine(ctx context.Context, e control.Event) (string, error) {
	var d struct {
		Session int64
		Reason  string
	}
	json.Unmarshal(e.Data, &d)
	switch control.StartChoice(d.Reason) {
	case control.StartTrusted, control.StartWithoutSettings:
	default:
		return ReplacedLine, nil
	}
	ran := false
	err := r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
		ran = ran || ev.Kind == "turn_started" && sessionOf(ev) == d.Session
	})
	if err != nil || !ran {
		return "", err
	}
	return StartedAgainLine, nil
}

// repoOf returns the repository of thread id.
func (r *reader) repoOf(ctx context.Context, id control.ThreadID) (string, error) {
	d, err := r.api.Thread(ctx, id, 0, 0)
	return d.Repo, err
}

// firstPos returns the position of a thread's first event.
func (r *reader) firstPos(ctx context.Context, id control.ThreadID) (uint64, error) {
	if p, ok := r.first[id]; ok {
		return p, nil
	}
	out, err := startOutcome(ctx, r.api, id)
	if err == nil {
		r.first[id] = out.first
		r.session[id] = out.session
	}
	return out.first, err
}

// catchUp follows the thread's events before position before for the
// progress line, once per thread.
func (r *reader) catchUp(ctx context.Context, id control.ThreadID, before uint64) error {
	if r.known[id] {
		return nil
	}
	r.known[id] = true
	return r.scan(ctx, id, before, func(e control.Event) { r.track(e); r.subagents(e) })
}

// stopEvents ends a subscription to API.Events and waits until it has
// stopped: its channel is closed only once nothing is read for it any more.
// A caller that returns without waiting leaves the store in use, so it
// could be closed while a read is still going on.
func stopEvents(cancel context.CancelFunc, evs <-chan control.Event) {
	cancel()
	for range evs {
	}
}

// scan calls fn with the thread's stored events before position before.
func (r *reader) scan(ctx context.Context, id control.ThreadID, before uint64, fn func(control.Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := r.api.Events(ctx, control.Filter{Thread: id}, 0)
	if err != nil {
		return err
	}
	defer stopEvents(cancel, evs)
	for e := range evs {
		if e.Kind == control.CaughtUp || e.Pos >= before {
			return nil
		}
		fn(e)
	}
	return ctx.Err()
}
