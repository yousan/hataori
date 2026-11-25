package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// session is thread id's session.
func (k *kinds) session(t *testing.T, id control.ThreadID) int64 {
	t.Helper()
	sid, _, err := k.core.current(context.Background(), k.core.Store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// as is the owner with OrdinaryOnly, so that nothing but that refuses its
// calls, asking in thread asksIn; with propose its starts and sends are
// proposals.
func (k *kinds) as(asksIn control.ThreadID, propose bool) control.API {
	return k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, AsksIn: asksIn, Propose: propose, OrdinaryOnly: true})
}

// refusedFor fails unless err refuses a call on confined thread id and says
// so.
func refusedFor(t *testing.T, what string, id control.ThreadID, err error) {
	t.Helper()
	if !errors.Is(err, control.ErrForbidden) || !strings.Contains(err.Error(), fmt.Sprintf("thread %d is a ", id)) ||
		!strings.Contains(err.Error(), "input from people only") {
		t.Errorf("%s: %v; want it refused, naming thread %d", what, err, id)
	}
}

// A caller with OrdinaryOnly gives no confined thread input, of whichever
// kind and state, the one it asks in included: its send, as a call and as a
// proposal, its request to interrupt it and its answer to what it asks are
// refused, and reach nothing. It reads them as before; an ordinary thread
// takes each of these, and a caller without it reaches each thread (#871).
func TestOrdinaryOnlyReachesNoConfinedThread(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	confined := []control.ThreadID{
		k.start(t, control.KindSupervisor, "sv"),
		k.start(t, control.KindConversation, "c1"),
		k.start(t, control.KindConversation, "c2"),
		k.start(t, control.KindLookup, "l1"),
	}
	ordinary, err := k.owner.Start(ctx, control.StartRequest{Key: "w", Scope: "s", Thread: "w", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, k.session(t, ordinary), 1, agent.Event{Kind: agent.SessionReady})
	person := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, AsksIn: ordinary})

	// Each of them asks in itself and in another, and calls and proposes.
	callers := func(f func(what string, api control.API)) {
		for _, asksIn := range confined {
			for _, propose := range []bool{false, true} {
				f(fmt.Sprintf("asking in %d, proposing %v", asksIn, propose), k.as(asksIn, propose))
			}
		}
	}
	unchanged := func(when string, f func()) {
		t.Helper()
		calls, events, rows := k.runner.Calls(), count(t, k.core.Store, "events"), count(t, k.core.Store, "proposals")+count(t, k.core.Store, "approvals")
		f()
		if got := k.runner.Calls(); got != calls {
			t.Errorf("%s: the runner got %q, want %q still", when, got, calls)
		}
		if got := count(t, k.core.Store, "events"); got != events {
			t.Errorf("%s: %d events, want %d still", when, got, events)
		}
		if got := count(t, k.core.Store, "proposals") + count(t, k.core.Store, "approvals"); got != rows {
			t.Errorf("%s: %d proposals and requests, want %d still", when, got, rows)
		}
	}
	for _, id := range confined {
		sid := k.session(t, id)
		unchanged("idle", func() {
			callers(func(what string, api control.API) {
				refusedFor(t, what+": a send to idle "+fmt.Sprint(id), id, api.Send(ctx, id, "k", control.Input{Text: "do it"}, control.Refuse))
				if d, err := api.Thread(ctx, id, 0, 0); err != nil || d.ID != id {
					t.Errorf("%s: reading thread %d: %v", what, id, err)
				}
			})
		})
		if err := person.Send(ctx, id, fmt.Sprint("p", id), control.Input{Text: "a person's"}, control.Refuse); err != nil {
			t.Fatalf("a person's send to thread %d: %v", id, err)
		}
		unchanged("running", func() {
			callers(func(what string, api control.API) {
				_, err := api.AskInterrupt(ctx, id, "k", "urgent")
				refusedFor(t, what+": to interrupt "+fmt.Sprint(id), id, err)
				refusedFor(t, what+": a send to running "+fmt.Sprint(id), id, api.Send(ctx, id, "k", control.Input{Text: "do it"}, control.Deliver))
			})
		})
		k.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})
		d, err := person.Thread(ctx, id, 0, 0)
		if err != nil || len(d.Requests) != 1 {
			t.Fatalf("thread %d's requests %+v, %v; want its question", id, d.Requests, err)
		}
		req := d.Requests[0].ID
		unchanged("waiting", func() {
			callers(func(what string, api control.API) {
				refusedFor(t, what+": an answer in "+fmt.Sprint(id), id, api.Answer(ctx, req, control.Answer{Text: "this one"}))
				refusedFor(t, what+": a send that answers in "+fmt.Sprint(id), id, api.Send(ctx, id, "k", control.Input{Text: "this one"}, control.Refuse))
			})
		})
		if err := person.Answer(ctx, req, control.Answer{Text: "this one"}); err != nil {
			t.Errorf("a person's answer in thread %d: %v", id, err)
		}
	}

	// The ordinary thread takes each.
	proposalOf(t, k.as(confined[1], true).Send(ctx, ordinary, "k1", control.Input{Text: "proposed"}, control.Refuse))
	worker := k.as(confined[1], false)
	if err := worker.Send(ctx, ordinary, "k2", control.Input{Text: "sent"}, control.Refuse); err != nil {
		t.Errorf("a send to an ordinary thread: %v", err)
	}
	if _, err := worker.AskInterrupt(ctx, ordinary, "k", "urgent"); err != nil {
		t.Errorf("a request to interrupt an ordinary thread: %v", err)
	}
	k.agentEvent(t, k.session(t, ordinary), 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})
	d, err := person.Thread(ctx, ordinary, 0, 0)
	if err != nil || len(d.Requests) == 0 {
		t.Fatalf("the ordinary thread's requests %+v, %v", d.Requests, err)
	}
	if err := worker.Answer(ctx, d.Requests[0].ID, control.Answer{Text: "this one"}); err != nil {
		t.Errorf("an answer in an ordinary thread: %v", err)
	}
}

// What is shown in a confined thread for a person to decide, a proposal and
// a request to interrupt, a caller with OrdinaryOnly decides in none: its
// own and another's.
func TestOrdinaryOnlyDecidesNothingShownInAConfinedThread(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	conv := k.start(t, control.KindConversation, "c1")
	other := k.start(t, control.KindConversation, "c2")
	worker, err := k.owner.Start(ctx, control.StartRequest{Key: "w", Scope: "s", Thread: "w", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, k.session(t, worker), 1, agent.Event{Kind: agent.SessionReady})
	sv := func(propose bool) control.API {
		return k.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: all, AsksIn: conv, Propose: propose, OrdinaryOnly: true, RefuseBusy: true})
	}
	proposalOf(t, sv(true).Send(ctx, worker, "k", control.Input{Text: "do it"}, control.Refuse))
	if err := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all}).
		Send(ctx, worker, "p", control.Input{Text: "a person's"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	if _, err := sv(false).AskInterrupt(ctx, worker, "k", "urgent"); err != nil {
		t.Fatal(err)
	}
	owner := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	d, err := owner.Thread(ctx, conv, 0, 0)
	if err != nil || len(d.Requests) != 2 {
		t.Fatalf("requests shown in thread %d: %+v, %v; want the proposal and the request to interrupt", conv, d.Requests, err)
	}
	calls := k.runner.Calls()
	for _, r := range d.Requests {
		for _, asksIn := range []control.ThreadID{conv, other} {
			refusedFor(t, "deciding "+r.Text, conv, k.as(asksIn, false).Answer(ctx, r.ID, control.Answer{Choices: []string{"approve"}}))
		}
	}
	if got := k.runner.Calls(); got != calls {
		t.Errorf("the runner got %q, want %q still", got, calls)
	}
	for _, r := range d.Requests {
		if err := owner.Answer(ctx, r.ID, control.Answer{Choices: []string{"refuse"}}); err != nil {
			t.Errorf("a person's decision of %s: %v", r.Text, err)
		}
	}
}

// A proposal of a send to a confined thread, made before they were refused,
// runs nothing when a person approves it, and says that it failed (#871).
func TestApprovedProposalReachesNoConfinedThread(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	asker := k.start(t, control.KindConversation, "c1")
	target := k.start(t, control.KindSupervisor, "sv")
	inputs := len(eventsOf(t, k.fixture, target, "input"))
	before := k.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpSend | control.OpStart,
		AsksIn: asker, Mark: "supervisor", RefuseBusy: true, Propose: true})
	proposalOf(t, before.Send(ctx, target, "k", control.Input{Text: "do it"}, control.Refuse))
	owner := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	d, err := owner.Thread(ctx, asker, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("requests: %+v %v", d.Requests, err)
	}
	if err := owner.Answer(ctx, d.Requests[0].ID, control.Answer{Choices: []string{"approve"}}); err != nil {
		t.Fatal(err)
	}
	if ev := eventsOf(t, k.fixture, asker, "proposal_answered"); len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"failed"`) {
		t.Fatalf("proposal_answered: %v; want one that failed", ev)
	}
	if calls := k.runner.Calls(); strings.Contains(calls, "do it") || len(eventsOf(t, k.fixture, target, "input")) != inputs {
		t.Fatalf("the confined thread got it: runner calls %s", calls)
	}
}

// So does an approved request to interrupt one: it is refused, interrupts
// nothing, and stays open for the person to refuse.
func TestApprovedInterruptReachesNoConfinedThread(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	asker := k.start(t, control.KindConversation, "c1")
	target := k.start(t, control.KindSupervisor, "sv")
	owner := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	if err := owner.Send(ctx, target, "p", control.Input{Text: "a person's"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	before := k.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: asker, Mark: "supervisor"})
	req, err := before.AskInterrupt(ctx, target, "k", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	calls, inputs := k.runner.Calls(), len(eventsOf(t, k.fixture, target, "input"))
	refusedFor(t, "its approval", target, owner.Answer(ctx, req, control.Answer{Allow: true}))
	if got := k.runner.Calls(); got != calls || len(eventsOf(t, k.fixture, target, "input")) != inputs {
		t.Fatalf("the confined thread got it: runner calls %s", got)
	}
	if err := owner.Answer(ctx, req, control.Answer{}); err != nil {
		t.Fatalf("its refusal: %v", err)
	}
	if ev := eventsOf(t, k.fixture, asker, "approval_answered"); len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"refused"`) {
		t.Fatalf("approval_answered: %v; want one that was refused", ev)
	}
}

// A request to interrupt a turn is the thread's it is asked in: shown there,
// with its own reason, and decided there, also when another of the scope's
// supervisor threads asks for the same turn. The turn is interrupted once
// (#871).
func TestInterruptRequestsOfEachThreadAreItsOwn(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	threads := []control.ThreadID{
		k.start(t, control.KindSupervisor, "sv"),
		k.start(t, control.KindConversation, "c1"),
		k.start(t, control.KindConversation, "c2"),
	}
	worker, err := k.owner.Start(ctx, control.StartRequest{Key: "w", Scope: "s", Thread: "w", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, k.session(t, worker), 1, agent.Event{Kind: agent.SessionReady})
	owner := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	if err := owner.Send(ctx, worker, "p", control.Input{Text: "a person's"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	reason := func(id control.ThreadID) string { return fmt.Sprintf("asked in %d", id) }
	reqs := map[control.ThreadID]control.RequestID{}
	for range 2 { // again gives each thread its own one
		for _, id := range threads {
			sv := k.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk | control.OpSend, AsksIn: id, Mark: "supervisor", OrdinaryOnly: true})
			req, err := sv.AskInterrupt(ctx, worker, "", reason(id))
			if err != nil || reqs[id] != "" && reqs[id] != req {
				t.Fatalf("asked in thread %d: %q, %v; its first one is %q", id, req, err, reqs[id])
			}
			reqs[id] = req
		}
	}
	for _, id := range threads {
		d, err := owner.Thread(ctx, id, 0, 0)
		if err != nil || len(d.Requests) != 1 || d.Requests[0].ID != reqs[id] || !strings.Contains(d.Requests[0].Text, reason(id)) {
			t.Errorf("thread %d shows %+v, %v; want its own request %q alone", id, d.Requests, err, reqs[id])
		}
		if ev := eventsOf(t, k.fixture, id, "approval_asked"); len(ev) != 1 || !strings.Contains(ev[0], reason(id)) {
			t.Errorf("thread %d: approval_asked %v; want its own one", id, ev)
		}
	}
	for _, want := range []struct {
		thread  control.ThreadID
		allow   bool
		outcome string
	}{{threads[2], true, "interrupted"}, {threads[1], true, "ended"}, {threads[0], false, "refused"}} {
		if err := owner.Answer(ctx, reqs[want.thread], control.Answer{Allow: want.allow}); err != nil {
			t.Fatal(err)
		}
		delete(reqs, want.thread)
		if ev := eventsOf(t, k.fixture, want.thread, "approval_answered"); len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"`+want.outcome+`"`) {
			t.Errorf("decided in thread %d: approval_answered %v; want one, %s", want.thread, ev, want.outcome)
		}
		for _, id := range threads {
			d, err := owner.Thread(ctx, id, 0, 0)
			if open := reqs[id] != ""; err != nil || open && (len(d.Requests) != 1 || d.Requests[0].ID != reqs[id]) || !open && len(d.Requests) != 0 {
				t.Errorf("after the decision in thread %d, thread %d shows %+v, %v; want %q alone if any", want.thread, id, d.Requests, err, reqs[id])
			}
		}
	}
	calls := k.runner.Calls()
	if strings.Count(calls, "interrupt") != 1 || strings.Count(calls, reason(threads[2])) != 1 || strings.Contains(calls, reason(threads[1])) {
		t.Errorf("runner calls %s; want one interrupt, with the reason of the one approved first", calls)
	}
}

// Open requests of several of the supervisor's threads for one turn that are
// approved at once interrupt it once: the ones approved while the first's
// interrupt is on its way wait for it, and end without one (#871).
func TestInterruptRequestsApprovedAtOnceInterruptOnce(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	threads := []control.ThreadID{k.start(t, control.KindSupervisor, "sv")}
	for i := range 4 {
		threads = append(threads, k.start(t, control.KindConversation, fmt.Sprint("c", i)))
	}
	worker, err := k.owner.Start(ctx, control.StartRequest{Key: "w", Scope: "s", Thread: "w", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, k.session(t, worker), 1, agent.Event{Kind: agent.SessionReady})
	owner := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	if err := owner.Send(ctx, worker, "p", control.Input{Text: "a person's"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	var reqs []control.RequestID
	for _, id := range threads {
		sv := k.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk | control.OpSend, AsksIn: id, Mark: "supervisor", OrdinaryOnly: true})
		req, err := sv.AskInterrupt(ctx, worker, "", fmt.Sprint("asked in ", id))
		if err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, req)
	}
	var wg sync.WaitGroup
	k.runner.mu.Lock()
	k.runner.beforeInterrupt = func() {
		for _, req := range reqs[1:] {
			wg.Go(func() {
				if err := owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
					t.Error(err)
				}
			})
		}
		// Each comes as far as it can while the first has no outcome.
		time.Sleep(500 * time.Millisecond)
	}
	k.runner.mu.Unlock()
	if err := owner.Answer(ctx, reqs[0], control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, id := range threads {
		want := "ended"
		if i == 0 {
			want = "interrupted"
		}
		if ev := eventsOf(t, k.fixture, id, "approval_answered"); len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"`+want+`"`) {
			t.Errorf("thread %d: approval_answered %v; want one, %s", id, ev, want)
		}
	}
	if calls := k.runner.Calls(); strings.Count(calls, "interrupt") != 1 {
		t.Errorf("runner calls %s; want one interrupt", calls)
	}
}

// The directory of a confined start is asked for after everything that may
// refuse the start, and once for a key: not for a guest, a person of no
// scope, a caller without OpStart, a scope that is off, nor for a key that
// started a thread (#871).
func TestConfinedStartAsksForItsDirectoryLast(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "ann", IDs: []string{"chat:ann"}}, {Name: "eve", IDs: []string{"chat:eve"}}},
		"owner", map[string][]string{"s": {"ann"}, "off": {"ann"}})
	asked := 0
	dir := func(context.Context) (string, error) { asked++; return k.dir, nil }
	start := func(from, scope, key string, may control.Ops) (control.ThreadID, error) {
		return k.core.StartConfined(ctx, control.Caller{From: from, Scopes: []string{scope}, May: may},
			control.StartRequest{Key: key, Scope: scope, Thread: key, Input: control.Input{Text: "go"}}, control.KindConversation, dir)
	}
	threads := count(t, k.core.Store, "threads")
	for what, refused := range map[string]func() (control.ThreadID, error){
		"a guest":                   func() (control.ThreadID, error) { return start("chat:stranger", "s", "k", all) },
		"nobody":                    func() (control.ThreadID, error) { return start("", "s", "k", all) },
		"a person of another scope": func() (control.ThreadID, error) { return start("chat:eve", "s", "k", all) },
		"a caller that may not":     func() (control.ThreadID, error) { return start("chat:ann", "s", "k", control.OpRead) },
		"a scope that is off":       func() (control.ThreadID, error) { return start("chat:ann", "off", "k", all) },
		"a key kept for proposals":  func() (control.ThreadID, error) { return start("chat:ann", "s", ProposalKeyPrefix+"x", all) },
	} {
		if id, err := refused(); err == nil || id != 0 {
			t.Errorf("%s: started %d, %v", what, id, err)
		}
	}
	if got := count(t, k.core.Store, "threads"); asked != 0 || got != threads {
		t.Fatalf("the directory was asked for %d times, and there are %d threads, after refused starts; want none and %d", asked, got, threads)
	}
	id, err := start("chat:ann", "s", "k", all)
	if err != nil || asked != 1 {
		t.Fatalf("a person's: %v, asked %d times", err, asked)
	}
	if again, err := start("chat:ann", "s", "k", all); err != nil || again != id || asked != 1 {
		t.Fatalf("its key again: thread %d, %v, asked %d times; want %d, and once", again, err, asked, id)
	}

	// Without one, nothing starts.
	for what, dir := range map[string]Dir{
		"no Dir":       nil,
		"an empty one": InDir(""),
		"a failed one": func(context.Context) (string, error) { return "", errors.New("no room") },
	} {
		if id, err := k.core.StartConfined(ctx, control.Caller{From: "chat:ann", Scopes: []string{"s"}, May: all},
			control.StartRequest{Key: what, Scope: "s", Thread: what, Input: control.Input{Text: "go"}}, control.KindConversation, dir); err == nil || id != 0 {
			t.Errorf("%s: started %d, %v", what, id, err)
		}
	}
	if got := count(t, k.core.Store, "threads"); got != threads+1 {
		t.Fatalf("%d threads, want %d", got, threads+1)
	}
}

// Two calls with one key at once make one thread: the second one asks for
// no directory and opens no chat thread.
func TestConfinedStartsOfOneKeyGoOneAtATime(t *testing.T) {
	k := newKinds(t)
	var asked atomic.Int32
	dir := func(context.Context) (string, error) {
		asked.Add(1)
		time.Sleep(100 * time.Millisecond) // a second one would come in here
		return k.dir, nil
	}
	threads := count(t, k.core.Store, "threads")
	var ids [2]control.ThreadID
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			id, err := k.core.StartConfined(context.Background(), control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
				control.StartRequest{Key: "k", Scope: "s", Thread: "k", Input: control.Input{Text: "go"}}, control.KindConversation, dir)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		})
	}
	wg.Wait()
	if got := count(t, k.core.Store, "threads"); ids[0] != ids[1] || got != threads+1 || asked.Load() != 1 {
		t.Fatalf("threads %v, %d made, the directory asked for %d times; want one of each", ids, got-threads, asked.Load())
	}
}
