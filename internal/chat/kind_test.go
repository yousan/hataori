package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface/fake"
)

func (f *front) setKind(t *testing.T, id control.ThreadID, kind control.ThreadKind) {
	t.Helper()
	if _, err := f.fe.Outbox.Store.DB.Exec(`UPDATE threads SET confined = ?, kind = ? WHERE id = ?`, kind.Confined(), kind, int64(id)); err != nil {
		t.Fatal(err)
	}
}

// Only a closed thread of the supervisor's own names the one that is the
// scope's now: a closed conversation and a closed look-up were replaced by
// none (#867).
func TestOnlyAClosedOwnThreadNamesTheCurrentOne(t *testing.T) {
	for kind, named := range map[control.ThreadKind]bool{control.KindOrdinary: false, control.KindSupervisor: true,
		control.KindConversation: false, control.KindLookup: false} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFront(t, workspaces{}, func(fe *FrontEnd) {
				fe.Supervisor = func(context.Context, string) (control.ThreadID, control.State, error) { return 99, control.Idle, nil }
			})
			id, _ := f.started(t)
			f.setKind(t, id, kind)
			for _, q := range []string{`UPDATE sessions SET state = 'closed'`, `UPDATE threads SET state = 'closed'`} {
				if _, err := f.fe.Outbox.Store.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			conv := f.conv(id)
			f.deliver(conv, fake.Webhook(hook), "are you there")
			want := ClosedLine
			if named {
				want += "\n" + supervisorNow(99, "")
			}
			// A pending prompt may be posted after the notice. Find the
			// notice among the posts instead of requiring it to be last.
			closed := func() string {
				for _, post := range postsIn(f.driver, conv) {
					if strings.HasPrefix(post, ClosedLine) {
						return post
					}
				}
				return ""
			}
			waitFor(t, "the closed line", func() bool { return closed() != "" })
			if got := closed(); got != want {
				t.Fatalf("posted %q, want %q", got, want)
			}
		})
	}
}

// A turn under a wake's key posts its final answer only, and nothing for
// "(quiet)", in a thread of each confined kind; in an ordinary one it posts
// all it says.
func TestConfinedKindsPostAWakesFinalAnswerOnly(t *testing.T) {
	for kind, want := range map[control.ThreadKind]string{control.KindOrdinary: "working|an answer|(quiet)", control.KindSupervisor: "an answer",
		control.KindConversation: "an answer", control.KindLookup: "an answer"} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFront(t, workspaces{})
			id, _ := f.started(t)
			f.setKind(t, id, kind)
			f.showing(t)
			f.event(t, id, "turn_started", map[string]any{"turn": 8, "by": "caller", "key": wakeKeyPrefix + "8"})
			f.event(t, id, "text", map[string]any{"text": "before", "final": true})
			ended(f, t, id, 8, "completed", "")
			base := len(f.postedUntil(t, id, "before"))
			f.event(t, id, "turn_started", map[string]any{"turn": 9, "by": "caller", "key": wakeKeyPrefix + "9"})
			f.event(t, id, "text", map[string]any{"text": "working", "final": false})
			f.event(t, id, "text", map[string]any{"text": "an answer", "final": true})
			ended(f, t, id, 9, "completed", "")
			f.event(t, id, "turn_started", map[string]any{"turn": 10, "by": "caller", "key": wakeKeyPrefix + "10"})
			f.event(t, id, "text", map[string]any{"text": "(quiet)", "final": true})
			ended(f, t, id, 10, "completed", "")
			f.event(t, id, "turn_started", map[string]any{"turn": 11, "by": "caller", "key": wakeKeyPrefix + "11"})
			f.event(t, id, "text", map[string]any{"text": "after", "final": true})
			got := f.postedUntil(t, id, "after")
			if s := texts(got[base : len(got)-1]); s != want {
				t.Errorf("posted %q, want %q", s, want)
			}
		})
	}
}
