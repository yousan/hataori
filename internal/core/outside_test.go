package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

var outsideSource = []control.OutsideText{{Kind: "message", ID: "outside-message"}}

func markOutside(t *testing.T, c *Core, id control.ThreadID, key string) {
	t.Helper()
	if err := c.Store.Update(context.Background(), func(tx *store.Tx) error {
		return store.MarkConversation(context.Background(), tx, id, key, outsideSource)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOutsideInputAndMarkCommitTogether(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	id := k.start(t, control.KindConversation, "conversation")
	// The second source fails after the first source has been inserted.
	bad := append(append([]control.OutsideText(nil), outsideSource...), control.OutsideText{})
	if err := k.fixture.owner.Send(ctx, id, "bad", control.Input{Text: "NEVER-GIVEN", Outside: bad}, control.Refuse); err == nil {
		t.Fatal("invalid provenance accepted")
	}
	marks, err := store.CurrentMarks(ctx, k.core.Store.DB, id)
	if err != nil || len(marks) != 0 || strings.Contains(k.runner.Calls(), "NEVER-GIVEN") {
		t.Fatalf("partial delivery: %v, %v, %s", marks, err, k.runner.Calls())
	}
	if err := k.fixture.owner.Send(ctx, id, "good", control.Input{Text: "OUTSIDE-GIVEN", Outside: outsideSource}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	marks, err = store.CurrentMarks(ctx, k.core.Store.DB, id)
	if err != nil || len(marks) != 1 || !strings.Contains(k.runner.Calls(), store.OutsideLine(outsideSource)) {
		t.Fatalf("delivery without mark/notice: %v, %v, %s", marks, err, k.runner.Calls())
	}
	var saved string
	if err := k.core.Store.DB.QueryRow(`SELECT outside FROM turns ORDER BY id DESC LIMIT 1`).Scan(&saved); err != nil || !strings.Contains(saved, "outside-message") {
		t.Fatalf("turn did not retain its mark: %q, %v", saved, err)
	}
	// These are internal fields. A session's JSON cannot set a mark.
	var input control.Input
	if err := json.Unmarshal([]byte(`{"text":"hello","outside":[{"kind":"message","id":"forged"}]}`), &input); err != nil || len(input.Outside) != 0 {
		t.Fatalf("socket set internal provenance: %+v, %v", input, err)
	}
}

func TestOutsideActChangesWithinTheSameTurn(t *testing.T) {
	ctx := context.Background()
	f, _, asksIn, target := proposalFixture(t)
	caller := control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpRead | control.OpSend | control.OpStart | control.OpAsk,
		AsksIn: asksIn, RefuseBusy: true, ProposeOutside: true}
	api := f.core.As(caller)
	if err := api.Send(ctx, target, "before", control.Input{Text: "BEFORE"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Start(ctx, control.StartRequest{Scope: "s", Key: "before-start", Thread: "before-start", Input: control.Input{Text: "BEFORE-START"}}); err != nil {
		t.Fatal(err)
	}
	sid, _, _ := f.core.current(ctx, f.core.Store.DB, target)
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "q", Text: "Which?"}})
	var turn int64
	f.core.Store.DB.QueryRow(`SELECT id FROM turns WHERE session_id = (SELECT max(id) FROM sessions WHERE thread_id = ?) AND ended_at IS NULL`, int64(asksIn)).Scan(&turn)
	if err := f.owner.Send(ctx, asksIn, "outside", control.Input{Text: "outside text", Outside: outsideSource}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	proposalOf(t, api.Send(ctx, target, "after-answer", control.Input{Text: "AFTER-ANSWER"}, control.Refuse))
	_, err := api.Start(ctx, control.StartRequest{Scope: "s", Key: "after-start", Input: control.Input{Text: "AFTER-START"}})
	proposalOf(t, err)
	if strings.Contains(f.runner.Calls(), "AFTER") {
		t.Fatalf("marked action reached runner: %s", f.runner.Calls())
	}
	for _, data := range eventsOf(t, f, asksIn, "proposal_made") {
		if !strings.Contains(data, "outside-message") {
			t.Fatalf("proposal omitted provenance: %s", data)
		}
	}
	var markedTurn string
	if err := f.core.Store.DB.QueryRow(`SELECT outside FROM turns WHERE id = ?`, turn).Scan(&markedTurn); err != nil || !strings.Contains(markedTurn, "outside-message") {
		t.Fatalf("same turn was not marked: %s, %v", markedTurn, err)
	}
	// An interrupt request identifies its source in every autonomy too.
	running, _ := f.running(t, "interrupt-target")
	if _, err := api.AskInterrupt(ctx, running, "interrupt", "please stop"); err != nil {
		t.Fatal(err)
	}
	if data := eventsOf(t, f, asksIn, "approval_asked"); len(data) != 1 || !strings.Contains(data[0], "outside-message") {
		t.Fatalf("interrupt omitted provenance: %v", data)
	}
}

func TestOutsideMarkSurvivesSleepReopenRestartAndReplacement(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	id := k.start(t, control.KindConversation, "conversation")
	markOutside(t, k.core, id, "")
	caller := control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, AsksIn: id, ProposeOutside: true}
	check := func(when string) {
		t.Helper()
		_, err := k.core.As(caller).Start(ctx, control.StartRequest{Scope: "s", Key: when, Input: control.Input{Text: "start"}})
		proposalOf(t, err)
	}
	check("ready")
	sid := k.session(t, id)
	if err := k.core.Machine.Apply(ctx, sid, session.Input{Kind: session.IdleTimeout}); err != nil {
		t.Fatal(err)
	}
	check("asleep")
	if err := k.fixture.owner.Send(ctx, id, "resume", control.Input{Text: "resume"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	check("resuming")
	k.agentEvent(t, sid, 2, agent.Event{Kind: agent.SessionReady, SessionID: "resumed"})
	k.close(t, id)
	check("closed")
	if _, err := k.owner.ReopenThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	check("reopened")
	if err := k.core.Store.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, k.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	k.core.Store, k.core.Machine.Store = st, st
	check("restarted")
	if err := st.Update(ctx, func(tx *store.Tx) error {
		_, err := k.core.again(ctx, tx, id, "s", sid, time.Now(), "conversation lost")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check("replaced")
}

func TestOutsideMarkBeforeFirstAgentID(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	id, err := k.core.StartConfined(ctx, control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
		control.StartRequest{Scope: "s", Key: "first", Thread: "first", Input: control.Input{Text: "outside first", Outside: outsideSource}}, control.KindConversation, InDir(k.dir))
	if err != nil {
		t.Fatal(err)
	}
	var agentID string
	if err := k.core.Store.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE thread_id = ?`, int64(id)).Scan(&agentID); err != nil || agentID != "" {
		t.Fatalf("agent has an ID already: %q, %v", agentID, err)
	}
	marks, err := store.CurrentMarks(ctx, k.core.Store.DB, id)
	if err != nil || len(marks) != 1 {
		t.Fatalf("first input not marked: %v, %v", marks, err)
	}
}

func TestOutsideMarkAtSendAcceptanceMakesAProposal(t *testing.T) {
	ctx := context.Background()
	f, _, asksIn, target := proposalFixture(t)
	api := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpSend, AsksIn: asksIn, ProposeOutside: true})
	f.core.Hooks = &countHooks{}
	f.core.stateRead = func() { markOutside(t, f.core, asksIn, "") }
	proposalOf(t, api.Send(ctx, target, "race", control.Input{Text: "RACE-MARKER"}, control.Refuse))
	if strings.Contains(f.runner.Calls(), "RACE-MARKER") {
		t.Fatal("action escaped while its caller became marked")
	}
}

func TestOutsideQuotaCountsProposalsAcrossConnections(t *testing.T) {
	ctx := context.Background()
	f, _, asksIn, target := proposalFixture(t)
	markOutside(t, f.core, asksIn, "")
	caller := control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpSend, AsksIn: asksIn, ProposeOutside: true, OutsideSends: 2}
	for i := range 2 {
		proposalOf(t, f.core.As(caller).Send(ctx, target, fmt.Sprint(i), control.Input{Text: "propose"}, control.Refuse))
	}
	if err := f.core.As(caller).Send(ctx, target, "third", control.Input{Text: "refused"}, control.Refuse); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("third marked send: %v", err)
	}
	other, _ := f.running(t, "unmarked")
	caller.AsksIn, caller.Propose = other, true
	proposalOf(t, f.core.As(caller).Send(ctx, target, "unmarked", control.Input{Text: "propose"}, control.Refuse))
}

func TestOutsideWakeIsIsolatedAndFinishedToolsKeepItsMark(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	id := k.start(t, control.KindSupervisor, "own")
	sid := k.session(t, id)
	if err := k.core.Machine.Apply(ctx, sid, session.Input{Kind: session.IdleTimeout}); err != nil {
		t.Fatal(err)
	}
	fresh := k.core.AsFresh(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, NoAnswer: true})
	wake := "hataori-wake-first"
	if err := fresh.Send(ctx, id, wake, control.Input{Text: "outside wake", Outside: outsideSource}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, sid, 2, agent.Event{Kind: agent.SessionReady, SessionID: "wake-id"})
	if err := k.core.Machine.Apply(ctx, sid, session.Input{Kind: session.Send, Principal: "owner", Key: wake, Text: "outside wake"}); err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	if err := k.core.Machine.Apply(ctx, sid, session.Input{Kind: session.IdleTimeout}); err != nil {
		t.Fatal(err)
	}
	marks, err := store.CurrentMarks(ctx, k.core.Store.DB, id)
	if err != nil || len(marks) != 0 {
		t.Fatalf("wake marked the set-aside conversation: %v, %v", marks, err)
	}
	var outside string
	if err := k.core.Store.DB.QueryRow(`SELECT outside FROM turns ORDER BY id DESC LIMIT 1`).Scan(&outside); err != nil || !strings.Contains(outside, "outside-message") {
		t.Fatalf("finished wake lost its mark: %q, %v", outside, err)
	}
	oldTools := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, AsksIn: id, ProposeOutside: true, ConversationKey: &wake})
	_, err = oldTools.Start(ctx, control.StartRequest{Scope: "s", Key: "late", Input: control.Input{Text: "late tool call"}})
	proposalOf(t, err)
	if err := fresh.Send(ctx, id, "hataori-wake-next", control.Input{Text: "ordinary wake"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	marks, err = store.CurrentMarks(ctx, k.core.Store.DB, id)
	if err != nil || len(marks) != 0 {
		t.Fatalf("next wake inherited a mark: %v, %v", marks, err)
	}
}

func TestOutsideMarkedActCannotChooseAnUnapprovedLaunch(t *testing.T) {
	ctx := context.Background()
	f, _, asksIn, _ := proposalFixture(t)
	id, sid := f.blocked(t, "blocked-worker", ".mcp.json")
	api := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpStart, AsksIn: asksIn, ProposeOutside: true})
	markOutside(t, f.core, asksIn, "")
	before := f.runner.Calls()
	if err := api.StartWithoutSettings(ctx, id, sid); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("marked caller chose a launch without approval: %v", err)
	}
	if f.runner.Calls() != before {
		t.Fatal("refused launch reached runner")
	}
}

func TestOutsideConcurrentCallsShareTheLastAllowance(t *testing.T) {
	ctx := context.Background()
	f, _, asksIn, target := proposalFixture(t)
	markOutside(t, f.core, asksIn, "")
	caller := control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpSend, AsksIn: asksIn, ProposeOutside: true, OutsideSends: 1}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- f.core.As(caller).Send(ctx, target, fmt.Sprintf("concurrent-%d", i), control.Input{Text: "proposal"}, control.Refuse)
		}(i)
	}
	wg.Wait()
	close(results)
	proposed, refused := 0, 0
	for err := range results {
		switch {
		case errors.Is(err, control.ErrProposed):
			proposed++
		case errors.Is(err, control.ErrForbidden):
			refused++
		default:
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if proposed != 1 || refused != 1 {
		t.Fatalf("%d proposed and %d refused; want one of each", proposed, refused)
	}
}
