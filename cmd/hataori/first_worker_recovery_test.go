package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workerssh"
)

func (f *firstWorker) client() *wire.Client {
	f.t.Helper()
	c, err := wire.Dial(f.main.root)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.Close() })
	return c
}

func (f *firstWorker) ready(c *wire.Client, id control.ThreadID) {
	f.t.Helper()
	eventually(f.t, "worker output/receipts caught up", 15*time.Second, func() bool {
		d, err := c.Thread(context.Background(), id, 0, 0)
		return err == nil && d.Availability != nil && d.Availability.State == "ready" && d.IOPaused == ""
	})
}

func (f *firstWorker) start(c *wire.Client, key, text, machine string) (control.ThreadID, surface.Conversation) {
	f.t.Helper()
	id, err := c.Start(context.Background(), control.StartRequest{Key: key, Scope: "c1", Machine: machine, Repo: f.repo, Input: control.Input{Text: text}})
	if err != nil {
		f.t.Fatal(err)
	}
	var conv surface.Conversation
	eventually(f.t, "worker API thread's chat link", 10*time.Second, func() bool {
		d, err := c.Thread(context.Background(), id, 0, 0)
		if err == nil && len(d.Links) > 0 {
			conv = surface.Conversation{Channel: d.Links[0].Channel, Thread: d.Links[0].Thread}
			return true
		}
		return false
	})
	return id, conv
}

func (f *firstWorker) inputCount(text string) int {
	f.t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.root, "records/inputs-*.jsonl"))
	if err != nil {
		f.t.Fatal(err)
	}
	count := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var in struct{ Text string }
			if json.Unmarshal(line, &in) == nil && in.Text == text {
				count++
			}
		}
	}
	return count
}

func (f *firstWorker) turnCount(id control.ThreadID, want int) {
	f.t.Helper()
	db := f.main.db(f.t)
	eventually(f.t, "all original worker turn ends committed", 10*time.Second, func() bool {
		var starts, ends int
		err := db.QueryRow(`SELECT count(*),count(ended_at) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, id).Scan(&starts, &ends)
		return err == nil && starts == want && ends == want
	})
}

func TestFirstWorkerCapacityScopeBindingAndOwnerAttach(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	ctx := context.Background()
	first, a := f.start(c, "capacity-one", "reply first slot", "build")
	f.main.posted(t, a, "first slot")
	if _, err := c.Start(ctx, control.StartRequest{Key: "scope-excluded", Scope: "excluded", Machine: "build", Repo: f.repo, Input: control.Input{Text: "reply forbidden excluded scope"}}); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("scope exclusion while capacity exists: %v", err)
	}
	_, b := f.start(c, "capacity-two", "reply second slot", "build")
	f.main.posted(t, b, "second slot")
	for _, request := range []control.StartRequest{
		{Key: "explicit-full", Scope: "c1", Machine: "build", Repo: f.repo, Input: control.Input{Text: "reply forbidden extra slot"}},
	} {
		if _, err := c.Start(ctx, request); err == nil {
			t.Fatalf("ineligible explicit start accepted: %+v", request)
		}
	}
	waiting, waitingConv := f.start(c, "automatic-wait", "reply waited for capacity", "")
	repeat, err := c.Start(ctx, control.StartRequest{Key: "automatic-wait", Scope: "c1", Repo: f.repo, Input: control.Input{Text: "reply waited for capacity"}})
	if err != nil || repeat != waiting {
		t.Fatalf("waiting key repeated: %d %d %v", waiting, repeat, err)
	}
	info, err := c.Thread(ctx, waiting, 0, 0)
	if err != nil || !info.PlacementPending || info.Machine != "" || f.inputCount("reply waited for capacity") != 0 {
		t.Fatalf("automatic start did not wait without a fallback: %+v %v", info.ThreadInfo, err)
	}
	c.Close()
	f.main.restart(t)
	c = f.client()
	info, err = c.Thread(ctx, waiting, 0, 0)
	if err != nil || !info.PlacementPending {
		t.Fatalf("waiting placement lost across actual main restart: %+v %v", info.ThreadInfo, err)
	}
	f.ready(c, first)
	// This is the actual owner CLI path, using the worker's own terminal.
	oldStdin := attachStdin
	attachStdin = bytes.NewReader([]byte{detachKey})
	defer func() { attachStdin = oldStdin }()
	code, _, stderr := cli(t, "attach", "--root", f.main.root, fmtThread(first))
	if code != 0 || !strings.Contains(stderr, "attached to thread") {
		t.Fatalf("owner CLI worker attachment: %d %s", code, stderr)
	}
	pool, err := workerssh.New(ctx, workerssh.Options{RunDir: filepath.Join(t.TempDir(), "wrong-binding"), ProbeInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	wrong := f.binding
	wrong.Worker = "not-the-registered-installation"
	w, err := pool.Worker(workerssh.Config{Destination: "first-worker", Root: f.root, Binding: wrong})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "wrong-worker binding refusal through SSH", 10*time.Second, func() bool {
		s := w.Status()
		return !s.Ready && s.Cause == workerssh.Binding
	})
	if err := c.CloseThread(ctx, first); err != nil {
		t.Fatal(err)
	}
	f.main.posted(t, waitingConv, "waited for capacity")
	info, err = c.Thread(ctx, waiting, 0, 0)
	if err != nil || info.Machine != "build" || info.PlacementPending || f.inputCount("reply waited for capacity") != 1 {
		t.Fatalf("waiting start did not acquire exactly one worker slot: %+v %v", info.ThreadInfo, err)
	}
	if f.main.driver.Runs() != 1 {
		t.Fatal("more than one chat client owns the fake surface")
	}
	t.Log("ran: explicit capacity/scope refusal; automatic waiting/key dedup across real main restart; checked owner CLI SSH attachment; wrong installation refusal; one worker launch after slot release")
}

func fmtThread(id control.ThreadID) string { return strconv.FormatInt(int64(id), 10) }

func TestFirstWorkerMainRestartMidTurn(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	id, conv := f.start(c, "restart-mid-turn", "tool wait 4s\nreply worker survived main", "build")
	db := f.main.db(t)
	eventually(t, "worker turn before main restart", 10*time.Second, func() bool {
		var n int
		db.QueryRow(`SELECT count(*) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=? AND x.ended_at IS NULL`, id).Scan(&n)
		return n == 1 && f.inputCount("tool wait 4s\nreply worker survived main") == 1
	})
	eventually(t, "worker tool is active", 10*time.Second, func() bool { return len(f.main.events(t, "tool_started", conv)) == 1 })
	c.Close()
	f.main.kill(t)
	if len(f.main.events(t, "tool_ended", conv)) != 0 {
		t.Fatal("tool finished before main was killed")
	}
	f.main.start(t)
	f.main.posted(t, conv, "worker survived main")
	f.turnCount(id, 1)
	if f.main.count(conv, "worker survived main") != 1 {
		t.Fatal("restart duplicated visible worker reply")
	}
	var starts, ends int
	db.QueryRow(`SELECT count(*),count(ended_at) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, id).Scan(&starts, &ends)
	if starts != 1 || ends != 1 || f.inputCount("tool wait 4s\nreply worker survived main") != 1 {
		t.Fatalf("main restart repeated worker input/end: %d %d", starts, ends)
	}
	t.Log("ran: actual owned main PID killed mid-turn; resident/PTY agent continued; same thread/directory, one input and one end")
}

func TestFirstWorkerLostWriteReplyDedupAndCatchup(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	ctx := context.Background()
	id, conv := f.start(c, "uncertain-write", "reply before uncertainty", "build")
	f.main.posted(t, conv, "before uncertainty")
	f.turnCount(id, 1)
	f.ready(c, id)
	f.proxy.duplicate.Store(true)
	f.proxy.dropInput.Store(true)
	f.proxy.hideReceipt.Store(true)
	// Send may acknowledge a queued codec input before the transport write.
	// The proxy counter below proves that the completed worker reply was lost.
	_ = c.Send(ctx, id, "uncertain-once", control.Input{Text: "reply dedup worker"}, control.Deliver)
	eventually(t, "one applied write with deliberately lost reply", 10*time.Second, func() bool { return f.proxy.dropped.Load() == 1 && f.inputCount("reply dedup worker") >= 1 })
	c.Close()
	f.main.restart(t)
	c = f.client()
	eventually(t, "unknown receipt blocks catch-up after restart", 10*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		return err == nil && d.Availability != nil && d.Availability.State != "ready" && strings.Contains(d.IOPaused, "unknown")
	})
	if _, err := c.Start(ctx, control.StartRequest{Key: "not-before-catchup", Scope: "c1", Machine: "build", Repo: f.repo, Input: control.Input{Text: "reply must wait for original receipt"}}); err == nil {
		t.Fatal("new worker start bypassed unresolved catch-up")
	}
	if err := c.Send(ctx, id, "held-after-unknown", control.Input{Text: "reply after original receipt"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	if f.inputCount("reply after original receipt") != 0 {
		t.Fatal("held input reached the worker before original receipt reconciliation")
	}
	f.proxy.hideReceipt.Store(false)
	f.main.posted(t, conv, "dedup worker")
	f.main.posted(t, conv, "after original receipt")
	f.turnCount(id, 3)
	if f.main.count(conv, "dedup worker") != 1 || f.main.count(conv, "after original receipt") != 1 {
		t.Fatal("catch-up duplicated visible worker replies")
	}
	if f.proxy.duplicates.Load() != 1 || f.inputCount("reply dedup worker") != 1 || f.inputCount("reply after original receipt") != 1 {
		t.Fatal("effect deduplication or ordered catch-up failed")
	}
	db := f.main.db(t)
	var starts, ends int
	db.QueryRow(`SELECT count(*),count(ended_at) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, id).Scan(&starts, &ends)
	if starts != 3 || ends != 3 {
		t.Fatalf("uncertain write repeated turn ends: %d %d", starts, ends)
	}
	t.Log("ran: identical worker input operation duplicated, complete ACK lost, receipt hidden across actual main restart; catch-up/new work blocked; original receipt restored; one effect and ordered held delivery")
}

func (f *firstWorker) away(c *wire.Client, id control.ThreadID) {
	f.t.Helper()
	eventually(f.t, "verified worker unavailability", 15*time.Second, func() bool {
		d, err := c.Thread(context.Background(), id, 0, 0)
		return err == nil && d.Availability != nil && d.Availability.State == "away"
	})
}

func TestFirstWorkerCutHeldInputAndStaleNamedStop(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	ctx := context.Background()
	id, conv := f.start(c, "cut-original", "tool wait 3s\nreply buffered before autonomous", "build")
	eventually(t, "original worker tool", 10*time.Second, func() bool { return len(f.main.events(t, "tool_started", conv)) == 1 })
	db := f.main.db(t)
	var original int64
	if err := db.QueryRow(`SELECT max(x.id) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, id).Scan(&original); err != nil {
		t.Fatal(err)
	}
	f.proxy.cut()
	f.away(c, id)
	for i := 0; i < 2; i++ {
		if err := c.Send(ctx, id, "held-cut", control.Input{Text: "reply held after autonomous"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		if err := c.Stop(ctx, id, original); !errors.Is(err, control.ErrActionHeld) {
			t.Fatalf("named stop was not held: %v", err)
		}
	}
	var actions, notices int
	db.QueryRow(`SELECT count(*) FROM machine_held_actions WHERE thread_id=?`, id).Scan(&actions)
	db.QueryRow(`SELECT count(*) FROM machine_absence_notices WHERE thread_id=?`, id).Scan(&notices)
	if actions != 2 || notices != 1 {
		t.Fatalf("held action/absence dedup: %d %d", actions, notices)
	}
	c.Close()
	f.main.restart(t)
	// Only the fixture-owned worker terminal is used. The chat connection is cut.
	dirs, err := filepath.Glob(filepath.Join(f.root, "state/sessions/*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("worker host directory: %v %v", dirs, err)
	}
	a, err := host.Attach(host.SocketPath(dirs[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Input([]byte("tool wait 8s\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "autonomous worker terminal input", 10*time.Second, func() bool { return f.inputCount("tool wait 8s") == 1 })
	f.proxy.away.Store(false)
	c = f.client()
	f.main.posted(t, conv, "buffered before autonomous")
	f.main.posted(t, conv, "held after autonomous")
	f.turnCount(id, 3)
	var terminalStatus string
	if err := db.QueryRow(`SELECT x.status FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=? AND x.id!=? ORDER BY x.id LIMIT 1`, id, original).Scan(&terminalStatus); err != nil || terminalStatus != "completion" {
		t.Fatalf("autonomous turn was interrupted: %q %v", terminalStatus, err)
	}
	var state string
	db.QueryRow(`SELECT state FROM machine_held_actions WHERE thread_id=? AND kind='stop'`, id).Scan(&state)
	if state != "refused" {
		t.Fatalf("old stop retargeted after catch-up: %q", state)
	}
	if f.inputCount("reply held after autonomous") != 1 || f.main.count(conv, "held after autonomous") != 1 {
		t.Fatal("cut recovery repeated or interrupted another turn")
	}
	t.Log("ran: real SSH cut; durable input/named-stop dedup and one absence notice across main restart; own-terminal autonomous turn; catch-up refused old stop without interrupting new turn; held input once")
}

func TestFirstWorkerConfirmedBootLoss(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	id, conv := f.start(c, "before-controlled-boot", "tool wait 30s\nreply must not survive boot", "build")
	eventually(t, "active worker before controlled boot loss", 10*time.Second, func() bool { return len(f.main.events(t, "tool_started", conv)) == 1 })
	before := f.workspaceIdentities()
	f.proxy.cut()
	f.away(c, id)
	f.stopResident()
	killSessions(f.root) // Only these fixture-owned hosts; this is not an OS reboot.
	waitSessionsGone(f.root)
	f.boot.Add(1)
	f.startResident()
	eventually(t, "confirmed lost worker turn has one end", 20*time.Second, func() bool { return len(f.main.events(t, "turn_ended", conv)) == 1 })
	if f.main.count(conv, "must not survive boot") != 0 {
		t.Fatal("lost worker turn falsely completed")
	}
	db := f.main.db(t)
	var status, reason string
	if err := db.QueryRow(`SELECT x.status,x.reason FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=? ORDER BY x.id LIMIT 1`, id).Scan(&status, &reason); err != nil || status != "failure" || reason != "interrupted" {
		t.Fatalf("boot loss did not record interrupted recovery: %q %q %v", status, reason, err)
	}
	f.ready(c, id)
	if err := c.Send(context.Background(), id, "wake-after-controlled-boot", control.Input{Text: "reply same worker after boot"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	f.main.posted(t, conv, "same worker after boot")
	f.turnCount(id, 2)
	f.sameWorkspaces(before)
	if f.inputCount("reply same worker after boot") != 1 {
		t.Fatal("wake repeated input")
	}
	t.Log("ran: controlled boot-ID change plus termination of fixture-owned worker hosts, verified loss reconciled once, wake reused directory; no actual OS reboot/platform runtime claim")
}

func TestFirstWorkerHeldChatRechecksPersonAndScope(t *testing.T) {
	for _, mode := range []string{"person revoked", "scope disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newFirstWorker(t)
			c := f.client()
			id, conv := f.start(c, "authority-start", "reply before authorization change", "build")
			f.main.posted(t, conv, "before authorization change")
			f.turnCount(id, 1)
			f.proxy.cut()
			f.away(c, id)
			f.main.driver.Inject(conv, fakesurface.Person(e2eHook), "reply revoked chat must never execute")
			db := f.main.db(t)
			eventually(t, "non-owner chat action durably held", 10*time.Second, func() bool {
				var n int
				db.QueryRow(`SELECT count(*) FROM machine_held_actions WHERE thread_id=? AND state='pending'`, id).Scan(&n)
				return n == 1
			})
			c.Close()
			f.main.kill(t)
			path := filepath.Join(f.main.root, instance.ConfigFile)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			scope := cfg["scopes"].([]any)[0].(map[string]any)
			if mode == "person revoked" {
				scope["people"] = []string{}
			} else {
				scope["enabled"] = false
			}
			data, _ = json.Marshal(cfg)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			f.main.start(t)
			f.proxy.away.Store(false)
			eventually(t, "held chat visibly refused with current grants", 15*time.Second, func() bool {
				var state string
				db.QueryRow(`SELECT state FROM machine_held_actions WHERE thread_id=?`, id).Scan(&state)
				return state == "refused"
			})
			if f.inputCount("reply revoked chat must never execute") != 0 {
				t.Fatal("revoked chat input reached worker")
			}
			t.Log("ran: actual non-owner chat origin, held on SSH cut, grants changed across main restart, visible refusal and no worker write")
		})
	}
}

func TestFirstWorkerExpiredHeldAnswer(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "short-hook")
	script := "#!/bin/sh\nif [ \"$1\" = hook ]; then\n shift\n exec " + quoteSh(os.Args[0]) + " hook \"$@\" --timeout 6s\nfi\nexec " + quoteSh(os.Args[0]) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f := newFirstWorker(t, func(cfg *fake.Config) { cfg.Hataori = wrapper })
	c := f.client()
	id, conv := f.start(c, "expire-question", "ask original question", "build")
	f.main.postedWith(t, conv, "original question")
	detail, err := c.Thread(context.Background(), id, 0, 0)
	if err != nil || len(detail.Requests) != 1 {
		t.Fatalf("original request: %+v %v", detail.Requests, err)
	}
	f.proxy.cut()
	f.away(c, id)
	if err := c.Answer(context.Background(), detail.Requests[0].ID, control.Answer{Text: "late original answer"}); err != nil {
		t.Fatal(err)
	}
	db := f.main.db(t)
	eventually(t, "original answer held", 3*time.Second, func() bool {
		var n int
		db.QueryRow(`SELECT count(*) FROM machine_held_actions WHERE thread_id=? AND kind='answer' AND state='pending'`, id).Scan(&n)
		return n == 1
	})
	c.Close()
	f.main.restart(t)
	// Let the worker-local hook wall deadline pass while SSH remains cut.
	time.Sleep(6200 * time.Millisecond)
	f.proxy.away.Store(false)
	eventually(t, "expired answer refused after retained withdrawal", 15*time.Second, func() bool {
		var state string
		db.QueryRow(`SELECT state FROM machine_held_actions WHERE thread_id=? AND kind='answer'`, id).Scan(&state)
		return state == "refused"
	})
	var answers, withdrawals int
	db.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='answer'`).Scan(&answers)
	db.QueryRow(`SELECT count(*) FROM events WHERE kind='request_withdrawn' AND thread_id=?`, id).Scan(&withdrawals)
	if answers != 0 || withdrawals != 1 {
		t.Fatalf("expired original answer crossed worker boundary: %d %d", answers, withdrawals)
	}
	t.Log("ran: worker's actual fake-hook wall timeout across SSH cut/main restart; retained withdrawal precedes visible held-answer refusal; no answer operation sent")
}

func TestFirstWorkerRemovalPreservesSoleCopy(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	id, conv := f.start(c, "remove-worker", "reply before removal", "build")
	f.main.posted(t, conv, "before removal")
	f.turnCount(id, 1)
	if err := c.CloseThread(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	before := f.workspaceIdentities()
	if len(before) != 1 {
		t.Fatal("expected one worker workspace")
	}
	var path string
	for name := range before {
		path = filepath.Join(f.root, "workspaces", name, "sole-copy.txt")
	}
	if err := os.WriteFile(path, []byte("only worker copy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveThread(context.Background(), id); err == nil {
		t.Fatal("removal accepted untracked sole-copy file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "only worker copy\n" {
		t.Fatalf("worker sole copy lost: %q %v", data, err)
	}
	f.sameWorkspaces(before)
	t.Log("ran: owner removal through main/SSH/worker reran proof and preserved sole untracked bytes and directory identity")
}
