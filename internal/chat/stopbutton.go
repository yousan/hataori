package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// The Stop button (#453, decision 7 of #248): a choice on a turn's progress
// line, the one message a quiet turn shows. Its token names the thread and
// the turn it was shown for, by the position of that turn's turn_started
// event, so it still works after a restart and never stops a later turn.
// A press calls Stop as the person who pressed, naming the turn by the
// number its turn_started gives it; the core decides who may, and
// interrupts only while that turn runs, checked with the interrupt (#457).
// Once the core has taken a press, the line shows that the turn is being
// stopped, without the button, until the turn ends and the line goes.

// stopToken starts a Stop button's token: stopturn:<thread>#<turn>.
const stopToken = "stopturn:"

// stoppedPart is the part of the line's edit that takes the button off,
// above every timed edit (linePart+n).
const stoppedPart = linePart + 1<<28

// stoppingText is what the line says once a press was taken.
const stoppingText = "Stopping the turn."

func stopChoice(thread control.ThreadID, turn uint64) surface.Choice {
	return surface.Choice{Label: "Stop", Token: fmt.Sprintf("%s%d#%d", stopToken, thread, turn)}
}

// stopKey is the record of a press the core took, kept in chat_notices
// under no channel: a button in any link of the thread is the same one.
func stopKey(thread control.ThreadID, turn uint64) string {
	return fmt.Sprintf("stopbtn:%d#%d", thread, turn)
}

// stopped reports whether a press of the turn's button was taken.
func (f *FrontEnd) stopped(ctx context.Context, thread control.ThreadID, turn uint64) (bool, error) {
	var n int
	err := f.Outbox.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM chat_notices WHERE client = ? AND channel = '' AND message = ?`,
		f.Name, stopKey(thread, turn)).Scan(&n)
	return n > 0, err
}

// pressStop stops the turn a Stop button was shown for, as the presser,
// while that turn runs. A press that stops nothing writes one log line and
// shows nothing.
func (f *FrontEnd) pressStop(s surface.Selection, rest string) {
	ctx := f.context()
	ignored := func(reason string, err error) {
		f.Log.Warn("button ignored", "reason", reason, "client", f.Name, "id", s.Author.ID,
			"channel", s.Ref.Conv.Channel, "message", s.Ref.ID, "err", err)
	}
	th, tu, ok := strings.Cut(rest, "#")
	n, err1 := strconv.ParseInt(th, 10, 64)
	turn, err2 := strconv.ParseUint(tu, 10, 64)
	if !ok || err1 != nil || err2 != nil || turn == 0 {
		ignored("no button of the front end's", nil)
		return
	}
	thread := control.ThreadID(n)
	t, ok, err := f.threadHere(ctx, s.Ref.Conv)
	if err != nil || !ok || t.ID != thread {
		ignored("not a button of this thread", err)
		return
	}
	number, err := f.turnOpen(ctx, t, turn)
	if err != nil || number == 0 {
		ignored("its turn is not running", err)
		return
	}
	// Each button stops once: a press after one the core took, also one
	// delivered again after a restart, does nothing more, by the record
	// written once the core took it. While the core is asked, the press is
	// held in this process only, so a second press meanwhile does nothing,
	// and a daemon that dies before the core took it leaves nothing that
	// refuses the next press (#534). The line changes only by the record, so
	// it shows nothing of a press the core refused.
	key := stopKey(thread, turn)
	if _, busy := f.pressing.LoadOrStore(key, struct{}{}); busy {
		ignored("the turn is being stopped", nil)
		return
	}
	defer f.pressing.Delete(key) // after the record is written: a press that comes next finds it
	if done, err := f.stopped(ctx, thread, turn); err != nil || done {
		ignored("the turn is being stopped", err)
		return
	}
	api := f.API(control.Caller{From: f.from(s.Author), Scopes: f.scopesIn(s.Ref.Conv.Channel), May: control.OpRead | control.OpStop})
	// The turn may end, and the next start, between the lookup and this
	// call: Stop names the turn, and the core then stops nothing.
	if err := api.Stop(ctx, thread, number); err != nil {
		if !errors.Is(err, control.ErrForbidden) { // the core logged a refusal
			ignored("not stopped", err)
		}
		return
	}
	if _, err := f.once(ctx, "", key, true); err != nil {
		f.Log.Warn("stop button: the press could not be recorded", "thread", int64(thread), "err", err)
	}
	f.Log.Info("stop button pressed", "thread", int64(thread), "client", f.Name, "id", s.Author.ID)
}

// turnOpen returns the number of the turn whose turn_started is at position
// pos, as that event gives it, while it is the thread's running turn: the
// thread runs, and no turn ended or started after it. It is 0 otherwise.
func (f *FrontEnd) turnOpen(ctx context.Context, t control.ThreadInfo, pos uint64) (int64, error) {
	if t.State != control.Running {
		return 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: []string{t.Scope}, May: control.OpRead})
	evs, err := api.Events(ctx, control.Filter{Thread: t.ID}, pos-1)
	if err != nil {
		return 0, err
	}
	defer stopEvents(cancel, evs)
	var number int64
	for e := range evs {
		switch {
		case e.Kind == control.CaughtUp:
			return number, nil
		case e.Pos == pos:
			if e.Kind == "turn_started" {
				var d struct {
					Turn int64 `json:"turn"`
				}
				json.Unmarshal(e.Data, &d) // no number: not a turn the button can name
				number = d.Turn
			}
		case e.Kind == "turn_started" || e.Kind == "turn_ended":
			return 0, nil
		}
	}
	return 0, ctx.Err()
}
