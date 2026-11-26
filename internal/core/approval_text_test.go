package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// Validation must precede both the durable request and its event.
func TestApprovalTextRejectedBeforeStorage(t *testing.T) {
	f, proposer, asksIn, target := proposalFixture(t)
	running, _ := f.running(t, "running")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: asksIn})
	for _, text := range []string{"do\u200bit", "\x00", "\r", "\u202e", "\U000e0061", "\U000e0100", "\u2800", "❤️️", "a\u200db", strings.Repeat("a", 32<<10+1)} {
		for _, call := range []struct {
			name string
			run  func() error
		}{
			{"send", func() error {
				return proposer.Send(context.Background(), target, "", control.Input{Text: text}, control.Refuse)
			}},
			{"start", func() error {
				_, err := proposer.Start(context.Background(), control.StartRequest{Scope: "s", Input: control.Input{Text: text}})
				return err
			}},
			{"interrupt", func() error { _, err := asker.AskInterrupt(context.Background(), running, "", text); return err }},
		} {
			err := call.run()
			if err == nil || errors.Is(err, control.ErrProposed) {
				t.Errorf("%s accepted invalid approval text (%d bytes)", call.name, len(text))
			}
		}
	}
	if n := count(t, f.core.Store, "proposals") + count(t, f.core.Store, "approvals"); n != 0 {
		t.Fatalf("stored %d invalid requests", n)
	}
	for _, kind := range []string{"proposal_made", "approval_asked"} {
		var n int
		if err := f.core.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind=?`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("posted %d invalid %s events", n, kind)
		}
	}
}
