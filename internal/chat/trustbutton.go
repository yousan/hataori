package chat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
)

// The trust button (#590): a start blocked because a start-up dialog named
// files the scope does not trust gets, on its "could not start" message, a
// button that trusts exactly those files for the scope's threads on the
// thread's repository and starts the thread again. Its token names the
// thread and the state event, so it works after a restart. A press calls
// Trust as the person who pressed; the core decides who may (the owner
// only) and refuses everyone else, who sees nothing, as with any button.
// Once the core has taken a press, the message loses its button and says
// who trusted the files.

// trustToken starts a trust button's token: trust:<thread>#<state event>.
const trustToken = "trust:"

// trustLabel is the button's label.
const trustLabel = "Trust these files for this repository"

// trustedPart is the part of the message's edit once a press was taken.
const trustedPart = 1

func trustChoice(thread control.ThreadID, pos uint64) surface.Choice {
	return surface.Choice{Label: trustLabel, Token: fmt.Sprintf("%s%d#%d", trustToken, thread, pos)}
}

// failedMessage is the "could not start" message of sf, with the trust
// button when the start was blocked by files and button is set, and the
// surface shows buttons.
func (f *FrontEnd) failedMessage(sf startFailed, repo string, button bool) surface.Message {
	m := surface.Message{Text: sf.text()}
	if len(sf.trust) == 0 || !button || !f.withButtons([]string{trustLabel}, false) {
		return m
	}
	m.Text += "\nThe owner may trust " + strings.Join(sf.trust, ", ") + " for this channel's threads on " + repo +
		", and start this thread again:"
	m.Choices = []surface.Choice{trustChoice(sf.thread, sf.pos)}
	return m
}

// pressTrust trusts the files that blocked the start the button was shown
// for, as the presser, and the thread starts again. A press that trusts
// nothing writes one log line and shows nothing.
func (f *FrontEnd) pressTrust(s surface.Selection, rest string) {
	ctx := f.context()
	ignored := func(reason string, err error) {
		f.Log.Warn("button ignored", "reason", reason, "client", f.Name, "id", s.Author.ID,
			"channel", s.Ref.Conv.Channel, "message", s.Ref.ID, "err", err)
	}
	th, p, ok := strings.Cut(rest, "#")
	n, err1 := strconv.ParseInt(th, 10, 64)
	pos, err2 := strconv.ParseUint(p, 10, 64)
	if !ok || err1 != nil || err2 != nil || pos == 0 {
		ignored("no button of the front end's", nil)
		return
	}
	thread := control.ThreadID(n)
	t, ok, err := f.threadHere(ctx, s.Ref.Conv)
	if err != nil || !ok || t.ID != thread {
		ignored("not a button of this thread", err)
		return
	}
	sf, ok, err := f.failureAt(ctx, t, pos)
	if err != nil || !ok || len(sf.trust) == 0 {
		ignored("no start blocked by files there", err)
		return
	}
	key := fmt.Sprintf("trustbtn:%d#%d", thread, pos)
	if _, busy := f.pressing.LoadOrStore(key, struct{}{}); busy {
		ignored("the files are being trusted", nil)
		return
	}
	defer f.pressing.Delete(key)
	api := f.API(control.Caller{From: f.from(s.Author), Scopes: f.scopesIn(s.Ref.Conv.Channel), May: control.OpRead | control.OpTrust})
	if err := api.Trust(ctx, thread, sf.session, sf.trust); err != nil {
		switch {
		case errors.Is(err, control.ErrForbidden): // the core logged it
		case errors.Is(err, session.ErrRefused):
			ignored("the session is not blocked by these files any more", err)
		default:
			ignored("not trusted", err)
		}
		return
	}
	who, _ := f.People.Resolve(f.from(s.Author))
	f.Log.Info("trust button pressed", "thread", int64(thread), "client", f.Name, "id", s.Author.ID)
	// The message loses its button, in every link of the thread.
	convs, err := f.convsOf(ctx, thread)
	if err != nil {
		f.Log.Warn("trust button: the message could not be changed", "thread", int64(thread), "err", err)
		return
	}
	m := f.failedMessage(sf, t.Repo, false)
	m.Text += "\nTrusted by " + who + " for this channel's threads on " + t.Repo + ": starting again."
	var rows []Row
	for _, c := range convs {
		rows = append(rows, Row{Event: pos, Part: trustedPart, Conv: c, Op: Edit, Target: &RowKey{Event: pos, Part: 0}, Message: m})
	}
	if err := f.Outbox.Write(ctx, rows...); err != nil {
		f.Log.Warn("trust button: the message could not be changed", "thread", int64(thread), "err", err)
		return
	}
	f.Outbox.Wake()
}

// failureAt reads the event at pos of thread t as its session's start
// failure.
func (f *FrontEnd) failureAt(ctx context.Context, t control.ThreadInfo, pos uint64) (startFailed, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: []string{t.Scope}, May: control.OpRead})
	evs, err := api.Events(ctx, control.Filter{Thread: t.ID}, pos-1)
	if err != nil {
		return startFailed{}, false, err
	}
	defer stopEvents(cancel, evs)
	for e := range evs {
		if e.Kind == control.CaughtUp || e.Pos > pos {
			return startFailed{}, false, nil
		}
		if e.Pos == pos {
			sf, ok := startFailure(e)
			return sf, ok, nil
		}
	}
	return startFailed{}, false, ctx.Err()
}
