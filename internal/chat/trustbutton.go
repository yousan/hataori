package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
)

// The choices of a start blocked at a start-up dialog that named files
// (#590): its "could not start" message says what to do next and, on a
// surface with buttons, offers the choices as buttons:
//
//  1. Start without this repository's settings: every launch of the thread
//     leaves the repository's own settings files out, so their hooks and
//     commands never run (StartWithoutSettings). Whoever may start a
//     thread in the scope may press it.
//  2. Trust and start: the files are trusted for the scope's threads on the
//     thread's repository, and the thread starts (Trust). The owner only.
//  3. Do not start: the thread is closed (CloseThread).
//
// A token names the thread, the state event and the choice, so it works
// after a restart. A press calls the core as the person who pressed; the
// core decides who may, and refuses everyone else, who sees nothing, as
// with any button. Once the core has taken a choice, made here or anywhere
// else, its start_chosen event takes the message's buttons away, in every
// link, and the message says who chose what.

// blockedToken starts a choice's token: trust:<thread>#<state event>:<choice>.
const blockedToken = "trust:"

// The choices, as their tokens end and as their buttons read.
const (
	choiceWithout = "without"
	choiceTrust   = "trust"
	choiceClose   = "close"

	labelWithout = "Start without this repository's settings"
	labelTrust   = "Trust and start"
	labelClose   = "Do not start"
)

func blockedChoice(sf startFailed, choice, label string) surface.Choice {
	return surface.Choice{Label: label, Token: fmt.Sprintf("%s%d#%d:%s", blockedToken, sf.thread, sf.pos, choice)}
}

// failedMessage is the "could not start" message of sf. A start blocked by
// files gets what to do next and, when buttons is set and the surface
// shows buttons, the choices.
func (f *FrontEnd) failedMessage(sf startFailed, repo string, buttons bool) surface.Message {
	m := surface.Message{Text: sf.text()}
	if len(sf.trust) == 0 {
		return m
	}
	files := strings.Join(sf.trust, ", ")
	config := `list ` + files + ` under "trust_files" of the scope "` + sf.scope + `" in hataori's config, and start again.`
	labels := []string{labelWithout, labelTrust, labelClose}
	if !buttons || !f.withButtons(labels, false) {
		m.Text += "\nWhat to do next: " + config
		return m
	}
	m.Text += "\nWhat to do next: choose below." +
		"\n- " + labelWithout + ": the thread runs without the repository's own settings files, so their hooks and commands never run; " +
		"it still reads the code and its instructions. Every later start of this thread does the same." +
		"\n- " + labelTrust + " (the owner only): trust " + files + " for this channel's threads on " + repo + ", and start." +
		"\n- " + labelClose + ": close this thread." +
		"\nOr " + config
	m.Choices = []surface.Choice{blockedChoice(sf, choiceWithout, labelWithout), blockedChoice(sf, choiceTrust, labelTrust),
		blockedChoice(sf, choiceClose, labelClose)}
	return m
}

// pressBlocked carries out the choice pressed on a blocked start's message,
// as the presser. A press that changes nothing writes one log line and
// shows nothing.
func (f *FrontEnd) pressBlocked(s surface.Selection, rest string) {
	ctx := f.context()
	ignored := func(reason string, err error) {
		f.Log.Warn("button ignored", "reason", reason, "client", f.Name, "id", s.Author.ID,
			"channel", s.Ref.Conv.Channel, "message", s.Ref.ID, "err", err)
	}
	at, choice, ok1 := strings.Cut(rest, ":")
	th, p, ok2 := strings.Cut(at, "#")
	n, err1 := strconv.ParseInt(th, 10, 64)
	pos, err2 := strconv.ParseUint(p, 10, 64)
	if !ok1 || !ok2 || err1 != nil || err2 != nil || pos == 0 {
		ignored("no button of the front end's", nil)
		return
	}
	thread := control.ThreadID(n)
	t, ok, err := f.threadHere(ctx, s.Ref.Conv)
	if err != nil || !ok || t.ID != thread {
		ignored("not a button of this thread", err)
		return
	}
	sf, ok, later, err := f.failureAt(ctx, t, pos)
	if err != nil || !ok || len(sf.trust) == 0 {
		ignored("no start blocked by files there", err)
		return
	}
	if later {
		// An old message's buttons act on its session only: "Do not
		// start" names no session to the core.
		ignored("the thread has a later session", nil)
		return
	}
	if t.State != control.Failed {
		ignored("the thread is not failed any more", nil)
		return
	}
	key := fmt.Sprintf("blockedbtn:%d#%d", thread, pos)
	if _, busy := f.pressing.LoadOrStore(key, struct{}{}); busy {
		ignored("a choice is being carried out", nil)
		return
	}
	defer f.pressing.Delete(key)
	caller := control.Caller{From: f.from(s.Author), Scopes: f.scopesIn(s.Ref.Conv.Channel), May: control.OpRead}
	switch choice {
	case choiceWithout:
		caller.May |= control.OpStart
		err = f.API(caller).StartWithoutSettings(ctx, thread, sf.session)
	case choiceTrust:
		caller.May |= control.OpTrust
		err = f.API(caller).Trust(ctx, thread, sf.session, sf.trust)
	case choiceClose:
		caller.May |= control.OpClose
		err = f.API(caller).CloseThread(ctx, thread)
	default:
		ignored("no button of the front end's", nil)
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, control.ErrForbidden): // the core logged it
		case errors.Is(err, session.ErrRefused):
			ignored("the session is not blocked by these files any more", err)
		default:
			ignored("not done", err)
		}
		return
	}
	// The core's start_chosen event takes the message's buttons away and
	// says who chose (chosenRows), as for a choice made elsewhere.
	f.Log.Info("blocked start: choice pressed", "thread", int64(thread), "choice", choice, "client", f.Name, "id", s.Author.ID)
}

// chosenRows are the edits of the blocked start's message that e, a
// start_chosen event, names, and of the replies that repeat it, for the
// thread's first conversation: each without its buttons, and who chose
// what, wherever they did.
func (f *FrontEnd) chosenRows(ctx context.Context, r *reader, e control.Event, conv surface.Conversation) ([]Row, error) {
	var d struct {
		Event     uint64
		Choice    string
		Principal string
	}
	if json.Unmarshal(e.Data, &d) != nil || d.Event == 0 {
		return nil, nil
	}
	var (
		sf    startFailed
		found bool
	)
	err := r.scan(ctx, e.Thread, e.Pos, func(ev control.Event) {
		if ev.Pos == d.Event {
			sf, found = startFailure(ev)
		}
	})
	if err != nil || !found {
		return nil, err
	}
	repo, err := r.repoOf(ctx, e.Thread)
	if err != nil {
		return nil, err
	}
	who := d.Principal
	if who == "" {
		who = "a person"
	}
	var done string
	switch control.StartChoice(d.Choice) {
	case control.StartWithoutSettings:
		done = "Started without this repository's settings by " + who + "."
	case control.StartTrusted:
		done = "Trusted by " + who + " for this channel's threads on " + repo + ": starting again."
	case control.StartClosed:
		done = "Not started: closed by " + who + "."
	default:
		return nil, nil // a choice of a later version
	}
	m := f.failedMessage(sf, repo, false)
	m.Text += "\n" + done
	rows := []Row{{Event: e.Pos, Conv: conv, Op: Edit, Target: &RowKey{Event: sf.pos, Part: 0}, Message: m, Decoration: true}}
	// The replies that offered the choices too (failedreply.go), each in the
	// link it was posted in: in the others the edit has no target.
	replies, err := f.failedReplies(ctx, sf.pos)
	if err != nil {
		return nil, err
	}
	for _, p := range replies {
		m := f.failedReplyMessage(sf, repo, false)
		m.Text += "\n" + done
		rows = append(rows, Row{Event: e.Pos, Part: p, Conv: conv, Op: Edit, Target: &RowKey{Event: sf.pos, Part: p}, Message: m, Decoration: true})
	}
	return rows, nil
}

// failureAt reads the event at pos of thread t as its session's start
// failure, and reports whether a later event names another session: one
// started again, or replaced.
func (f *FrontEnd) failureAt(ctx context.Context, t control.ThreadInfo, pos uint64) (sf startFailed, ok, later bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: []string{t.Scope}, May: control.OpRead})
	evs, err := api.Events(ctx, control.Filter{Thread: t.ID}, pos-1)
	if err != nil {
		return startFailed{}, false, false, err
	}
	defer stopEvents(cancel, evs)
	for e := range evs {
		switch {
		case e.Kind == control.CaughtUp:
			return sf, ok, later, nil
		case e.Pos == pos:
			sf, ok = startFailure(e)
			if !ok {
				return sf, false, false, nil
			}
		case e.Pos < pos || !ok:
			return startFailed{}, false, false, nil // no event at pos
		case e.Kind == "session_replaced" || sessionOf(e) != 0 && sessionOf(e) != sf.session:
			later = true
		}
	}
	return startFailed{}, false, false, ctx.Err()
}
