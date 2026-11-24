package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// What the log line of a session launched with another config says
// (runner, compare).
const launchedOtherwise = "launched with another config"

// svConfig changes keys of c1's supervisor section in the config file.
func svConfig(t *testing.T, root string, keys map[string]any) {
	t.Helper()
	setConfig(t, root, func(cfg map[string]any) {
		sv := cfg["scopes"].([]any)[0].(map[string]any)["supervisor"].(map[string]any)
		for k, v := range keys {
			sv[k] = v
		}
	})
}

// changedLines are the log's lines about a session launched with another
// config.
func changedLines(log string) []string {
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, launchedOtherwise) {
			lines = append(lines, l)
		}
	}
	return lines
}

// After a change of what a launch of the supervisor is given and a daemon
// restart, the idle session sleeps, with one log line that names what
// changed, and a person's next message resumes the same conversation, in
// the same chat thread, with the new model, effort and instructions. A
// restart without such a change keeps the agent.
func TestSupervisorConfigChangeReachesAPersonsMessage(t *testing.T) {
	record := t.TempDir()
	r, _ := svRoot(t, true, recordIn(record))
	layer := supervisor.Layer
	t.Cleanup(func() { supervisor.Layer = layer })
	stop, _ := runDaemonLog(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	sv := supervisorThread(t, owner, "c1", control.Idle)
	conversation, _ := starts(t, record)[0].value("--session")
	if conversation == "" {
		t.Fatalf("the supervisor's first start %v names no conversation", starts(t, record)[0].Args)
	}
	model, effort := "", supervisor.DefaultEffort
	for i, tc := range []struct {
		name    string
		change  func()
		changed string // what the log line names; empty for no relaunch
		says    string // in the instructions
	}{
		{"nothing", func() {}, "", "Route the channel's work."},
		{"the model", func() { model = "m-2"; svConfig(t, r.root, map[string]any{"model": model}) }, "changed=model", ""},
		{"nothing more", func() {}, "", ""},
		{"the effort", func() { effort = "max"; svConfig(t, r.root, map[string]any{"effort": effort}) }, "changed=effort", ""},
		{"the instructions", func() { svConfig(t, r.root, map[string]any{"instructions": "Watch the second channel."}) },
			`changed="the config's instructions"`, "Watch the second channel."},
		{"a key the config's layer states", func() { svConfig(t, r.root, map[string]any{"turns_per_hour": 7}) },
			`changed="the config's instructions"`, "turns_per_hour 7,"},
		{"a key it does not state", func() { svConfig(t, r.root, map[string]any{"quiet": "40s"}) }, "", ""},
		{"a user's file", func() {
			dirs, _ := filepath.Glob(filepath.Join(r.root, "confined", "*"))
			if len(dirs) != 1 {
				t.Fatalf("directories %v; want c1's alone", dirs)
			}
			if err := os.WriteFile(filepath.Join(dirs[0], "AGENTS.md"), []byte("Short answers."), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "", ""},
		{"hataori's layer", func() { supervisor.Layer = layer + "\nOne more rule of hataori." }, `changed="hataori's instructions"`, "One more rule of hataori."},
		{"two of them", func() {
			model, effort = "m-3", "low"
			svConfig(t, r.root, map[string]any{"model": model, "effort": effort})
		}, `changed="model, effort"`, ""},
		{"no model any more", func() { model = ""; svConfig(t, r.root, map[string]any{"model": ""}) }, "changed=model", ""},
		{"a session of a hataori that recorded nothing", func() {
			db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE sessions SET spec = json_remove(spec, '$.config') WHERE thread_id = ?`, int64(sv.ID)); err != nil {
				t.Fatal(err)
			}
		}, `changed="not recorded"`, ""},
	} {
		before := len(starts(t, record))
		owner.Close()
		stop()
		tc.change()
		var logged func() string
		stop, logged = runDaemonLog(t, r.root)
		if owner, err = wire.Dial(r.root); err != nil {
			t.Fatal(err)
		}
		text := "reply turn " + string(rune('a'+i))
		if a := sendAndRead(t, owner, sv.ID, "k"+string(rune('a'+i)), text); !strings.Contains(a, "turn "+string(rune('a'+i))) {
			t.Fatalf("%s: the answer is %q", tc.name, a)
		}
		all := starts(t, record)
		lines := changedLines(logged())
		if tc.changed == "" {
			if len(all) != before || len(lines) != 0 {
				t.Fatalf("%s: %d starts before the restart, %d after, with the lines %q; want the agent kept", tc.name, before, len(all), lines)
			}
		} else {
			if len(all) != before+1 {
				t.Fatalf("%s: %d starts before the restart, %d after the next message; want one more", tc.name, before, len(all))
			}
			if len(lines) != 1 || !strings.HasSuffix(lines[0], tc.changed) {
				t.Fatalf("%s: the log says %q; want one line ending in %s", tc.name, lines, tc.changed)
			}
			last := all[len(all)-1]
			m, _ := last.value("--model")
			e, _ := last.value("--effort")
			c, _ := last.value("--resume")
			if m != model || e != effort || c != conversation {
				t.Fatalf("%s: resumed with model %q, effort %q, conversation %q; want %q, %q, %q", tc.name, m, e, c, model, effort, conversation)
			}
		}
		if in := instructionsOf(t, r.root, sv.ID); !strings.Contains(in, tc.says) {
			t.Fatalf("%s: the instructions do not say %q:\n%s", tc.name, tc.says, in)
		}
		if n := supervisorThreads(t, owner, "c1"); n != 1 || r.openedIn("c1") != 1 {
			t.Fatalf("%s: %d supervisor threads, %d chat threads opened; want one of each", tc.name, n, r.openedIn("c1"))
		}
		if d, err := owner.Thread(context.Background(), sv.ID, 0, 1); err != nil || d.Model != model || d.Effort != effort {
			t.Fatalf("%s: the thread shows model %q and effort %q, %v; want %q and %q", tc.name, d.Model, d.Effort, err, model, effort)
		}
	}
	owner.Close()
	stop()
}

// After a change of the model, the effort and the instructions and a daemon
// restart, the next wake's conversation is launched with the new ones, in
// the same chat thread, also when the session was asleep at the restart;
// the thread's own conversation, which a message resumes, is the one it was.
func TestSupervisorConfigChangeReachesAWake(t *testing.T) {
	record := t.TempDir()
	wt := newWakeTest(t, recordIn(record))
	stop, owner, sv := wt.start(t)
	_, own, _, _ := wt.conversation(t, sv.ID)
	for i, model := range []string{"m-2", "m-3"} { // idle at the restart, then asleep after a wake
		n := string(rune('1' + i))
		owner.Close()
		stop()
		svConfig(t, wt.r.root, map[string]any{"model": model, "effort": "low", "instructions": "Rule " + n + "."})
		before := len(starts(t, record))
		stop, owner, _ = wt.start(t)
		_, pos := wt.worker(t, owner, "c1", "w"+n, "reply done "+n)
		wt.read(t, pos)
		wt.quiet(t)
		wt.woken(t, owner, sv.ID, i+1)
		wt.asleepIn(t, sv.ID, own)
		var wakes []fakeStart
		for _, s := range starts(t, record)[before:] {
			if len(s.Inputs) == 1 && s.Inputs[0] != "reply done "+n { // not the worker's
				wakes = append(wakes, s)
			}
		}
		if len(wakes) != 1 {
			t.Fatalf("starts after the restart %+v; want the worker's and the wake's", starts(t, record)[before:])
		}
		wake := wakes[0]
		m, _ := wake.value("--model")
		e, _ := wake.value("--effort")
		_, resumed := wake.value("--resume")
		if m != model || e != "low" || resumed {
			t.Fatalf("the wake was launched with %v; want model %s, effort low, a conversation of its own", wake.Args, model)
		}
		if in := instructionsOf(t, wt.r.root, sv.ID); !strings.Contains(in, "Rule "+n+".") {
			t.Fatalf("the wake's instructions do not say Rule %s:\n%s", n, in)
		}
	}
	sendAndRead(t, owner, sv.ID, "after", "reply after")
	all := starts(t, record)
	last := all[len(all)-1]
	m, _ := last.value("--model")
	if c, _ := last.value("--resume"); c != own || m != "m-3" {
		t.Fatalf("the message resumed %v; want the conversation %s with m-3", last.Args, own)
	}
	if n := supervisorThreads(t, owner, "c1"); n != 1 || wt.r.openedIn("c1") != 1 {
		t.Fatalf("%d supervisor threads, %d chat threads opened; want one of each", n, wt.r.openedIn("c1"))
	}
	owner.Close()
	stop()
}

// A supervisor's turn that runs when the daemon comes back with another
// config, one that waits for an answer, and a background task of its last
// turn go on with the agent they run in; the session sleeps once they have
// ended, without another message.
func TestSupervisorConfigChangeInterruptsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		busy   control.State // before the stop
		ended  int           // turn ends at busy, the first turn's included
		then   func(t *testing.T, owner *wire.Client, sv control.ThreadID)
		text   string // the last text
	}{
		"a running turn": {"tool hold 8s\nreply the old one", control.Running, 1, nil, "the old one"},
		"a question": {"ask which colour? -- red | blue", control.Waiting, 2, func(t *testing.T, owner *wire.Client, sv control.ThreadID) {
			d, err := owner.Thread(context.Background(), sv, 0, 1)
			if err != nil || len(d.Requests) != 1 {
				t.Fatalf("open requests %+v, %v; want one", d.Requests, err)
			}
			if err := owner.Answer(context.Background(), d.Requests[0].ID, control.Answer{Choices: []string{d.Requests[0].Choices[0].Token}}); err != nil {
				t.Fatal(err)
			}
		}, "answer: red"},
		"a background task": {"background 8s\nreply started", control.Idle, 2, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			record := t.TempDir()
			r, _ := svRoot(t, true, recordIn(record))
			stop, _ := runDaemonLog(t, r.root)
			owner, err := wire.Dial(r.root)
			if err != nil {
				t.Fatal(err)
			}
			sv := supervisorThread(t, owner, "c1", control.Idle)
			if err := owner.Send(context.Background(), sv.ID, "k", control.Input{Text: tc.script}, control.Refuse); err != nil {
				t.Fatal(err)
			}
			state := func() control.State {
				d, err := owner.Thread(context.Background(), sv.ID, 0, 1)
				if err != nil {
					t.Fatal(err)
				}
				return d.State
			}
			eventually(t, "the supervisor "+string(tc.busy), 15*time.Second, func() bool {
				return state() == tc.busy && len(threadEvents(t, owner, sv.ID, "turn_ended")) == tc.ended
			})
			owner.Close()
			stop()
			svConfig(t, r.root, map[string]any{"model": "m-2"})
			stop, logged := runDaemonLog(t, r.root)
			defer stop()
			if owner, err = wire.Dial(r.root); err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			time.Sleep(1500 * time.Millisecond) // longer than it is tried again
			if s, n := state(), len(threadEvents(t, owner, sv.ID, "turn_ended")); s != tc.busy || n != tc.ended {
				t.Fatalf("after the restart the supervisor is %s with %d turn ends; want it %s still, with %d", s, n, tc.busy, tc.ended)
			}
			if len(changedLines(logged())) != 1 {
				t.Fatalf("the log's lines of the change: %q; want one", changedLines(logged()))
			}
			if tc.then != nil {
				tc.then(t, owner, sv.ID)
			}
			supervisorThread(t, owner, "c1", control.Asleep)
			ends := threadEvents(t, owner, sv.ID, "turn_ended")
			if len(ends) != tc.ended+1 {
				t.Fatalf("%d turn ends, want %d", len(ends), tc.ended+1)
			}
			for _, e := range ends {
				if e["outcome"] != "completion" && e["outcome"] != "blocked" {
					t.Fatalf("a turn ended %v; want it completed, or blocked by the question", e)
				}
			}
			texts := threadEvents(t, owner, sv.ID, "text")
			if last := texts[len(texts)-1]["text"].(string); !strings.Contains(last, tc.text) {
				t.Fatalf("the last text is %q; want %q", last, tc.text)
			}
			if n := len(starts(t, record)); n != 1 {
				t.Fatalf("%d starts before the next message, want the first alone", n)
			}
			sendAndRead(t, owner, sv.ID, "next", "reply next")
			all := starts(t, record)
			m, _ := all[len(all)-1].value("--model")
			if len(all) != 2 || m != "m-2" {
				t.Fatalf("%d starts, the last %v; want a second one with m-2", len(all), all[len(all)-1].Args)
			}
		})
	}
}
