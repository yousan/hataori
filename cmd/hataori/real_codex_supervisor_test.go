package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// Runs only on explicit request, with disposable daemon/surface/workers and
// existing Codex CLI authentication. It never starts a terminal or another
// provider. Every supervisor launch is the actual adapter launch.
func TestRealCodexSupervisor(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CODEX_SUPERVISOR") != "1" {
		t.Skip("set HATAORI_REAL_CODEX_SUPERVISOR=1")
	}
	if runtime.GOOS != "linux" {
		t.Skip("verified Linux confinement only")
	}
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	repo := bareRepo(t)
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["agent"] = fake.Name
		cfg["idle"] = "8s"
		cfg["login_relay"] = false
		cfg["owner"] = "boss"
		cfg["people"] = []map[string]any{{"name": "boss", "ids": []string{remoteSurface + ":" + e2eOwner}}, {"name": "dispatch", "ids": []string{remoteSurface + ":" + e2eHook}}, {"name": "sv", "ids": []string{supervisor.ID("c1")}}}
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": remoteSurface, "enabled": true, "people": []string{"dispatch", "sv"}, "supervisor": map[string]any{"agent": codex.Name, "repos": []string{repo}, "model": "gpt-6-luna", "effort": "low", "quiet": "1s", "autonomy": "act", "turns_per_hour": 0, "sends_per_hour": 0, "tokens_per_day": 0, "instructions": "This is a disposable test. Use only the supplied hataori tools. Follow explicit test stage requests once each. Never use another provider or any native environment tool. For wake digests, inspect the named worker state and wait for the next authorized stage. Include scope-instructions-1088 in your initial readiness answer only."}}}
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")}}, codex.Name: map[string]any{"command": cli}}
	})
	p.deadline = time.Now().Add(15 * time.Minute)
	t.Cleanup(func() {
		if t.Failed() {
			p.logMu.Lock()
			t.Logf("disposable daemon: %s", p.log.String())
			p.logMu.Unlock()
		}
	})
	p.start(t)
	owner, err := wire.Dial(p.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { owner.Close() }()
	var sv control.ThreadInfo
	eventually(t, "real confined supervisor initial turn", 2*time.Minute, func() bool {
		ts, err := owner.Threads(context.Background(), control.Filter{Scope: "c1"})
		if err != nil {
			return false
		}
		for _, th := range ts {
			if th.Kind == control.KindSupervisor {
				sv = th
				return th.State == control.Idle && th.Turns > 0
			}
		}
		return false
	})
	var session int64
	t.Cleanup(func() {
		if session != 0 {
			exportRealCodexSupervisorCapture(t, p, session)
		}
	})
	if err := p.db(t).QueryRow(`SELECT id FROM sessions WHERE thread_id=? ORDER BY id DESC LIMIT 1`, int64(sv.ID)).Scan(&session); err != nil {
		t.Fatal(err)
	}
	initial := threadEvents(t, owner, sv.ID, "text")
	if len(initial) == 0 || !strings.Contains(initial[len(initial)-1]["text"].(string), "scope-instructions-1088") {
		t.Fatal("scope instructions did not reach the actual CLI", initial)
	}
	t.Log("actual confined launch, complete scope instructions and initial turn passed")
	send := func(key, text string) string {
		t.Helper()
		eventually(t, "ready to send "+key, 2*time.Minute, func() bool {
			th, e := owner.Thread(context.Background(), sv.ID, 0, 0)
			if e != nil || th.State != control.Idle && th.State != control.Asleep {
				return false
			}
			return owner.Send(context.Background(), sv.ID, key, control.Input{Text: text}, control.Refuse) == nil
		})
		eventually(t, "real supervisor turn "+key, 2*time.Minute, func() bool {
			var n int
			p.db(t).QueryRow(`SELECT count(*) FROM turns t JOIN sessions s ON s.id=t.session_id WHERE s.thread_id=? AND t.trigger=? AND t.ended_at IS NOT NULL`, int64(sv.ID), key).Scan(&n)
			return n == 1
		})
		texts := threadEvents(t, owner, sv.ID, "text")
		if len(texts) == 0 {
			t.Fatal("no real supervisor response")
		}
		return texts[len(texts)-1]["text"].(string)
	}
	answer := send("allowed", "Use hataori list_threads now. Then use hataori write_file to create notes/continuity.md with the exact text codex-continuity. Do no other writes. Reply ready after both calls succeed.")
	t.Logf("allowed management result: %s", answer)
	var dir string
	if err := p.db(t).QueryRow(`SELECT workspace FROM threads WHERE id=?`, int64(sv.ID)).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "notes/continuity.md"))
	if err != nil || string(content) != "codex-continuity" {
		t.Fatalf("scope note: %q, %v", content, err)
	}
	t.Log("scope-bound MCP management and file write passed")
	_ = send("escapes", "Use tool_search to discover every surfaced hataori tool. Also search the catalog for exec, wait, code mode, native patch/shell/files, browser/web/network fetch, subagents, background or wake tools. Never invoke a capability if one is present; report its presence as a failure. Use list_threads once through the supplied MCP. Do not write anything.")
	assertDirectSupervisorCapture(t, p.root, session)
	// The daemon is disposable. Session host and CLI remain alive during restart.
	p.stopDaemon()
	owner.Close()
	p.start(t)
	owner, err = wire.Dial(p.root)
	if err != nil {
		t.Fatal(err)
	}
	_ = send("reconnect", "Use hataori read_file to read notes/continuity.md, then hataori list_threads. Reply with the note's text. No writes.")
	var after int64
	p.db(t).QueryRow(`SELECT id FROM sessions WHERE thread_id=? ORDER BY id DESC LIMIT 1`, int64(sv.ID)).Scan(&after)
	if after != session {
		t.Fatalf("restart duplicated session: %d then %d", session, after)
	}
	t.Log("daemon restart preserved actual CLI management and session")
	if err := os.WriteFile(filepath.Join(dir, "USER.md"), []byte("Answer format: append the literal suffix refreshed-user-layer-1088 to your response. This is a public test label, not a secret."), 0600); err != nil {
		t.Fatal(err)
	}
	// A separate process resume uses the same isolated CLI state, never a
	// transcript of another provider. The configured idle timeout sleeps it.
	eventually(t, "confined session asleep", 30*time.Second, func() bool {
		th, e := owner.Thread(context.Background(), sv.ID, 0, 0)
		return e == nil && th.State == control.Asleep
	})
	resumed := send("resume", "Use hataori read_file for notes/continuity.md and list_threads. Reply with the note's text. Do not read USER.md. No native environment tools may be used.")
	generated, err := os.ReadFile(filepath.Join(runner.StateDir(p.root, session), "codex-confined/instructions.txt"))
	if err != nil || !strings.Contains(string(generated), "refreshed-user-layer-1088") {
		t.Fatal("launch did not compose current user-file instructions", err)
	}
	t.Log("generated resume instructions contain the refreshed user layer")
	if !strings.Contains(resumed, "refreshed-user-layer-1088") {
		t.Fatal("resumed launch did not receive current user-file instructions", resumed)
	}
	t.Log("actual process sleep/resume and continued MCP management passed")
	// Actual Codex manages exactly two disposable fake workers.
	_ = send("start-workers", fmt.Sprintf("Use hataori start_thread exactly twice in repo %q. First text: reply stage-one-done. Second text: ask fixture-question. Keep ordinary worker defaults (do not set model/agent). Do not send follow-up work or answer the question yet. Never start replacements or extra workers.", repo))
	var workers []control.ThreadInfo
	eventually(t, "two disposable workers, completed and blocked", 2*time.Minute, func() bool {
		ts, e := owner.Threads(context.Background(), control.Filter{Scope: "c1"})
		if e != nil {
			return false
		}
		workers = nil
		for _, th := range ts {
			if !th.Confined {
				workers = append(workers, th)
			}
		}
		if len(workers) != 2 {
			return false
		}
		return (workers[0].State == control.Waiting && workers[1].Turns == 1) || (workers[1].State == control.Waiting && workers[0].Turns == 1)
	})
	var done, question control.ThreadInfo
	for _, th := range workers {
		if th.Agent != fake.Name {
			t.Fatal("ordinary worker adapter changed")
		}
		if th.State == control.Waiting {
			question = th
		} else {
			done = th
		}
	}
	eventually(t, "completion/question wake delivered", 2*time.Minute, func() bool {
		var n int
		p.db(t).QueryRow(`SELECT count(*) FROM turns t JOIN sessions s ON s.id=t.session_id WHERE s.thread_id=? AND t.aside=1 AND t.ended_at IS NOT NULL`, int64(sv.ID)).Scan(&n)
		return n > 0
	})
	_ = send("answer-worker", fmt.Sprintf("Use hataori send_to_thread exactly once for worker %d with text fixture-answer. Do not start any other work.", question.ID))
	eventually(t, "fake worker question answered", time.Minute, func() bool {
		th, e := owner.Thread(context.Background(), question.ID, 1024, 0)
		return e == nil && strings.Contains(th.Recent, "answer: fixture-answer")
	})
	_ = send("next-stage", fmt.Sprintf("Authorize the next stage now: send exactly reply stage-two-done once to worker %d with hataori send_to_thread. Never start a new worker or repeat the previous stage.", done.ID))
	eventually(t, "authorized second stage completed", time.Minute, func() bool {
		th, e := owner.Thread(context.Background(), done.ID, 1024, 0)
		return e == nil && th.Turns == 2 && strings.Contains(th.Recent, "stage-two-done")
	})
	t.Log("two actual supervisor-started fake workers; completion wake, question answer and next stage passed")
	// A new human conversation uses the selected adapter and its own token.
	// Exercise the existing Converse entry point in a disposable daemon
	// in this process, as a chat adapter calls it; no new public API exists.
	p.stopDaemon()
	owner.Close()
	sessions := supervisors(t)
	stopConversationDaemon, _ := runDaemonLog(t, p.root)
	owner, err = wire.Dial(p.root)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := sessions.Load().Converse(context.Background(), supervisor.Call{Scope: "c1", Key: "real-human-conversation", From: remoteSurface + ":" + e2eOwner, Text: "Use hataori list_threads and read_file notes/continuity.md. Reply with its text. No writes or worker actions."})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "selected human conversation completed", 2*time.Minute, func() bool {
		th, e := owner.Thread(context.Background(), conversation, 0, 0)
		return e == nil && th.Kind == control.KindConversation && th.Agent == codex.Name && th.Turns > 0 && (th.State == control.Idle || th.State == control.Asleep)
	})
	stopConversationDaemon()
	owner.Close()
	p.start(t)
	owner, err = wire.Dial(p.root)
	if err != nil {
		t.Fatal(err)
	}
	_ = send("reminder", "Set exactly one reminder with hataori set_reminder, minutes 5, note fixture-reminder-once. Do not cancel it or set any other reminder. Continue to inspect wakes without dispatching any worker work.")
	var reminders int
	p.db(t).QueryRow(`SELECT count(*) FROM supervisor_reminders WHERE note='fixture-reminder-once' AND state='open'`).Scan(&reminders)
	if reminders != 1 {
		t.Fatalf("expected one outstanding reminder, got %d", reminders)
	}
	p.stopDaemon()
	owner.Close()
	p.start(t)
	owner, err = wire.Dial(p.root)
	if err != nil {
		t.Fatal(err)
	}
	_ = send("after-restart", "Read notes/continuity.md using hataori read_file and list the two existing workers. Do not start, resend, cancel or modify work/reminders.")
	t.Log("waiting for the actual five-minute reminder across daemon restart")
	eventually(t, "reminder delivered and wake completed", 7*time.Minute, func() bool {
		var n int
		p.db(t).QueryRow(`SELECT count(*) FROM supervisor_reminders WHERE note='fixture-reminder-once' AND state='given'`).Scan(&n)
		if n != 1 {
			return false
		}
		th, e := owner.Thread(context.Background(), sv.ID, 0, 0)
		return e == nil && (th.State == control.Idle || th.State == control.Asleep)
	})
	ts, err := owner.Threads(context.Background(), control.Filter{Scope: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, th := range ts {
		if !th.Confined {
			count++
			// Answering the fake question is a second worker turn too.
			want := 2
			if th.Turns != want {
				t.Fatalf("work duplicated across restart: worker %d has %d turns, want %d", th.ID, th.Turns, want)
			}
		}
	}
	if count != 2 {
		t.Fatalf("worker duplicated: %d", count)
	}
	assertDirectSupervisorCapture(t, p.root, session)
	t.Log("actual reminder delivered once; restart preserved notes/history and did not duplicate workers/stages")
}

// Inspect actual CLI response items, not a model-authored inventory. The
// direct catalog must never produce code-mode custom calls or foreign tools.
func assertDirectSupervisorCapture(t *testing.T, root string, session int64) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(runner.StateDir(root, session)), "*/codex-confined/sessions/*/*/*/*.jsonl"))
	searches, calls := 0, 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					Type      string `json:"type"`
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
					Tools     []struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(line), &record) != nil || record.Type != "response_item" {
				continue
			}
			switch record.Payload.Type {
			case "custom_tool_call":
				t.Fatalf("direct launch exposed custom/code-mode tool: %s", record.Payload.Name)
			case "function_call":
				calls++
				if record.Payload.Namespace != "mcp__hataori" {
					t.Fatalf("foreign direct tool call: %s", record.Payload.Name)
				}
			case "tool_search_output":
				searches++
				for _, tool := range record.Payload.Tools {
					if tool.Type != "namespace" || tool.Name != "mcp__hataori" {
						t.Fatalf("foreign surfaced tool: %s/%s", tool.Type, tool.Name)
					}
				}
			}
		}
	}
	if searches == 0 || calls == 0 {
		t.Fatal("missing real direct catalog search/allowed MCP evidence", searches, calls)
	}
}

func exportRealCodexSupervisorCapture(t *testing.T, p *procRoot, session int64) {
	// Only fixture outputs are exported; neither generated credential config,
	// auth or database are read/copied as evidence.
	if out := os.Getenv("HATAORI_CODEX_SUPERVISOR_CAPTURE"); out != "" {
		if err := os.Mkdir(out, 0700); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(runner.StateDir(p.root, session), host.EventsFile))
		if err != nil {
			t.Fatal(err)
		}
		// Export only response items from this disposable CLI's rollouts,
		// never auth/config. They contain actual code-mode calls and results.
		var records []byte
		files, _ := filepath.Glob(filepath.Join(filepath.Dir(runner.StateDir(p.root, session)), "*/codex-confined/sessions/*/*/*/*.jsonl"))
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(data), "\n") {
				var record struct {
					Type    string `json:"type"`
					Payload struct {
						Type string `json:"type"`
					} `json:"payload"`
				}
				if json.Unmarshal([]byte(line), &record) == nil && record.Type == "response_item" && record.Payload.Type != "reasoning" {
					records = append(records, []byte(line+"\n")...)
				}

			}
		}
		token, err := os.ReadFile(filepath.Join(runner.StateDir(p.root, session), supervisor.TokenFile))
		if err != nil {
			t.Fatal(err)
		}
		safeRecords := strings.ReplaceAll(string(records), strings.TrimSpace(string(token)), "FIXTURE_TOKEN_REDACTED")
		b = []byte(strings.ReplaceAll(string(b), strings.TrimSpace(string(token)), "FIXTURE_TOKEN_REDACTED"))
		if err := os.WriteFile(filepath.Join(out, "response-items.raw.jsonl"), []byte(safeRecords), 0600); err != nil {
			t.Fatal(err)
		}
		// Paths/identifiers are scrubbed separately before adding the capture.
		if err := os.WriteFile(filepath.Join(out, "adapter-events.raw.jsonl"), b, 0600); err != nil {
			t.Fatal(err)
		}
		summary, _ := json.MarshalIndent(map[string]any{"cli": "codex-cli 0.163.0", "model": "gpt-6-luna", "effort": "low", "test_passed": !t.Failed(), "fixture": "actual adapter; fake surface and two fake workers; direct Sessions.Converse"}, "", "  ")
		if err := os.WriteFile(filepath.Join(out, "outcome.json"), append(summary, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "fixture-root.txt"), []byte(p.root), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
