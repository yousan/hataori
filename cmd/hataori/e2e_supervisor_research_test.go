package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #681 with the fake agent: a supervisor's
// research thread starts without the repository's settings, and a thread it
// started that fails wakes it.

// settingsDialog makes every workspace's start show a folder-trust dialog
// that names a project file nobody trusts, and have a project hook.
func settingsDialog(cfg map[string]any) {
	ads := cfg["adapters"].(map[string]any)
	c := ads[fake.Name].(fake.Config)
	c.Terminal, c.Hataori = true, os.Args[0]
	c.Dialog, c.DialogTrust, c.ProjectHook = "Do you trust the files in this folder? Enter to go on", []string{".claude/settings.json"}, true
	ads[fake.Name] = c
}

// startsA is the fake supervisor's script that starts a thread in repo with
// script, as research or not.
func startsA(repo, script string, research bool) string {
	return fmt.Sprintf("mcp start_thread {\"repo\": %q, \"text\": %q, \"research\": %v}", repo, script, research)
}

// ranHook reports whether the project's hook ran in thread id's workspace.
func (wt *wakeTest) ranHook(t *testing.T, id control.ThreadID) bool {
	t.Helper()
	var dir string
	if err := wt.db.QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(id)).Scan(&dir); err != nil || dir == "" {
		t.Fatalf("thread %d's workspace: %q, %v", id, dir, err)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(wt.r.root, dir)
	}
	_, err := os.Stat(filepath.Join(dir, fake.ProjectHookFile))
	return err == nil
}

// foldedFailure waits until thread id failed to start and the waker read it.
func (wt *wakeTest) foldedFailure(t *testing.T, id control.ThreadID) {
	t.Helper()
	wt.failed(t, id)
	var pos uint64
	if err := wt.db.QueryRow(`SELECT max(pos) FROM events WHERE thread_id = ? AND kind = 'state'`, int64(id)).Scan(&pos); err != nil {
		t.Fatal(err)
	}
	wt.read(t, pos)
	time.Sleep(300 * time.Millisecond) // for the waker to act on it
}

// failed waits until thread id failed to start.
func (wt *wakeTest) failed(t *testing.T, id control.ThreadID) {
	t.Helper()
	eventually(t, fmt.Sprintf("thread %d to fail", id), 20*time.Second, func() bool {
		var n int
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'state' AND json_extract(data, '$.to') = 'failed'`, int64(id)).Scan(&n)
		return n > 0
	})
}

// A supervisor's read-only research thread starts in a repository whose
// project settings the start-up dialog names, and runs its turns, also
// after a sleep, without the project's hook; an ordinary start of its there
// is blocked. Nobody else's start is made so.
func TestResearchThreadStartsWithoutTheSettings(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), settingsDialog)
	stop, owner, sv := wt.start(t)
	defer stop()
	if a := sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply looked it up", true)); !strings.HasPrefix(a, "mcp: Started thread") {
		t.Fatalf("start_thread: %s", a)
	}
	id := wt.newest(t, 1)
	wt.ended(t, id, 1)
	if slept, err := wt.waker.Sleep(context.Background(), id); err != nil || !slept {
		t.Fatalf("putting the research thread to sleep: %v, %v", slept, err)
	}
	if a := sendAndRead(t, owner, id, "again", "reply looked again"); a != "looked again" {
		t.Fatalf("its next turn: %q", a)
	}
	if wt.ranHook(t, id) {
		t.Fatal("the project's hook ran in a research thread")
	}
	sendAndRead(t, owner, sv.ID, "q2", startsA(wt.repo, "reply changed it", false))
	ordinary := wt.newest(t, id)
	wt.failed(t, ordinary)

	_, err := owner.Start(context.Background(), control.StartRequest{Key: "o", Scope: "c1", Thread: "t-o", Repo: wt.repo, Research: true,
		Input: control.Input{Text: "reply no"}})
	if !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("the owner's start as research: %v, want not allowed", err)
	}
	if n := wt.newest(t, id); n != ordinary {
		t.Fatalf("thread %d was started", n)
	}
}

// Under propose the approver is shown that the thread is research, without
// the repository's settings, and the approved start is made so.
func TestResearchThreadProposed(t *testing.T) {
	f := newSvAutonomy(t, "propose", settingsDialog)
	n := f.propose(t, supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "text": "reply looked it up", "research": true})
	shown := f.shown(t, n)
	if !strings.Contains(shown.Message.Text, "start a read-only research thread, without the repository's settings, in ") {
		t.Fatalf("the proposal: %s", shown.Message.Text)
	}
	// So is who approves elsewhere, as in the dashboard.
	if reqs := f.requests(t); len(reqs) != 1 || !strings.Contains(reqs[0], "start a read-only research thread, without the repository's settings, in ") {
		t.Fatalf("the thread's requests: %q", reqs)
	}
	f.press(t, shown, e2eHook, "Approve")
	id := f.wt.newest(t, 1)
	f.wt.ended(t, id, 1)
	if f.wt.ranHook(t, id) {
		t.Fatal("the project's hook ran in a research thread")
	}
	// Any other proposal of its is as before.
	n = f.propose(t, supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "text": "reply changed it"})
	if shown := f.shown(t, n); strings.Contains(shown.Message.Text, "research") {
		t.Fatalf("the proposal of an ordinary start: %s", shown.Message.Text)
	}
	if reqs := f.requests(t); len(reqs) != 1 || strings.Contains(reqs[0], "research") {
		t.Fatalf("the thread's requests: %q", reqs)
	}
}

// requests returns the texts of the supervisor's thread's open requests.
func (f *svAutonomy) requests(t *testing.T) []string {
	t.Helper()
	d, err := f.owner.Thread(context.Background(), f.sv.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range d.Requests {
		out = append(out, r.Text)
	}
	return out
}

// An approved proposal whose thread failed: the failure wakes the
// supervisor, and the decision does not say that the thread runs.
func TestApprovedProposalThatFails(t *testing.T) {
	f := newSvAutonomy(t, "propose", settingsDialog)
	n := f.propose(t, supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "text": "reply changed it"})
	f.press(t, f.shown(t, n), e2eHook, "Approve")
	id := f.wt.newest(t, 1)
	f.wt.failed(t, id)
	// In one wake or two, whichever is first.
	got := strings.Join(f.wt.wokenN(t, f.owner, f.sv.ID, 1), "\n")
	const failed, decided = "\nFailed: it could not start (blocked at start)", "\nDecided by a person:\n"
	if !strings.Contains(got, failed) || !strings.Contains(got, decided) {
		got = strings.Join(f.wt.wokenN(t, f.owner, f.sv.ID, 2), "\n")
	}
	if want := fmt.Sprintf(", but thread %d, which it made, has failed.", id); strings.Count(got, failed) != 1 || strings.Count(got, want) != 1 {
		t.Fatalf("the wakes:\n%s\nwant the failure once, and %q", got, want)
	}
	f.wt.noWake(t, f.sv.ID, time.Hour, strings.Count(got, "hataori: "), "after them")
}

// An agent that cannot leave the project's settings out is not started for
// a research thread: it fails, and the supervisor is told.
func TestResearchThreadOfAnAgentThatKeepsTheSettings(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), func(cfg map[string]any) {
		ads := cfg["adapters"].(map[string]any)
		c := ads[fake.Name].(fake.Config)
		c.ProjectHook, c.KeepsSettings = true, true
		ads[fake.Name] = c
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply looked it up", true))
	id := wt.newest(t, 1)
	wt.failed(t, id)
	if got := wt.woken(t, owner, sv.ID, 1); !strings.Contains(got[0], "cannot start without the repository's settings") {
		t.Fatalf("the wake:\n%s", got[0])
	}
}

// A thread whose turn end the budget holds and that then cannot start
// again: the failure's wake gives the turn end and its output with it, and
// the next hour, also after a restart, neither again.
func TestFailedThreadWithATurnEndHeld(t *testing.T) {
	wt := newWakeTest(t, budget(2, 0, 0), autonomy("act"))
	stop, owner, sv := wt.start(t)
	defer func() { stop() }()
	ctx := context.Background()
	sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply first", false))
	w := wt.newest(t, 1)
	wt.read(t, wt.ended(t, w, 1))
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1) // past the hour (#677)
	wt.again(t, owner, w, "w-2", "reply the important result", 2)
	eventually(t, "the hold alone", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
	if slept, err := wt.waker.Sleep(ctx, w); err != nil || !slept {
		t.Fatalf("sleep: %v, %v", slept, err)
	}
	var dir string
	wt.db.QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(w)).Scan(&dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(wt.r.root, dir)
	}
	if err := os.Rename(dir, dir+"-gone"); err != nil {
		t.Fatal(err)
	}
	owner.Send(ctx, w, "w-3", control.Input{Text: "reply no"}, control.Refuse)
	wt.failed(t, w)
	got := wt.woken(t, owner, sv.ID, 2)
	if !strings.Contains(got[1], "> the important result\nThen it failed: it could not start again (") {
		t.Fatalf("the wake:\n%s", got[1])
	}
	stop()
	stop, owner, sv = wt.start(t)
	wt.noWake(t, sv.ID, 10*time.Minute, 2, "after a restart")
	wt.until(t, wakeBase.Add(time.Hour))
	wt.noWake(t, sv.ID, 10*time.Minute, 2, "in the next hour")
}

// A failure not given yet, as the supervisor's own turn runs, is not given
// once a person closed the thread.
func TestFailedThreadThatIsClosed(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), settingsDialog)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	if err := owner.Send(ctx, sv.ID, "q1", control.Input{Text: startsA(wt.repo, "reply no", false) + "\ntool hold 6s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	id := wt.newest(t, 1)
	wt.foldedFailure(t, id)
	// Its agent may still be ending, which the close says; it is closed then.
	closeErr := owner.CloseThread(ctx, id)
	if d, err := owner.Thread(ctx, id, 0, 0); err != nil || d.State != control.Closed {
		t.Fatalf("closing the thread: %v; %s, %v", closeErr, d.State, err)
	}
	if d, err := owner.Thread(ctx, sv.ID, 0, 0); err != nil || d.State != control.Running {
		t.Fatalf("the supervisor's turn is over (%s, %v): the machine is too slow for this test", d.State, err)
	}
	wt.read(t, wt.ended(t, sv.ID, 2))
	time.Sleep(time.Second) // for the waker to act on it
	wt.noWake(t, sv.ID, 10*time.Minute, 0, "after the thread was closed")
}

// A thread the supervisor started for a person that fails to start wakes it
// once, at once and past a used-up hour, with what failed and who asked; so
// does a research thread that fails, across a restart too. What the budget
// holds comes in the next hour, without them.
func TestFailedThreadWakesTheSupervisorOnce(t *testing.T) {
	missing := "file:///nonexistent/" + t.Name() + ".git"
	wt := newWakeTest(t, budget(2, 0, 0), autonomy("act"), settingsDialog, func(cfg map[string]any) {
		sv := cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)
		sv["repos"] = append(sv["repos"].([]string), missing)
	})
	stop, owner, sv := wt.start(t) // its first turn is one of the hour's
	defer func() { stop() }()
	sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply the answer", false)) // and this one the other
	blocked := wt.newest(t, 1)
	wt.failed(t, blocked)
	got := wt.woken(t, owner, sv.ID, 1) // no clock moved: no quiet period
	want := fmt.Sprintf("hataori: a thread you started failed.\n\nThread %d: reply the answer\nFailed: it could not start (blocked at start), so no turn ran.\nWhat it showed:\n> the dialog names .claude/settings.json; this session may not trust them\n"+
		"Asked for by boss, who waits for an answer: tell @boss what failed, and what you do instead.\n", blocked)
	if got[0] != want {
		t.Fatalf("the wake:\n%s\nwant:\n%s", got[0], want)
	}
	wt.noWake(t, sv.ID, 5*time.Minute, 1, "after it")

	// A turn end is held, and a research thread fails.
	if a := sendAndRead(t, owner, sv.ID, "q2", startsA(missing, "reply looked it up", true)); !strings.HasPrefix(a, "mcp error: failed: ") {
		t.Fatalf("start_thread in a repository that is not there: %s", a)
	}
	research := wt.newest(t, blocked)
	wt.failed(t, research)
	got = wt.woken(t, owner, sv.ID, 2)
	if !strings.HasPrefix(got[1], fmt.Sprintf("hataori: a thread you started failed.\n\nThread %d: reply looked it up\nFailed: it could not start (start: ", research)) ||
		!strings.Contains(got[1], "tell @boss what failed") || names(got[1], blocked) {
		t.Fatalf("the wake of the research thread:\n%s", got[1])
	}
	stop()
	stop, owner, sv = wt.start(t)
	wt.noWake(t, sv.ID, 20*time.Minute, 2, "after a restart")
	wt.until(t, wakeBase.Add(time.Hour))
	wt.noWake(t, sv.ID, 10*time.Minute, 2, "in the next hour")
}

// A failed start while the budget holds a turn end: the failure is given
// alone, and after a restart and the hour the held one comes without it. A
// thread started in a wake's turn, which no person asked for, is told too.
func TestFailedThreadPastAHold(t *testing.T) {
	wt := newWakeTest(t, budget(2, 0, 0), autonomy("act"), settingsDialog, func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
	})
	stop, owner, sv := wt.start(t)
	defer func() { stop() }()
	// A worker that runs: research, started by a person's message.
	sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply "+startsA(wt.repo, "reply nobody asked", false), true))
	w := wt.newest(t, 1)
	pos := wt.ended(t, w, 1)
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1) // past the hour (#677); in it the supervisor starts a thread
	unasked := wt.newest(t, w)
	wt.failed(t, unasked)
	got := wt.woken(t, owner, sv.ID, 2)
	if !names(got[1], unasked) || !strings.Contains(got[1], "\nFailed: it could not start (blocked at start)") || strings.Contains(got[1], "Asked for by") {
		t.Fatalf("the wake of a thread nobody asked for:\n%s", got[1])
	}
	// A turn end that no person waits for any more: the hold takes it, with
	// no quiet period.
	wt.again(t, owner, w, "w-2", "reply later", 2)
	eventually(t, "the hold alone", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
	// It fails while the supervisor's turn still runs: told right after it.
	if err := owner.Send(context.Background(), sv.ID, "q2", control.Input{Text: startsA(wt.repo, "reply the answer", false) + "\ntool hold 5s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	blocked := wt.newest(t, unasked)
	wt.failed(t, blocked)
	if d, err := owner.Thread(context.Background(), sv.ID, 0, 0); err != nil || d.State != control.Running {
		t.Fatalf("the supervisor's turn is over (%s, %v): the machine is too slow for this test", d.State, err)
	}
	got = wt.woken(t, owner, sv.ID, 3)
	if !names(got[2], blocked) || names(got[2], w) {
		t.Fatalf("the wake past the hold:\n%s", got[2])
	}
	stop()
	stop, owner, sv = wt.start(t)
	wt.noWake(t, sv.ID, 10*time.Minute, 3, "after a restart")
	wt.until(t, wakeBase.Add(time.Hour))
	got = wt.wokenN(t, owner, sv.ID, 4)
	if !names(got[3], w) || names(got[3], blocked) || names(got[3], unasked) || strings.Contains(got[3], "failed") {
		t.Fatalf("the wake of the next hour:\n%s", got[3])
	}
	eventually(t, "what went past the hold to be forgotten", 10*time.Second, func() bool {
		var n int
		return wt.db.QueryRow(`SELECT count(*) FROM supervisor_given`).Scan(&n) == nil && n == 0
	})
}

// A failed start's wake that the daemon had stored, or sent, when it died is
// given once.
func TestFailedThreadOnceAcrossARestart(t *testing.T) {
	for _, step := range []string{"stored", "sent"} {
		t.Run(step, func(t *testing.T) {
			wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), settingsDialog)
			stopped := errors.New("stopped here")
			h := func(at string) error {
				if at == step {
					return stopped
				}
				return nil
			}
			wt.hook.Store(&h)
			stop, owner, sv := wt.start(t)
			sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply the answer", false))
			wt.failed(t, wt.newest(t, 1))
			eventually(t, "the wake to be "+step, 10*time.Second, func() bool {
				var n int
				return wt.db.QueryRow(`SELECT count(*) FROM supervisor_wakes WHERE send_to IS NOT NULL`).Scan(&n) == nil && n == 1
			})
			if step == "sent" {
				wt.woken(t, owner, sv.ID, 1) // and its turn ended, its position not stored
			}
			stop()
			stop, owner, sv = wt.start(t)
			defer stop()
			if got := wt.woken(t, owner, sv.ID, 1); !strings.Contains(got[0], "\nFailed: it could not start") {
				t.Fatalf("the wake:\n%s", got[0])
			}
			wt.noWake(t, sv.ID, time.Hour, 1, "after the restart")
		})
	}
}

// withoutSettings makes the choice for a blocked start as the owner.
func (wt *wakeTest) withoutSettings(t *testing.T, owner *wire.Client, id control.ThreadID) {
	t.Helper()
	var session int64
	if err := wt.db.QueryRow(`SELECT max(id) FROM sessions WHERE thread_id = ?`, int64(id)).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if err := owner.StartWithoutSettings(context.Background(), id, session); err != nil {
		t.Fatal(err)
	}
}

// A thread that failed to start and that a person starts after all still is
// the one that person asked for: the failure's wake leaves #677's mark.
func TestAskedThreadThatFailedFirst(t *testing.T) {
	wt := newWakeTest(t, budget(2, 0, 0), autonomy("act"), settingsDialog)
	stop, owner, sv := wt.start(t)
	defer stop()
	sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply the answer", false)) // the hour is used up
	id := wt.newest(t, 1)
	wt.failed(t, id)
	wt.woken(t, owner, sv.ID, 1)
	wt.withoutSettings(t, owner, id)
	wt.read(t, wt.ended(t, id, 1))
	wt.quietly(t)
	if got := wt.woken(t, owner, sv.ID, 2); !names(got[1], id) || !strings.Contains(got[1], "> the answer") {
		t.Fatalf("the wake of its first turn end:\n%s", got[1])
	}
}

// A failure not given yet, as the supervisor's own turn runs, is not given
// once the thread runs after all.
func TestFailedThreadThatRunsAfterAll(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), settingsDialog)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	if err := owner.Send(ctx, sv.ID, "q1", control.Input{Text: startsA(wt.repo, "tool work 20s", false) + "\ntool hold 6s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	id := wt.newest(t, 1)
	wt.foldedFailure(t, id)
	wt.withoutSettings(t, owner, id)
	eventually(t, "the thread to run", 10*time.Second, func() bool {
		d, err := owner.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Running
	})
	if d, err := owner.Thread(ctx, sv.ID, 0, 0); err != nil || d.State != control.Running {
		t.Fatalf("the supervisor's turn is over (%s, %v): the machine is too slow for this test", d.State, err)
	}
	wt.read(t, wt.ended(t, sv.ID, 2))
	time.Sleep(time.Second) // for the waker to act on it
	wt.noWake(t, sv.ID, 10*time.Minute, 0, "while the thread runs")
}

// Nothing else that fails wakes it: a thread a person started, and a session
// that could not resume, which another replaced.
func TestOtherFailuresDoNotWakeTheSupervisor(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), settingsDialog)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	id, err := owner.Start(ctx, control.StartRequest{Key: "p", Scope: "c1", Thread: "t-p", Repo: wt.repo, Input: control.Input{Text: "reply no"}})
	if err != nil {
		t.Fatal(err)
	}
	wt.failed(t, id)
	// A research thread of the supervisor's loses its conversation.
	sendAndRead(t, owner, sv.ID, "q1", startsA(wt.repo, "reply looked it up", true))
	w := wt.newest(t, id)
	wt.read(t, wt.ended(t, w, 1))
	wt.wokenN(t, owner, sv.ID, 1)
	if slept, err := wt.waker.Sleep(ctx, w); err != nil || !slept {
		t.Fatalf("sleep: %v, %v", slept, err)
	}
	var agentSession string
	wt.db.QueryRow(`SELECT agent_session_id FROM sessions WHERE thread_id = ?`, int64(w)).Scan(&agentSession)
	if err := os.Remove(filepath.Join(wt.conv, filepath.Base(agentSession))); err != nil {
		t.Fatal(err)
	}
	if err := owner.Send(ctx, w, "again", control.Input{Text: "reply looked again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.failed(t, w)
	wt.read(t, wt.ended(t, w, 2))
	got := wt.wokenN(t, owner, sv.ID, 2)
	if strings.Contains(got[1], "ailed") || !strings.Contains(got[1], "\nEnded: completed.") {
		t.Fatalf("the wake after the replaced session's turn:\n%s", got[1])
	}
	wt.noWake(t, sv.ID, time.Hour, 2, "after it")
}
