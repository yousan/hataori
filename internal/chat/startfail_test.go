package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// trustBlocked is the detail an agent adapter gives a start blocked
// at the folder-trust dialog, as its capture test produces it.
const trustBlocked = "the folder-trust dialog says trusting the folder loads .agent/settings.json; " +
	"this scope does not trust .agent/settings.json; to trust them, add them to the scope's trust_files"

// A start that fails after Start returned, as one blocked at the
// folder-trust dialog does (#589): the thread gets one message saying why,
// after the prompt, also when the trigger is delivered again and when the
// daemon restarts and reads the events again.
func TestAStartThatFailsLaterIsShownOnce(t *testing.T) {
	f := newFront(t, workspaces{})
	id, trigger := f.started(t)
	stop := f.showing(t)
	// The agent ends its session at a start-up dialog it may not answer,
	// after the Start call returned.
	l := f.runner.all()
	if len(l) != 1 {
		t.Fatalf("%d launches", len(l))
	}
	err := f.core.Machine.Apply(context.Background(), l[0].Session, session.Input{Kind: session.AgentEvent,
		Agent: agent.Event{Seq: 1, Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Text: trustBlocked}})
	if err != nil {
		t.Fatal(err)
	}
	// The event names no files: the detail is the agent's, shown verbatim.
	want := couldNotStart + "blocked at start.\n" + f.driver.Verbatim(trustBlocked)
	calls := f.postedUntil(t, id, want)
	if texts(calls) != "the prompt|"+want {
		t.Fatalf("thread shows %q, want the prompt and one message with the reason", texts(calls))
	}
	if calls[1].Message.ReplyTo == nil || *calls[1].Message.ReplyTo != calls[0].Ref {
		t.Fatalf("the message replies to %+v, want the prompt %+v", calls[1].Message.ReplyTo, calls[0].Ref)
	}

	// Delivered again, and read again from the start after a restart.
	f.fe.Message(trigger)
	stop()
	if _, err := f.fe.Outbox.Store.DB.Exec(`DELETE FROM chat_cursors`); err != nil {
		t.Fatal(err)
	}
	f.showing(t)
	conv := f.conv(id)
	if err := f.fe.Outbox.Write(context.Background(), row(1<<40, 0, conv, "marker")); err != nil {
		t.Fatal(err)
	}
	if got := texts(f.postedUntil(t, id, "marker")); got != "the prompt|"+want+"|marker" {
		t.Fatalf("thread shows %q, want the message once", got)
	}
}

// A detail of several lines for a person, as the agent adapter writes for
// a start blocked at a start-up dialog that names files (#589): the
// message holds it below the reason: why hataori stopped, and the dialog as
// the agent drew it with the files it names. The dialog may hold text of the
// repository, such as the rules its settings allow, so it is shown verbatim
// (#754); the words around it, which are the adapter's, are as they are.
func TestAStartFailureWithTheDialogPostsItBelowTheReason(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	const why = "hataori did not answer the dialog for you, because it names files of the repository; it has not judged this repository dangerous.\n" +
		"The agent showed:\n"
	shown := []string{"⚠ This folder pre-approves 3 tool permissions in .agent/settings.json:",
		"Bash(ls:*), Bash([text](https://example.com)), Bash(> # a)", "# a heading", "> a quote", "```",
		"Trusting this folder loads .agent/settings.json.", "❯ No, exit", "Yes, I trust this folder"}
	const after = "\nMore of the adapter's."
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start",
		"detail": why + "> " + strings.Join(shown, "\n> ") + after, "trust": []string{".agent/settings.json"}})
	want := couldNotStart + "blocked at start.\n" + why + f.driver.Verbatim(strings.Join(shown, "\n")) + after + "\nWhat to do next: "
	var got string
	waitFor(t, "the message", func() bool {
		for _, c := range f.driver.Calls(f.conv(id)) {
			if c.Op == fake.OpPost && strings.HasPrefix(c.Message.Text, couldNotStart) {
				got = c.Message.Text
			}
		}
		return got != ""
	})
	if !strings.HasPrefix(got, want) {
		t.Fatalf("thread shows %q, want it to begin with %q", got, want)
	}
}

// A detail of a start that no dialog blocked is the agent's own output, of
// which nothing is known: all of it is shown verbatim (#754).
func TestAStartFailuresOutputIsShownVerbatim(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	const out = "> # not\n[a link](https://example.com)"
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start", "detail": out})
	want := couldNotStart + "blocked at start.\n" + f.driver.Verbatim(out)
	if got := texts(f.postedUntil(t, id, want)); got != "the prompt|"+want {
		t.Fatalf("thread shows %q", got)
	}
}

// A detail that is terminal output, such as the screen of a start that
// timed out, stays out of the chat; the reason is posted alone.
func TestAStartFailureWithTheScreenPostsTheReasonOnly(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start",
		"detail": "\x1b[2J\x1b[HAccessing workspace:\r\n/w\r\n\x1b[1mQuick safety check\x1b[0m"})
	want := couldNotStart + "blocked at start"
	if got := texts(f.postedUntil(t, id, want)); got != "the prompt|"+want {
		t.Fatalf("thread shows %q", got)
	}
}

// A later session that fails at its start, after a wake, gets its own
// message.
func TestALaterSessionsStartFailureIsShownToo(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "idle"})
	ev := map[string]any{"from": "creating", "to": "failed", "reason": "cannot resume"}
	f.event(t, id, "state", ev)
	want := couldNotStart + "cannot resume"
	if got := texts(f.postedUntil(t, id, want)); got != "the prompt|"+want {
		t.Fatalf("thread shows %q", got)
	}
}

func TestReadableDetail(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "",
		" one line ":              "one line",
		"two\nlines":              "two\nlines",
		"two\r\nlines":            "",
		"\x1b[1mbold":             "",
		strings.Repeat("x", 2001): "",
	} {
		if got := ReadableDetail(in); got != want {
			t.Errorf("ReadableDetail(%q) = %q, want %q", in, got, want)
		}
	}
}

// The event reader reads the failure before the trigger's start wrote the
// prompt (here: the daemon died between the two, and the trigger comes
// again later). The failure is not posted before the prompt.
func TestAFailureReadBeforeThePromptIsPostedAfterIt(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) { fe.Outbox.MaxFailures = 2 })
	st := f.fe.Outbox.Store
	st.DB.Exec(`CREATE TRIGGER lost AFTER INSERT ON chat_outbox BEGIN DELETE FROM chat_outbox WHERE id = NEW.id; END`)
	id, trigger := f.started(t)
	st.DB.Exec(`DROP TRIGGER lost`)
	f.showing(t)
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start"})
	time.Sleep(50 * time.Millisecond) // the outbox's retries, had it a row
	f.fe.Message(trigger)
	want := couldNotStart + "blocked at start"
	calls := f.postedUntil(t, id, want)
	if texts(calls) != "the prompt|"+want || calls[1].Message.ReplyTo == nil || *calls[1].Message.ReplyTo != calls[0].Ref {
		t.Fatalf("posts %+v, want the prompt, then the failure replying to it", calls)
	}
}

// A prompt that is never written, as after a daemon that died before
// writing it and a trigger never delivered again: the failure is posted
// after promptWait all the same.
func TestAFailureWhosePromptIsLostIsPostedAfterTheWait(t *testing.T) {
	defer func(w time.Duration) { promptWait = w }(promptWait)
	promptWait = 200 * time.Millisecond
	f := newFront(t, workspaces{}, func(fe *FrontEnd) { fe.Outbox.MaxFailures = 2 })
	st := f.fe.Outbox.Store
	st.DB.Exec(`CREATE TRIGGER lost AFTER INSERT ON chat_outbox BEGIN DELETE FROM chat_outbox WHERE id = NEW.id; END`)
	id, _ := f.started(t)
	st.DB.Exec(`DROP TRIGGER lost`)
	f.showing(t)
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start"})
	want := couldNotStart + "blocked at start"
	if got := texts(f.postedUntil(t, id, want)); got != want {
		t.Fatalf("thread shows %q", got)
	}
}

// A start blocked by files says what to do next (#589, #590): on a surface
// with buttons, the three choices, each explained, and the config entry
// named with its scope; without buttons, the config entry alone.
func TestABlockedStartsMessageSaysWhatToDoNext(t *testing.T) {
	for _, buttons := range []bool{true, false} {
		f := newFront(t, workspaces{})
		if !buttons {
			l := f.driver.Limits()
			l.MaxChoices = 0
			f.driver.SetLimits(l)
		}
		id, _ := f.started(t)
		f.showing(t)
		f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start",
			"detail": "hataori did not answer the dialog.\nThe agent showed:\n> Trusting this folder loads .agent/settings.json.",
			"trust":  []string{".agent/settings.json"}})
		var msg string
		var choices []string
		waitFor(t, "the message", func() bool {
			for _, c := range f.driver.Calls(f.conv(id)) {
				if c.Op == fake.OpPost && strings.HasPrefix(c.Message.Text, couldNotStart) {
					msg = c.Message.Text
					choices = nil
					for _, ch := range c.Message.Choices {
						choices = append(choices, ch.Label)
					}
					return true
				}
			}
			return false
		})
		config := `list .agent/settings.json under "trust_files" of the scope "c1" in hataori's config, and start again.`
		if !buttons {
			if !strings.HasSuffix(msg, "\nWhat to do next: "+config) || len(choices) != 0 {
				t.Fatalf("without buttons: %q %q", msg, choices)
			}
			continue
		}
		for _, want := range []string{"\nWhat to do next: choose below.",
			"\n- Start without this repository's settings: the thread runs without the repository's own settings files, so their hooks and commands never run",
			"\n- Trust and start (the owner only): trust .agent/settings.json for this channel's threads on https://example.com/r.git, and start.",
			"\n- Do not start: close this thread.", "\nOr " + config} {
			if !strings.Contains(msg, want) {
				t.Errorf("the message has no %q:\n%s", want, msg)
			}
		}
		if strings.Join(choices, "|") != "Start without this repository's settings|Trust and start|Do not start" {
			t.Fatalf("choices %q", choices)
		}
	}
}

// An old message's buttons act on its own session only: once the thread
// has a later session, "Do not start" on the old message closes nothing,
// whoever presses it (#590).
func TestAnOldBlockedMessagesButtonsDoNothing(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	l := f.runner.all()
	err := f.core.Machine.Apply(context.Background(), l[0].Session, session.Input{Kind: session.AgentEvent,
		Agent: agent.Event{Seq: 1, Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Text: "blocked", Trust: []string{".agent/settings.json"}}})
	if err != nil {
		t.Fatal(err)
	}
	var msg fake.Call
	waitFor(t, "the choices", func() bool {
		for _, c := range f.driver.Calls(f.conv(id)) {
			if c.Op == fake.OpPost && len(c.Message.Choices) == 3 {
				msg = c
				return true
			}
		}
		return false
	})
	// A later session, as after a start again that failed once more.
	f.event(t, id, "session_replaced", map[string]any{"new_session": 2, "reason": "trusted"})
	f.fe.Choice(surface.Selection{Ref: msg.Ref, Author: fake.Webhook(hook), Token: msg.Message.Choices[2].Token})
	if !strings.Contains(strings.Join(f.log.lines(), "\n"), "the thread has a later session") {
		t.Fatalf("log: %q", f.log.lines())
	}
	api := f.fe.API(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpRead})
	if d, err := api.Thread(context.Background(), id, 0, 0); err != nil || d.State != control.Failed {
		t.Fatalf("the thread is %s, %v; want failed", d.State, err)
	}
}

// A choice taken anywhere, as in the dashboard, takes the buttons away from
// the blocked start's message, which then says who chose what (#680). A
// choice of a later version changes nothing.
func TestAChoiceMadeElsewhereEndsTheButtons(t *testing.T) {
	for choice, want := range map[control.StartChoice]string{
		control.StartWithoutSettings:  "Started without this repository's settings by ada.",
		control.StartTrusted:          "Trusted by ada for this channel's threads on https://example.com/r.git: starting again.",
		control.StartClosed:           "Not started: closed by ada.",
		"a choice of a later version": "",
	} {
		f := newFront(t, workspaces{})
		id, _ := f.started(t)
		f.showing(t)
		blocked := f.event(t, id, "state", map[string]any{"session": 1, "from": "creating", "to": "failed", "reason": "blocked at start",
			"detail": "the dialog", "trust": []string{".agent/settings.json"}})
		var msg fake.Call
		waitFor(t, "the choices", func() bool {
			for _, c := range f.driver.Calls(f.conv(id)) {
				if c.Op == fake.OpPost && len(c.Message.Choices) == 3 {
					msg = c
					return true
				}
			}
			return false
		})
		f.event(t, id, "start_chosen", map[string]any{"session": 1, "event": blocked, "choice": choice, "principal": "ada", "via": "dashboard"})
		if err := f.fe.Outbox.Write(context.Background(), row(1<<40, 0, f.conv(id), "marker")); err != nil {
			t.Fatal(err)
		}
		f.postedUntil(t, id, "marker")
		edits := func() (got []string) {
			for _, c := range f.driver.Calls(f.conv(id)) {
				if c.Op == fake.OpEdit && c.Ref == msg.Ref && len(c.Message.Choices) == 0 {
					got = append(got, c.Message.Text)
				}
			}
			return got
		}
		if want == "" {
			time.Sleep(100 * time.Millisecond)
			if got := edits(); len(got) != 0 {
				t.Errorf("%s: edits %q", choice, got)
			}
			continue
		}
		waitFor(t, "the buttons to go", func() bool { return len(edits()) > 0 })
		if got := edits(); len(got) != 1 || !strings.HasPrefix(got[0], couldNotStart) || !strings.HasSuffix(got[0], "\n"+want) {
			t.Errorf("%s: edits %q, want one that ends with %q", choice, got, want)
		}
	}
}

// What a session_replaced event says in the chat (#710): a session that a
// choice replaced is said to be one only if it ran a turn, and one that the
// agent lost always is. Each is said once, also when the start after a
// choice is blocked in its turn.
func TestAReplacedSessionsLine(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	replaced := func(old int, reason string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session": old, "new_session": old + 1, "reason": reason})
		if err := f.fe.Outbox.Store.Update(context.Background(), func(tx *store.Tx) error {
			_, err := tx.Append(context.Background(), control.Event{Thread: id, Kind: "session_replaced", Data: b})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := "the prompt"
	shows := func(line string) {
		t.Helper()
		want += "|" + line
		var got string
		waitFor(t, want, func() bool {
			var posts []fake.Call
			for _, c := range f.driver.Calls(f.conv(id)) {
				if c.Op == fake.OpPost {
					posts = append(posts, c)
				}
			}
			got = texts(posts)
			return strings.Count(got, "|") >= strings.Count(want, "|")
		})
		if got != want {
			t.Fatalf("thread shows %q, want %q", got, want)
		}
	}
	nothing := func() {
		t.Helper()
		// The next event's row comes after any of the replaced session's.
		f.event(t, id, "reopened", map[string]any{"workspace": "kept", "conversation": "resume"})
		shows(ReopenedLine)
	}

	replaced(1, "trusted") // before any turn
	nothing()
	replaced(1, "without project settings")
	nothing()
	turn(f, t, id, 1, "k")
	ended(f, t, id, 1, "success", "")
	replaced(1, "trusted") // blocked as it woke
	shows(StartedAgainLine)
	replaced(2, "without project settings") // blocked again before a turn
	nothing()
	replaced(1, "without project settings")
	shows(StartedAgainLine)
	replaced(1, "cannot resume")
	shows(ReplacedLine)
	replaced(9, "cannot resume") // before any turn, as today
	shows(ReplacedLine)
}

// A session that could not resume and was replaced: the thread did start, so
// it gets the line of the new conversation and not that it could not start
// (#703), from the event reader and from a trigger delivered again alike. One
// that no session replaced, or that another was replaced for, is shown.
func TestAReplacedSessionsFailureIsNotShown(t *testing.T) {
	for name, c := range map[string]struct {
		session  int64  // of session_replaced; the failed one is 1
		reason   string // of session_replaced
		notShown bool
	}{
		"replaced":          {1, "cannot resume", true},
		"another session":   {9, "cannot resume", false},
		"another's reason":  {1, "trusted", false},
		"no event after it": {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFront(t, workspaces{})
			id, trigger := f.started(t)
			events := []control.Event{{Kind: "state", Data: json.RawMessage(`{"session":1,"from":"creating","to":"failed","reason":"cannot resume"}`)}}
			if c.session != 0 {
				b, _ := json.Marshal(map[string]any{"session": c.session, "new_session": c.session + 1, "reason": c.reason})
				events = append(events, control.Event{Kind: "session_replaced", Data: b})
			}
			// In one transaction, as the runner stores them.
			if err := f.fe.Outbox.Store.Update(context.Background(), func(tx *store.Tx) error {
				for _, e := range events {
					e.Thread = id
					if _, err := tx.Append(context.Background(), e); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var want []string
			if !c.notShown {
				want = append(want, couldNotStart+"cannot resume")
			}
			// The trigger again, before the events are read: what it writes.
			f.fe.Message(trigger)
			f.settled(t, f.conv(id))
			if got := postsIn(f.driver, f.conv(id)); !slices.Equal(got, append([]string{"the prompt"}, append(want, "marker")...)) {
				t.Fatalf("after the trigger came again the thread shows %q", got)
			}
			f.showing(t)
			ended(f, t, id, 1, "failure", "login")
			last := failureLine("login")
			f.postedUntil(t, id, last)
			if c.reason == "cannot resume" {
				want = append(want, ReplacedLine)
			}
			var got []string
			for _, p := range postsIn(f.driver, f.conv(id)) {
				if p != "marker" {
					got = append(got, p)
				}
			}
			if !slices.Equal(got, append(append([]string{"the prompt"}, want...), last)) {
				t.Fatalf("the thread shows %q", got)
			}
		})
	}
}
