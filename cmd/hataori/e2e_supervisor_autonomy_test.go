package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// svAutonomy is a root whose scope c1 has a supervisor of the autonomy a
// test names, run on the test's clock (wakeTest), and the supervisor's
// tools as its session's token reaches them. Its own person, sv1, also
// has the chat id sv-person.
type svAutonomy struct {
	wt    *wakeTest
	stop  func()
	log   func() string
	owner *wire.Client
	sv    control.ThreadInfo
	conv  surface.Conversation
	token string
	m     *supervisor.MCP
}

// The chat ids of the fake surface that press buttons in these tests: a
// person of the scope, the supervisor's own person, and a guest.
const (
	svPerson = "sv-person"
	svGuest  = "stranger"
)

func newSvAutonomy(t *testing.T, mode string, more ...func(cfg map[string]any)) *svAutonomy {
	t.Helper()
	opts := append([]func(cfg map[string]any){func(cfg map[string]any) {
		cfg["people"].([]map[string]any)[2]["ids"] = []string{supervisor.ID("c1"), fakeSurface + ":" + svPerson}
	}}, more...)
	if mode != "" {
		opts = append(opts, autonomy(mode))
	}
	f := &svAutonomy{wt: newWakeTest(t, opts...)}
	f.start(t)
	t.Cleanup(func() {
		f.owner.Close()
		f.stop()
	})
	return f
}

func (f *svAutonomy) start(t *testing.T) {
	t.Helper()
	f.stop, f.log = runDaemonLog(t, f.wt.r.root)
	f.wt.r.connected(t)
	var err error
	if f.owner, err = wire.Dial(f.wt.r.root); err != nil {
		t.Fatal(err)
	}
	if f.wt.db == nil {
		f.wt.db = f.wt.r.db(t)
	}
	f.sv = supervisorThread(t, f.owner, "c1", control.Idle)
	f.conv = surface.Conversation{Channel: f.sv.Links[0].Channel, Thread: f.sv.Links[0].Thread}
	var sid int64
	if err := f.wt.db.QueryRow(`SELECT id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, f.sv.ID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(runner.StateDir(f.wt.r.root, sid), supervisor.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	f.token = strings.TrimSpace(string(token))
	root := f.wt.r.root
	f.m = &supervisor.MCP{Dial: func() (control.API, error) { return wire.DialToken(root, f.token) }, Scope: "c1"}
}

func (f *svAutonomy) restart(t *testing.T) {
	t.Helper()
	f.owner.Close()
	f.stop()
	f.start(t)
}

// running starts a thread whose turn runs for a while, and waits until it
// runs.
func (f *svAutonomy) running(t *testing.T, key string) control.ThreadID {
	t.Helper()
	id, err := f.owner.Start(context.Background(), control.StartRequest{Key: key, Scope: "c1", Thread: "t-" + key, Repo: f.wt.repo,
		Input: control.Input{Text: "tool t 30s"}})
	if err != nil {
		t.Fatal(err)
	}
	f.busy(t, id)
	return id
}

// busy waits until thread id runs a turn.
func (f *svAutonomy) busy(t *testing.T, id control.ThreadID) {
	t.Helper()
	eventually(t, fmt.Sprintf("thread %d running", id), 15*time.Second, func() bool {
		d, err := f.owner.Thread(context.Background(), id, 0, 0)
		return err == nil && d.State == control.Running
	})
}

// propose calls tool with args, which must answer with a proposal, and
// returns its number.
func (f *svAutonomy) propose(t *testing.T, tool string, args map[string]any) int64 {
	t.Helper()
	out := mcpCall(t, f.m, tool, args)
	rest, ok := strings.CutPrefix(out.Text, "Proposed as proposal ")
	if out.IsError || !ok {
		t.Fatalf("%s: %+v, want a proposal", tool, out)
	}
	n, err := strconv.ParseInt(rest[:strings.IndexByte(rest, ':')], 10, 64)
	if err != nil {
		t.Fatalf("%s: %q: %v", tool, out.Text, err)
	}
	return n
}

// shown waits for the post of proposal n in the supervisor's thread, with
// Approve and Refuse, and returns it.
func (f *svAutonomy) shown(t *testing.T, n int64) fakesurface.Call {
	t.Helper()
	return f.buttons(t, fmt.Sprintf("Proposal %d of the supervisor: ", n))
}

// buttons waits for a post in the supervisor's thread that starts with
// prefix and has two buttons, and returns it.
func (f *svAutonomy) buttons(t *testing.T, prefix string) fakesurface.Call {
	t.Helper()
	var q fakesurface.Call
	eventually(t, "a post with buttons: "+prefix, 15*time.Second, func() bool {
		for _, c := range f.wt.r.driver.Calls(f.conv) {
			if c.Op == fakesurface.OpPost && strings.HasPrefix(c.Message.Text, prefix) && len(c.Message.Choices) == 2 {
				q = c
				return true
			}
		}
		return false
	})
	return q
}

func (f *svAutonomy) press(t *testing.T, q fakesurface.Call, who, label string) {
	t.Helper()
	if err := f.wt.r.driver.PressMany(q.Ref, who, choiceToken(t, q, label)); err != nil {
		t.Fatal(err)
	}
}

// decided waits until the supervisor's thread has n events of kind, and
// returns them with the position of the last.
func (f *svAutonomy) decided(t *testing.T, kind string, n int) ([]map[string]any, uint64) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d %s", n, kind), 15*time.Second, func() bool { return len(threadEvents(t, f.owner, f.sv.ID, kind)) >= n })
	var pos uint64
	f.wt.db.QueryRow(`SELECT max(pos) FROM events WHERE thread_id = ? AND kind = ?`, int64(f.sv.ID), kind).Scan(&pos)
	return threadEvents(t, f.owner, f.sv.ID, kind), pos
}

// fromSupervisor counts the inputs and answers of thread id that the
// supervisor gave it.
func (f *svAutonomy) fromSupervisor(t *testing.T, id control.ThreadID) int {
	t.Helper()
	var n int
	if err := f.wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind IN ('input', 'answer') AND json_extract(data, '$.from') = ?`,
		int64(id), supervisor.ID("c1")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// threadsIn counts the threads of c1.
func (f *svAutonomy) threadsIn(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.wt.db.QueryRow(`SELECT count(*) FROM threads WHERE scope = 'c1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Under watch the supervisor's start, send and interrupt request are
// refused, with a reason that names the mode, and reach no thread (#650).
func TestAutonomyWatchRefuses(t *testing.T) {
	f := newSvAutonomy(t, "watch")
	idle, _ := f.wt.worker(t, f.owner, "c1", "idle", "reply idle")
	busy := f.running(t, "busy")
	threads := f.threadsIn(t)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{supervisor.ToolSend, map[string]any{"number": int64(idle), "text": "reply sent"}},
		{supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "text": "reply started"}},
		{supervisor.ToolInterrupt, map[string]any{"number": int64(busy), "reason": "urgent"}},
	} {
		out := mcpCall(t, f.m, c.tool, c.args)
		if !out.IsError || !strings.HasPrefix(out.Text, `not allowed: the supervisor's autonomy in this channel is "watch"`) {
			t.Fatalf("%s under watch: %+v", c.tool, out)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.fromSupervisor(t, idle) + f.fromSupervisor(t, busy); n != 0 {
		t.Fatalf("%d inputs of the supervisor reached a thread", n)
	}
	if got := f.threadsIn(t); got != threads {
		t.Fatalf("%d threads, want %d", got, threads)
	}
	if d, err := f.owner.Thread(context.Background(), busy, 0, 0); err != nil || d.State != control.Running || len(threadEvents(t, f.owner, busy, "turn_ended")) != 0 {
		t.Fatalf("the busy thread: %+v %v", d, err)
	}
	for _, kind := range []string{"proposal_made", "approval_asked"} {
		if ev := threadEvents(t, f.owner, f.sv.ID, kind); len(ev) != 0 {
			t.Fatalf("%s under watch: %v", kind, ev)
		}
	}
	for _, op := range []string{"Send", "Start", "AskInterrupt"} {
		if !refusedLine(f.log(), op, "c1") {
			t.Fatalf("no refusal of %s logged:\n%s", op, f.log())
		}
	}
}

// Under propose, the default, a send reaches no thread until a person of
// the scope approved it in the supervisor's thread. The supervisor's own
// person, a guest and the supervisor's token cannot approve it. It is kept
// across a restart; approved twice, and again after a restart, it runs
// once (#650).
func TestAutonomyProposedSend(t *testing.T) {
	f := newSvAutonomy(t, "") // no autonomy key: propose
	ctx := context.Background()
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	n := f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply ordered"})
	q := f.shown(t, n)
	if want := fmt.Sprintf("Proposal %d of the supervisor: send to thread %d", n, w); !strings.HasPrefix(q.Message.Text, want) ||
		!strings.HasSuffix(q.Message.Text, "?\nreply ordered") {
		t.Fatalf("the proposal's post: %q", q.Message.Text)
	}
	d, err := f.owner.Thread(ctx, f.sv.ID, 0, 0)
	if err != nil || len(d.Requests) != 1 || d.Requests[0].Kind != control.Approval {
		t.Fatalf("the supervisor's requests: %+v %v", d.Requests, err)
	}
	req := d.Requests[0].ID

	f.press(t, q, svPerson, "Approve")
	f.press(t, q, svGuest, "Approve")
	tokenAPI, err := wire.DialToken(f.wt.r.root, f.token)
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenAPI.Answer(ctx, req, control.Answer{Allow: true}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("an answer on the supervisor's token: %v", err)
	}
	tokenAPI.Close()
	eventually(t, "the refused answers logged", 10*time.Second, func() bool {
		return strings.Count(f.log(), "op=Answer") >= 3
	})
	if got := f.fromSupervisor(t, w); got != 0 || len(threadEvents(t, f.owner, f.sv.ID, "proposal_answered")) != 0 {
		t.Fatalf("before a person approved: %d inputs of the supervisor, %v", got, threadEvents(t, f.owner, f.sv.ID, "proposal_answered"))
	}

	f.restart(t)
	f.press(t, q, e2eHook, "Approve")
	ev, _ := f.decided(t, "proposal_answered", 1)
	if ev[0]["outcome"] != "run" || ev[0]["principal"] != "dispatch" || ev[0]["target"] != float64(w) {
		t.Fatalf("proposal_answered: %v", ev[0])
	}
	f.wt.ended(t, w, 2)
	f.wt.r.posted(t, f.conv, fmt.Sprintf("Approved by dispatch: proposal %d was sent to thread %d.", n, w))
	marked := false
	for _, in := range threadEvents(t, f.owner, w, "input") {
		marked = marked || in["text"] == supervisor.Mark+"\nreply ordered"
	}
	if !marked {
		t.Fatalf("the worker's inputs: %v", threadEvents(t, f.owner, w, "input"))
	}

	f.press(t, q, e2eOwner, "Approve")
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err == nil {
		t.Fatal("answering a decided proposal succeeded")
	}
	f.restart(t)
	f.press(t, q, e2eHook, "Approve")
	time.Sleep(300 * time.Millisecond)
	if got := f.fromSupervisor(t, w); got != 1 || len(threadEvents(t, f.owner, f.sv.ID, "proposal_answered")) != 1 ||
		len(threadEvents(t, f.owner, w, "turn_ended")) != 2 {
		t.Fatalf("after approving again: %d inputs of the supervisor, %d decisions", got, len(threadEvents(t, f.owner, f.sv.ID, "proposal_answered")))
	}
}

// Under propose a start makes no thread until a person approved it; then
// it makes one, once, with its first input marked (#650).
func TestAutonomyProposedStart(t *testing.T) {
	f := newSvAutonomy(t, "propose")
	threads := f.threadsIn(t)
	n := f.propose(t, supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "text": "reply started"})
	q := f.shown(t, n)
	if !strings.Contains(q.Message.Text, "start a thread in "+f.wt.repo) || !strings.HasSuffix(q.Message.Text, "?\nreply started") {
		t.Fatalf("the proposal's post: %q", q.Message.Text)
	}
	time.Sleep(300 * time.Millisecond)
	if got := f.threadsIn(t); got != threads {
		t.Fatalf("%d threads before the approval, want %d", got, threads)
	}
	f.press(t, q, svPerson, "Approve")
	f.press(t, q, e2eHook, "Approve")
	ev, _ := f.decided(t, "proposal_answered", 1)
	target, _ := ev[0]["target"].(float64)
	if ev[0]["outcome"] != "run" || ev[0]["principal"] != "dispatch" || target == 0 {
		t.Fatalf("proposal_answered: %v", ev[0])
	}
	id := control.ThreadID(target)
	f.wt.ended(t, id, 1)
	f.wt.r.posted(t, f.conv, fmt.Sprintf("Approved by dispatch: proposal %d started thread %d.", n, id))
	in := threadEvents(t, f.owner, id, "input")
	if len(in) != 1 || in[0]["text"] != supervisor.Mark+"\nreply started" || in[0]["via"] != control.ViaProposal {
		t.Fatalf("the started thread's inputs: %v", in)
	}
	f.press(t, q, e2eOwner, "Approve")
	time.Sleep(300 * time.Millisecond)
	if got := f.threadsIn(t); got != threads+1 {
		t.Fatalf("%d threads after approving twice, want %d", got, threads+1)
	}
}

// A proposed send approved while its target runs a turn is not given to
// it, and the outcome, in the supervisor's thread and in its next wake,
// says it was busy (#650).
func TestAutonomyProposedSendToABusyThread(t *testing.T) {
	f := newSvAutonomy(t, "propose")
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	n := f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply late"})
	q := f.shown(t, n)
	if err := f.owner.Send(context.Background(), w, "busy", control.Input{Text: "tool t 30s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.busy(t, w)
	f.press(t, q, e2eHook, "Approve")
	ev, pos := f.decided(t, "proposal_answered", 1)
	if ev[0]["outcome"] != "busy" {
		t.Fatalf("proposal_answered: %v", ev[0])
	}
	f.wt.r.posted(t, f.conv, fmt.Sprintf("Approved by dispatch, but thread %d was running a turn or waiting on a question: proposal %d was not sent.", w, n))
	if got := f.fromSupervisor(t, w); got != 0 {
		t.Fatalf("%d inputs of the supervisor reached the busy thread", got)
	}
	f.wt.read(t, pos)
	f.wt.quiet(t)
	wakes := f.wt.woken(t, f.owner, f.sv.ID, 1)
	if want := fmt.Sprintf("Your proposal %d, to send to thread %d: approved by dispatch, but not sent: the thread was running a turn or waiting on a question.", n, w); !strings.Contains(wakes[0], want) {
		t.Fatalf("the wake:\n%s\nwant a line %q", wakes[0], want)
	}
}

// A refused proposal, and each other outcome of a proposal and of an
// interrupt request, reaches the supervisor in its next wake, once, also
// across a restart (#650).
func TestAutonomyOutcomesWakeTheSupervisor(t *testing.T) {
	f := newSvAutonomy(t, "propose")
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	r := f.running(t, "r")
	n1 := f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply one"})
	n2 := f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply two"})
	if out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(r), "reason": "urgent"}); out.IsError {
		t.Fatalf("interrupt: %+v", out)
	}
	f.press(t, f.shown(t, n1), e2eHook, "Approve")
	f.press(t, f.shown(t, n2), e2eHook, "Refuse")
	f.press(t, f.buttons(t, fmt.Sprintf("Interrupt thread %d?", r)), e2eHook, "Refuse")
	_, p1 := f.decided(t, "proposal_answered", 2)
	_, p2 := f.decided(t, "approval_answered", 1)
	f.wt.ended(t, w, 2) // the approved send's turn
	f.wt.read(t, max(p1, p2))
	f.wt.quiet(t)
	wakes := f.wt.woken(t, f.owner, f.sv.ID, 1)
	for _, want := range []string{
		"3 of your requests were decided.",
		fmt.Sprintf("Your proposal %d, to send to thread %d: approved by dispatch, and sent.", n1, w),
		fmt.Sprintf("Your proposal %d, to send to thread %d: refused by dispatch; not sent.", n2, w),
		fmt.Sprintf("Your request to interrupt thread %d: refused by dispatch; nothing was interrupted.", r),
	} {
		if !strings.Contains(wakes[0], want) {
			t.Fatalf("the wake:\n%s\nwant %q", wakes[0], want)
		}
	}
	f.restart(t)
	time.Sleep(500 * time.Millisecond)
	if n := f.wt.clock().waiting(); n != 0 {
		t.Fatalf("after the restart, a wake waits: %d timers", n)
	}
	if got := f.wt.wakes(t, f.sv.ID); len(got) != 1 {
		t.Fatalf("%d wakes, want 1: %q", len(got), got)
	}
}

// Under act sends and starts run at once. Under act and under propose an
// interrupt only asks a person, and interrupts nothing by itself; under
// watch it is refused (TestAutonomyWatchRefuses) (#650).
func TestAutonomyActAndInterrupts(t *testing.T) {
	for _, mode := range []string{"act", "propose"} {
		t.Run(mode, func(t *testing.T) {
			f := newSvAutonomy(t, mode)
			r := f.running(t, "r")
			out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(r), "reason": "urgent"})
			if out.IsError || !strings.HasPrefix(out.Text, "asked a person") {
				t.Fatalf("interrupt: %+v", out)
			}
			f.buttons(t, fmt.Sprintf("Interrupt thread %d?", r))
			time.Sleep(300 * time.Millisecond)
			if d, err := f.owner.Thread(context.Background(), r, 0, 0); err != nil || d.State != control.Running ||
				len(threadEvents(t, f.owner, r, "turn_ended")) != 0 || f.fromSupervisor(t, r) != 0 {
				t.Fatalf("the running thread after the request: %+v %v", d, err)
			}
			if mode != "act" {
				return
			}
			w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
			if out := mcpCall(t, f.m, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply now"}); out.IsError ||
				out.Text != fmt.Sprintf("Sent to thread #%d.", w) {
				t.Fatalf("a send under act: %+v", out)
			}
			if got := f.fromSupervisor(t, w); got != 1 {
				t.Fatalf("%d inputs of the supervisor right after a send under act, want 1", got)
			}
			threads := f.threadsIn(t)
			if out := mcpCall(t, f.m, supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "text": "reply new"}); out.IsError ||
				!strings.HasPrefix(out.Text, "Started thread #") {
				t.Fatalf("a start under act: %+v", out)
			}
			if got := f.threadsIn(t); got != threads+1 {
				t.Fatalf("%d threads right after a start under act, want %d", got, threads+1)
			}
			if ev := threadEvents(t, f.owner, f.sv.ID, "proposal_made"); len(ev) != 0 {
				t.Fatalf("proposals under act: %v", ev)
			}
		})
	}
}

// A proposal counts once in sends_per_hour, when it is made: its run after
// a person approved it is not counted again. The count is the event log's,
// so a restart keeps it (#650).
func TestBudgetCountsAProposalOnce(t *testing.T) {
	f := newSvAutonomy(t, "propose", budget(0, 3, 0))
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	for i, text := range []string{"reply one", "reply two"} {
		n := f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": text})
		f.press(t, f.shown(t, n), e2eHook, "Approve")
		f.wt.ended(t, w, 2+i)
	}
	if got := f.fromSupervisor(t, w); got != 2 {
		t.Fatalf("%d sends of the supervisor ran, want 2", got)
	}
	f.restart(t)
	f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply three"})
	out := mcpCall(t, f.m, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply four"})
	if !out.IsError || !strings.HasPrefix(out.Text, "not allowed: over budget: 3 of 3 sends and starts this hour") {
		t.Fatalf("a fourth proposal: %+v", out)
	}
}

// A person's decision of one of the supervisor's requests in its thread is
// a person's answer there, so it resets the loop guard (#650).
func TestLoopGuardResetByADecision(t *testing.T) {
	f := newSvAutonomy(t, "act", budget(0, 0, 0), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["loop_cycles"] = 1
	})
	ctx := context.Background()
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	r := f.running(t, "r")
	if out := mcpCall(t, f.m, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply again"}); out.IsError {
		t.Fatalf("send: %+v", out)
	}
	f.wt.read(t, f.wt.ended(t, w, 2))
	guard := f.wt.waker.Gates[0].(*supervisor.LoopGuard)
	count := func() int {
		n, _, err := guard.Count(ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.wt.settle(t, "a cycle on the worker", func() bool { return count() == 1 })
	if out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(r), "reason": "urgent"}); out.IsError {
		t.Fatalf("interrupt: %+v", out)
	}
	f.press(t, f.buttons(t, fmt.Sprintf("Interrupt thread %d?", r)), e2eHook, "Refuse")
	f.decided(t, "approval_answered", 1)
	if n := count(); n != 0 {
		t.Fatalf("%d cycles on thread %d after a person's decision in the supervisor's thread, want 0", n, w)
	}
}
