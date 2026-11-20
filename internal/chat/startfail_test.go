package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/session"
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
	want := couldNotStart + "blocked at start: " + trustBlocked
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
// message holds it whole, below the reason: why hataori stopped, the
// dialog as the agent drew it with the files it names, and the next step.
func TestAStartFailureWithTheDialogPostsItBelowTheReason(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.showing(t)
	detail := "hataori did not answer the dialog for you, because it names files of the repository; it has not judged this repository dangerous.\n" +
		"The agent showed:\n> Trusting this folder loads .agent/settings.json.\n> ❯ No, exit\n> Yes, I trust this folder\n" +
		`What to do next: list it under "trust_files" of the scope "c1" in hataori's config, and start again.`
	f.event(t, id, "state", map[string]any{"from": "creating", "to": "failed", "reason": "blocked at start", "detail": detail})
	want := couldNotStart + "blocked at start.\n" + detail
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
