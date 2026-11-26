package core

import (
	"context"
	"errors"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

func TestApprovalOfAFailedThreadEndsWithoutHooks(t *testing.T) {
	for _, configured := range []bool{false, true} {
		f, _, sid, _, req := approvalFixture(t)
		// A failed restart ended the old turn. Match the persisted state
		// reached by the normal session failure path.
		if _, err := f.core.Store.DB.Exec(`UPDATE turns SET ended_at=1,status='failure' WHERE session_id=? AND ended_at IS NULL`, sid); err != nil {
			t.Fatal(err)
		}
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET state='failed' WHERE id=?`, sid); err != nil {
			t.Fatal(err)
		}
		hooks := &countHooks{}
		if configured {
			f.core.Hooks = hooks
		}
		before, inbox := f.runner.Calls(), count(t, f.core.Store, "inbox")
		if err := f.owner.Answer(context.Background(), req, control.Answer{Allow: true}); err != nil {
			t.Fatalf("hooks=%t: %v", configured, err)
		}
		if got := approvalState(t, f, req); got != "ended" {
			t.Fatalf("hooks=%t: approval %q", configured, got)
		}
		if hooks.n.Load() != 0 || f.runner.Calls() != before || count(t, f.core.Store, "inbox") != inbox {
			t.Fatalf("hooks=%t: hooks ran or input was accepted", configured)
		}
		if err := f.owner.Answer(context.Background(), req, control.Answer{Allow: true}); !errors.Is(err, ErrNotOpen) {
			t.Fatalf("repeat: %v", err)
		}
	}
}

func TestOrdinaryAndProposedSendToFailedThreadStayRefused(t *testing.T) {
	for name, guard := range map[string]*sendGuard{"ordinary": nil, "approved proposal": {proposal: true}} {
		t.Run(name, func(t *testing.T) {
			f, id := closedThread(t)
			hooks := &countHooks{}
			f.core.Hooks = hooks
			if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET state='failed' WHERE thread_id=?`, id); err != nil {
				t.Fatal(err)
			}
			before, inbox := f.runner.Calls(), count(t, f.core.Store, "inbox")
			f.core.stateRead = func() {
				// Even a reopen after the failed-state read must not take
				// an ordinary input whose hooks were skipped.
				if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET state='idle' WHERE thread_id=?`, id); err != nil {
					t.Error(err)
				}
			}
			err := f.owner.(*api).send(context.Background(), id, "m1", control.Input{Text: "message"}, control.Deliver, guard)
			if !errors.Is(err, session.ErrRefused) || hooks.n.Load() != 0 || f.runner.Calls() != before || count(t, f.core.Store, "inbox") != inbox {
				t.Fatalf("send %v, hooks %d, calls %s", err, hooks.n.Load(), f.runner.Calls())
			}
		})
	}
}
