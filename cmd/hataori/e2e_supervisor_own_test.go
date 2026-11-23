package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

var updateOwn = flag.Bool("update-own", false, "write the golden file of a wake's launch in a conversation of its own")

// goldenOwn compares got with testdata/supervisor-own/name, or writes it
// with -update-own.
func goldenOwn(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "supervisor-own", name)
	if *updateOwn {
		os.MkdirAll(filepath.Dir(file), 0o755)
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("the wake's launch differs from %s:\n%s", file, got)
	}
}

// recordIn makes the fake program record each start and each input it
// takes in dir.
func recordIn(dir string) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		ads := cfg["adapters"].(map[string]any)
		c := ads[fake.Name].(fake.Config)
		c.Record = dir
		ads[fake.Name] = c
	}
}

// fakeStart is one start of the fake program, as it recorded it.
type fakeStart struct {
	Args   []string
	Inputs []string
	at     time.Time
}

// value returns the value of flag in the start's arguments, and whether
// it has the flag.
func (s fakeStart) value(flag string) (string, bool) {
	i := slices.Index(s.Args, flag)
	if i < 0 || i+1 >= len(s.Args) {
		return "", false
	}
	return s.Args[i+1], true
}

// starts reads the starts the fake recorded in dir, oldest first.
func starts(t *testing.T, dir string) []fakeStart {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "start-*.json"))
	var out []fakeStart
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s fakeStart
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(f)
		s.at = fi.ModTime()
		pid := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "start-"), ".json")
		if in, err := os.ReadFile(filepath.Join(dir, "inputs-"+pid+".jsonl")); err == nil {
			for _, l := range strings.Split(strings.TrimSpace(string(in)), "\n") {
				var x struct{ Text string }
				json.Unmarshal([]byte(l), &x)
				s.Inputs = append(s.Inputs, x.Text)
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// conversation returns the agent's id of the conversation of thread sv's
// newest session, and the one it set aside, if any.
func (wt *wakeTest) conversation(t *testing.T, sv control.ThreadID) (int64, string, sql.NullString, control.State) {
	t.Helper()
	var (
		sid     int64
		id      string
		aside   sql.NullString
		stateOf string
	)
	if err := wt.db.QueryRow(`SELECT id, agent_session_id, set_aside, state FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`,
		int64(sv)).Scan(&sid, &id, &aside, &stateOf); err != nil {
		t.Fatal(err)
	}
	return sid, id, aside, control.State(stateOf)
}

// asleepIn waits until thread sv is asleep, its conversation id back,
// with nothing set aside.
func (wt *wakeTest) asleepIn(t *testing.T, sv control.ThreadID, main string) {
	t.Helper()
	eventually(t, "the supervisor asleep in its own conversation", 15*time.Second, func() bool {
		_, id, aside, state := wt.conversation(t, sv)
		return state == control.Asleep && !aside.Valid && id == main
	})
}

// A digest wake starts the supervisor's agent with no --resume, in a new
// conversation given the wake's text alone, as the golden file fixes the
// launch; when its turn ends, the session is asleep with its conversation
// back, and a person's message resumes that one.
func TestSupervisorWakeInAConversationOfItsOwn(t *testing.T) {
	record := t.TempDir()
	wt := newWakeTest(t, recordIn(record))
	stop, owner, sv := wt.start(t)
	defer stop()
	_, main, _, _ := wt.conversation(t, sv.ID)
	if main == "" {
		t.Fatal("the supervisor's conversation has no id")
	}
	_, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
	wt.read(t, pos)
	wt.quiet(t)
	digest := wt.woken(t, owner, sv.ID, 1)[0]
	wt.asleepIn(t, sv.ID, main)

	var wake []fakeStart
	for _, s := range starts(t, record) {
		if slices.Contains(s.Inputs, digest) {
			wake = append(wake, s)
		}
	}
	if len(wake) != 1 {
		t.Fatalf("%d starts got the wake, want 1", len(wake))
	}
	w := wake[0]
	if _, ok := w.value("--resume"); ok {
		t.Fatalf("the wake's start resumes a conversation: %q", w.Args)
	}
	id, ok := w.value("--session")
	if !ok || id == main {
		t.Fatalf("the wake's start is not a new conversation (main %s): %q", main, w.Args)
	}
	if len(w.Inputs) != 1 || w.Inputs[0] != digest {
		t.Fatalf("the wake's conversation was given %q, want the wake's text alone", w.Inputs)
	}
	sid, _, _, _ := wt.conversation(t, sv.ID)
	instructions, err := os.ReadFile(filepath.Join(runner.StateDir(wt.r.root, sid), fake.InstructionsFile))
	if err != nil {
		t.Fatal(err)
	}
	conversations, _ := w.value("--conversations")
	args := strings.Join(w.Args, "\n")
	for _, r := range [][2]string{{wt.r.root, "<root>"}, {record, "<record>"}, {conversations, "<conversations>"}, {id, "<new conversation>"}} {
		args = strings.ReplaceAll(args, r[0], r[1])
	}
	launch := "# arguments\n" + args + "\n\n# instructions\n" + string(instructions) + "\n\n# input\n" + w.Inputs[0]
	goldenOwn(t, "wake.txt", strings.ReplaceAll(launch, wt.repo, "<repo>"))

	// A person's message resumes the conversation the wake set aside.
	if a := sendAndRead(t, owner, sv.ID, "p", "reply hello"); a != "hello" {
		t.Fatalf("the person's turn: %q", a)
	}
	all := starts(t, record)
	if r, _ := all[len(all)-1].value("--resume"); r != main {
		t.Fatalf("the person's message started %q, want --resume %s", all[len(all)-1].Args, main)
	}
}

// After two digest wakes, the person's message that resumes the
// supervisor's conversation begins with exactly two lines, one naming each
// wake; the next message, given to that conversation asleep again, begins
// with none.
func TestSupervisorPersonHearsOfTheWakesAnswered(t *testing.T) {
	record := t.TempDir()
	wt := newWakeTest(t, recordIn(record))
	stop, owner, sv := wt.start(t)
	defer stop()
	_, main, _, _ := wt.conversation(t, sv.ID)
	var workers []control.ThreadID
	for i, key := range []string{"a", "b"} {
		w, pos := wt.worker(t, owner, "c1", key, "reply "+key)
		workers = append(workers, w)
		wt.read(t, pos)
		wt.quiet(t)
		wt.woken(t, owner, sv.ID, i+1)
		wt.asleepIn(t, sv.ID, main)
	}
	resumed := func(key, text string) string {
		t.Helper()
		if a := sendAndRead(t, owner, sv.ID, key, text); a != strings.TrimPrefix(text, "reply ") {
			t.Fatalf("the person's turn %q: %q", text, a)
		}
		all := starts(t, record)
		last := all[len(all)-1]
		if r, _ := last.value("--resume"); r != main || len(last.Inputs) != 1 {
			t.Fatalf("the person's message %q started %q with %q, want --resume %s with one input", text, last.Args, last.Inputs, main)
		}
		return last.Inputs[0]
	}
	got := resumed("p1", "reply one")
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || lines[2] != "reply one" {
		t.Fatalf("the resumed turn's input; want two lines, then the person's message:\n%s", got)
	}
	for i, w := range workers {
		want := fmt.Sprintf(` on thread %d was answered in a conversation of its own; its answer began: "done".`, w)
		if !strings.HasPrefix(lines[i], "hataori, meanwhile: the wake of ") || !strings.HasSuffix(lines[i], want) {
			t.Fatalf("line %d does not name the wake of thread %d:\n%s", i+1, w, got)
		}
	}

	// Asleep again, its conversation's last turn is the person's: the next
	// message hears of no wake.
	if slept, err := wt.waker.Sleep(context.Background(), sv.ID); err != nil || !slept {
		t.Fatalf("putting the supervisor to sleep: %v, %v", slept, err)
	}
	wt.asleepIn(t, sv.ID, main)
	if got := resumed("p2", "reply two"); got != "reply two" {
		t.Fatalf("the next message's input; want it alone:\n%s", got)
	}
}

// A daemon stopped while a wake's conversation runs its turn, and started
// again, gives the wake once: the turn goes on in the agent that kept
// running, ends, and the session sleeps with its conversation back; no
// second conversation is started for it, and it is not lost.
func TestSupervisorWakeOnceAcrossARestartInItsConversation(t *testing.T) {
	record := t.TempDir()
	wt := newWakeTest(t, recordIn(record), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
	})
	stop, owner, sv := wt.start(t)
	_, main, _, _ := wt.conversation(t, sv.ID)
	_, pos := wt.worker(t, owner, "c1", "a", "reply tool hold 6s")
	wt.read(t, pos)
	wt.quiet(t)
	wakeTurn := func() (started, ended int) {
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'turn_started'
			AND substr(json_extract(data, '$.key'), 1, ?) = ?`, int64(sv.ID), len(supervisor.WakeKeyPrefix), supervisor.WakeKeyPrefix).Scan(&started)
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'turn_ended'
			AND json_extract(data, '$.turn') IN (SELECT json_extract(data, '$.turn') FROM events WHERE thread_id = ?
				AND kind = 'turn_started' AND substr(json_extract(data, '$.key'), 1, ?) = ?)`, int64(sv.ID), int64(sv.ID),
			len(supervisor.WakeKeyPrefix), supervisor.WakeKeyPrefix).Scan(&ended)
		return
	}
	eventually(t, "the wake's turn to run", 10*time.Second, func() bool {
		s, e := wakeTurn()
		return s == 1 && e == 0
	})
	stop()
	if s, e := wakeTurn(); s != 1 || e != 0 {
		t.Fatalf("the wake's turns when the daemon stopped: %d started, %d ended; want one running", s, e)
	}

	stop = runDaemon(t, wt.r.root)
	defer stop()
	if s, e := wakeTurn(); s != 1 || e != 0 {
		t.Fatalf("the wake's turns after the restart: %d started, %d ended; want one still running", s, e)
	}
	wt.asleepIn(t, sv.ID, main)
	if s, e := wakeTurn(); s != 1 || e != 1 {
		t.Fatalf("the wake's turns: %d started, %d ended; want one of each", s, e)
	}
	var outcome string
	wt.db.QueryRow(`SELECT json_extract(data, '$.outcome') FROM events WHERE thread_id = ? AND kind = 'turn_ended' ORDER BY pos DESC LIMIT 1`,
		int64(sv.ID)).Scan(&outcome)
	if outcome != "completion" {
		t.Fatalf("the wake's turn ended as %q, want completion", outcome)
	}
	wt.hour()
	time.Sleep(300 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 1 {
		t.Fatalf("%d wakes after the restart and an hour, want 1: %q", len(got), got)
	}
	var fresh int
	for _, s := range starts(t, record) {
		if _, ok := s.value("--resume"); !ok && slices.ContainsFunc(s.Inputs, func(in string) bool { return strings.HasPrefix(in, "hataori: ") }) {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d new conversations were given the wake, want 1", fresh)
	}
}

// A person's message in the supervisor's thread while a wake's turn runs is
// not given to that turn: it waits for its end, and is answered in the
// conversation the thread resumes then, after the line naming that wake.
func TestSupervisorPersonsMessageDuringAWake(t *testing.T) {
	record := t.TempDir()
	wt := newWakeTest(t, recordIn(record), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	_, main, _, _ := wt.conversation(t, sv.ID)
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	wt.r.posted(t, conv, "done") // the answer to its first message
	w, pos := wt.worker(t, owner, "c1", "a", "reply tool hold 3s\nreply reply woken")
	wt.read(t, pos)
	wt.quiet(t)
	eventually(t, "the wake's turn to run", 10*time.Second, func() bool { return wt.wakeRunning(t, sv.ID) })
	if err := owner.Send(context.Background(), sv.ID, "cli", control.Input{Text: "reply never"}, control.Deliver); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("a Send while the wake's turn runs: %v, want busy", err)
	}
	wt.r.driver.Inject(conv, fakesurface.Person(e2eOwner), "reply hello")
	if got := wt.r.posted(t, conv, "hello"); got.Message.Text != "hello" {
		t.Fatalf("the person's answer: %+v", got)
	}
	var fresh, resumed []fakeStart
	for _, s := range starts(t, record) {
		switch {
		case slices.ContainsFunc(s.Inputs, func(in string) bool { return strings.HasPrefix(in, "hataori: ") }):
			fresh = append(fresh, s)
		case slices.ContainsFunc(s.Inputs, func(in string) bool { return strings.HasSuffix(in, "reply hello") }):
			resumed = append(resumed, s)
		}
	}
	if len(fresh) != 1 || len(fresh[0].Inputs) != 1 {
		t.Fatalf("the wake's conversation was given %+v, want the wake alone", fresh)
	}
	if len(resumed) != 1 {
		t.Fatalf("%d starts were given the person's message, want 1", len(resumed))
	}
	if r, _ := resumed[0].value("--resume"); r != main {
		t.Fatalf("the person's message started %q, want --resume %s", resumed[0].Args, main)
	}
	want := regexp.MustCompile(fmt.Sprintf(`^hataori, meanwhile: the wake of .* on thread %d was answered in a conversation of its own; its answer began: "[^"]*"\.\nreply hello$`, w))
	if got := resumed[0].Inputs[0]; !want.MatchString(got) {
		t.Fatalf("the person's message reached the resumed conversation as:\n%s", got)
	}
}

// wakeRunning reports whether a wake's turn of sv runs.
func (wt *wakeTest) wakeRunning(t *testing.T, sv control.ThreadID) bool {
	t.Helper()
	var running int
	wt.db.QueryRow(`SELECT count(*) FROM events s WHERE s.thread_id = ? AND s.kind = 'turn_started'
		AND substr(json_extract(s.data, '$.key'), 1, ?) = ?
		AND NOT EXISTS (SELECT 1 FROM events e WHERE e.thread_id = s.thread_id AND e.kind = 'turn_ended'
			AND json_extract(e.data, '$.turn') = json_extract(s.data, '$.turn'))`,
		int64(sv), len(supervisor.WakeKeyPrefix), supervisor.WakeKeyPrefix).Scan(&running)
	return running == 1
}

// The usage an agent records after the wake's turn ended, as Claude Code
// does (#675), counts as that turn's and in the thread's tokens of the day,
// though the session sleeps when the turn ends.
func TestSupervisorWakeLateUsage(t *testing.T) {
	for _, then := range []string{"nothing", "a message at once"} {
		t.Run(then, func(t *testing.T) { wakeLateUsage(t, then) })
	}
}

func wakeLateUsage(t *testing.T, then string) {
	wt := newWakeTest(t, recordIn(t.TempDir()), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	_, main, _, _ := wt.conversation(t, sv.ID)
	today := func() (n int64) {
		wt.db.QueryRow(`SELECT tokens_today FROM threads WHERE id = ?`, int64(sv.ID)).Scan(&n)
		return
	}
	before := today()
	_, pos := wt.worker(t, owner, "c1", "a", "reply late-usage 100 5 0 0 0 0\nreply reply woken")
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1)
	if then == "a message at once" {
		// Before the usage is there (fake's lateUsage): the message starts
		// the session again while the wake's agent is still read.
		if err := owner.Send(context.Background(), sv.ID, "p", control.Input{Text: "reply hello"}, control.Refuse); err != nil {
			t.Fatal(err)
		}
	} else {
		wt.asleepIn(t, sv.ID, main)
	}
	eventually(t, "the wake's late usage to be counted", 10*time.Second, func() bool { return today() == before+105 })
	var turn, usageTurn int64
	wt.db.QueryRow(`SELECT json_extract(data, '$.turn') FROM events WHERE thread_id = ? AND kind = 'turn_started'
		AND substr(json_extract(data, '$.key'), 1, ?) = ?`, int64(sv.ID), len(supervisor.WakeKeyPrefix), supervisor.WakeKeyPrefix).Scan(&turn)
	wt.db.QueryRow(`SELECT json_extract(data, '$.turn') FROM events WHERE thread_id = ? AND kind = 'usage' ORDER BY pos DESC LIMIT 1`, int64(sv.ID)).Scan(&usageTurn)
	if turn == 0 || usageTurn != turn {
		t.Fatalf("the late usage counts toward turn %d, want the wake's %d", usageTurn, turn)
	}
}
