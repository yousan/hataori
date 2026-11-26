package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// Real control connections, fake agents and a fake surface exercise the
// origin recorded at acceptance through the resumed conversation's reply.
func TestWorkerWakesItsConversationHome(t *testing.T) {
	f := newSvTalks(t, "act", budget(0, 0, 0))
	ctx := context.Background()
	talk := f.talk(t, "home")
	_, conversation, _, _ := f.wt.conversation(t, talk)
	if slept, err := f.wt.waker.Sleep(ctx, talk); err != nil || !slept {
		t.Fatalf("sleep home: %v, %v", slept, err)
	}
	api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	w, err := api.Start(ctx, control.StartRequest{Scope: "c1", Key: "home-worker", Repo: f.wt.repo, Input: control.Input{Text: "reply home-result"}})
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 1)
	f.wt.quiet(t)
	got := f.wt.woken(t, f.owner, talk, 1)
	if !strings.Contains(got[0], "> home-result") {
		t.Fatalf("home digest: %q", got)
	}
	if own := f.wt.wakes(t, f.sv.ID); len(own) != 0 {
		t.Fatalf("own thread also woken: %q", own)
	}
	if _, resumed, _, _ := f.wt.conversation(t, talk); resumed != conversation {
		t.Fatalf("home resumed %q, want %q", resumed, conversation)
	}
	var aside int
	if err := f.wt.db.QueryRow(`SELECT max(aside) FROM turns WHERE session_id IN (SELECT id FROM sessions WHERE thread_id = ?)`, talk).Scan(&aside); err != nil || aside != 0 {
		t.Fatalf("conversation wake ran fresh: aside=%d, %v", aside, err)
	}
	// A person writes directly to the worker; its existing home stays.
	if err := f.owner.Send(ctx, w, "person-worker", control.Input{Text: "reply person-result"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 2)
	f.wt.quiet(t)
	f.wt.woken(t, f.owner, talk, 2)
	// An own-thread send moves the home; its next end wakes only that one.
	ownAPI, err := wire.DialToken(f.wt.r.root, f.token)
	if err != nil {
		t.Fatal(err)
	}
	defer ownAPI.Close()
	if err := ownAPI.Send(ctx, w, "own-worker", control.Input{Text: "reply own-result"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 3)
	f.wt.quiet(t)
	f.wt.woken(t, f.owner, f.sv.ID, 1)
	if got := f.wt.wakes(t, talk); len(got) != 2 {
		t.Fatalf("former home woken: %q", got)
	}
	// The reply of each wake is posted in that conversation's chat thread.
	eventually(t, "wake reply in the home chat thread", 10*time.Second, func() bool {
		for _, call := range f.wt.r.driver.Calls(f.chat("home")) {
			if call.Op == fakesurface.OpPost && call.Message.Text == "done" {
				return true
			}
		}
		return false
	})
}

func TestBlockedHomeRetainsItsEndAcrossAnotherHomesWakeAndRestart(t *testing.T) {
	f := newSvTalks(t, "act", budget(0, 0, 0))
	ctx := context.Background()
	talk := f.talk(t, "blocked-home")
	api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if err := f.owner.Send(ctx, talk, "question", control.Input{Text: "ask proceed? -- yes | no"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "home waiting on question", 10*time.Second, func() bool {
		d, err := f.owner.Thread(ctx, talk, 0, 0)
		return err == nil && d.State == control.Waiting
	})
	w, err := api.Start(ctx, control.StartRequest{Scope: "c1", Key: "blocked-worker", Repo: f.wt.repo, Input: control.Input{Text: "reply original-home-only"}})
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 1)
	f.wt.quiet(t)
	// Move the same worker to the own thread, and deliver its newer end first.
	ownAPI, err := wire.DialToken(f.wt.r.root, f.token)
	if err != nil {
		t.Fatal(err)
	}
	defer ownAPI.Close()
	if err := ownAPI.Send(ctx, w, "new-home", control.Input{Text: "reply own-home-only"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 2)
	f.wt.quiet(t)
	f.wt.woken(t, f.owner, f.sv.ID, 1)
	if got := f.wt.wakes(t, talk); len(got) != 0 {
		t.Fatalf("wake answered question: %q", got)
	}
	f.restart(t)
	d, err := f.owner.Thread(ctx, talk, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("question after restart: %+v, %v", d, err)
	}
	if err := f.owner.Answer(ctx, d.Requests[0].ID, control.Answer{Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	// A recovered reader may still be finishing its quiet period.
	f.wt.settle(t, "the original home's pending wake", func() bool { return len(f.wt.wakes(t, talk)) == 1 })
	got := f.wt.woken(t, f.owner, talk, 1)
	if !strings.Contains(got[0], "> original-home-only") || strings.Contains(got[0], "own-home-only") {
		t.Fatalf("wrong home's output: %q", got)
	}
	if got := f.wt.wakes(t, f.sv.ID); len(got) != 1 {
		t.Fatalf("own woken again: %q", got)
	}
}

func TestConversationWakeOnceAcrossEveryDeliveryBoundary(t *testing.T) {
	for _, step := range []string{"quiet", "stored", "sent"} {
		t.Run(step, func(t *testing.T) {
			f := newSvTalks(t, "act", budget(0, 0, 0))
			talk := f.talk(t, "restart-home")
			api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			var reached atomic.Bool
			// Hooks are assigned before the next daemon creates child readers.
			hook := func(at string) error {
				if at == step {
					reached.Store(true)
					return errors.New("test crash boundary")
				}
				return nil
			}
			if step != "quiet" {
				f.wt.hook.Store(&hook)
			}
			f.restart(t)
			api.Close()
			api, err = wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			w, err := api.Start(context.Background(), control.StartRequest{Scope: "c1", Key: "restart-worker", Repo: f.wt.repo, Input: control.Input{Text: "reply retained-result"}})
			if err != nil {
				t.Fatal(err)
			}
			pos := f.wt.ended(t, w, 1)
			if step == "quiet" {
				eventually(t, "pending quiet timer", 10*time.Second, func() bool { return f.wt.clock().waiting() > 0 })
			} else {
				f.wt.quiet(t)
				eventually(t, "delivery crash boundary", 10*time.Second, reached.Load)
			}
			if step == "sent" {
				f.wt.woken(t, f.owner, talk, 1)
			}
			f.restart(t)
			f.wt.settle(t, "one recovered home wake", func() bool { return len(f.wt.wakes(t, talk)) == 1 })
			f.wt.woken(t, f.owner, talk, 1)
			eventually(t, "home wake cursor committed", 10*time.Second, func() bool {
				var cursor uint64
				var pending any
				err := f.wt.db.QueryRow(`SELECT pos, send_to FROM supervisor_conversation_wakes WHERE scope = 'c1' AND thread = ?`, talk).Scan(&cursor, &pending)
				return err == nil && pending == nil && cursor >= pos
			})
			f.restart(t)
			f.wt.hour()
			if got := f.wt.wakes(t, talk); len(got) != 1 {
				t.Fatalf("after second restart: %q", got)
			}
			if got := f.wt.wakes(t, f.sv.ID); len(got) != 0 {
				t.Fatalf("own thread woke: %q", got)
			}
		})
	}
}

func TestClosedConversationHomesFallBackIndependently(t *testing.T) {
	f := newSvTalks(t, "act", budget(0, 0, 0))
	ctx := context.Background()
	var workers []control.ThreadID
	for i := 0; i < 2; i++ {
		talk := f.talk(t, fmt.Sprintf("closed-home-%d", i))
		api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
		if err != nil {
			t.Fatal(err)
		}
		w, err := api.Start(ctx, control.StartRequest{Scope: "c1", Key: fmt.Sprintf("closed-worker-%d", i), Repo: f.wt.repo, Input: control.Input{Text: fmt.Sprintf("reply closed-result-%d", i)}})
		api.Close()
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
		f.wt.ended(t, w, 1)
		if err := f.owner.CloseThread(ctx, talk); err != nil {
			t.Fatal(err)
		}
	}
	// Both independent readers catch up to the same scope boundary.
	f.restart(t)
	f.wt.settle(t, "both closed homes' fallback wakes", func() bool { return len(f.wt.wakes(t, f.sv.ID)) == 2 })
	got := f.wt.woken(t, f.owner, f.sv.ID, 2)
	for i := range workers {
		if !strings.Contains(strings.Join(got, "\n"), fmt.Sprintf("> closed-result-%d", i)) {
			t.Fatalf("missing fallback %d: %q", i, got)
		}
	}
	var aside int
	if err := f.wt.db.QueryRow(`SELECT count(*) FROM turns WHERE session_id IN (SELECT id FROM sessions WHERE thread_id = ?) AND aside = 1`, f.sv.ID).Scan(&aside); err != nil || aside != 2 {
		t.Fatalf("fallback did not use fresh own conversations: %d, %v", aside, err)
	}
}

func TestQueuedHomeWakeSurvivesClosureAndDoesNotHoldOwnThread(t *testing.T) {
	f := newSvTalks(t, "act", budget(0, 0, 0), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["conversations_at_once"] = 1
	})
	ctx := context.Background()
	home := f.talk(t, "queued-home")
	busy := f.talk(t, "busy-home")
	if err := f.owner.Send(ctx, busy, "occupy-place", control.Input{Text: "tool t 30s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.busy(t, busy)
	api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, home))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	w, err := api.Start(ctx, control.StartRequest{Scope: "c1", Key: "queued-worker", Repo: f.wt.repo, Input: control.Input{Text: "reply queued-result"}})
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 1)
	f.wt.quiet(t)
	eventually(t, "home wake waiting for a place", 10*time.Second, func() bool {
		var n int
		f.wt.db.QueryRow(`SELECT count(*) FROM waiting_inputs WHERE thread_id = ? AND wake = 1`, home).Scan(&n)
		return n == 1
	})
	_, pos := f.wt.worker(t, f.owner, "c1", "independent-own-worker", "reply independent-own-result")
	f.wt.read(t, pos)
	f.wt.quiet(t)
	f.wt.woken(t, f.owner, f.sv.ID, 1)
	f.restart(t)
	if err := f.owner.CloseThread(ctx, home); err != nil {
		t.Fatal(err)
	}
	f.wt.settle(t, "closed queued home's fallback", func() bool { return len(f.wt.wakes(t, f.sv.ID)) == 2 })
	got := f.wt.woken(t, f.owner, f.sv.ID, 2)
	if !strings.Contains(got[1], "> queued-result") {
		t.Fatalf("lost queued output: %q", got)
	}
	if got := f.wt.wakes(t, home); len(got) != 0 {
		t.Fatalf("closed queued home ran: %q", got)
	}
}

func TestConversationHomeBackgroundWakeDoesNotRepeatAfterRestart(t *testing.T) {
	f := newSvTalks(t, "act", budget(0, 0, 0))
	talk := f.talk(t, "background-home")
	api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	w, err := api.Start(context.Background(), control.StartRequest{Scope: "c1", Key: "background-worker", Repo: f.wt.repo, Input: control.Input{Text: "background 10h\nreply background-result"}})
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, w, 1)
	f.wt.steps(61 * time.Minute)
	got := f.wt.wokenN(t, f.owner, talk, 1)
	if !strings.Contains(got[0], staleLine) {
		t.Fatalf("not a stale background wake: %q", got)
	}
	f.restart(t)
	f.wt.noWake(t, talk, 2*time.Hour, 1, "stale conversation wake after restart")
}

func TestFailedWorkerStartWakesItsConversationHome(t *testing.T) {
	f := newSvTalks(t, "act", budget(0, 0, 0), settingsDialog)
	talk := f.talk(t, "failure-home")
	api, err := wire.DialToken(f.wt.r.root, tokenOf(t, f.wt.r.root, talk))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	w, _ := api.Start(context.Background(), control.StartRequest{Scope: "c1", Key: "failed-home-worker", Repo: f.wt.repo, Input: control.Input{Text: "reply cannot-start"}})
	if w == 0 {
		t.Fatal("start did not create its worker")
	}
	f.wt.failed(t, w)
	f.wt.settle(t, "failed start to wake its home", func() bool { return len(f.wt.wakes(t, talk)) == 1 })
	got := f.wt.woken(t, f.owner, talk, 1)
	if !strings.Contains(got[0], "Failed: it could not start") {
		t.Fatalf("failure missing: %q", got)
	}
	if got := f.wt.wakes(t, f.sv.ID); len(got) != 0 {
		t.Fatalf("failure woke own thread: %q", got)
	}
}
