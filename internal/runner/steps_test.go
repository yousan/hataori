package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/store"
)

const captures = "../agent/claudecode/testdata/2.1.322"

// captured returns a captured hook payload, with fields replaced by set.
func captured(t *testing.T, file string, set map[string]string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(captures, file))
	if err != nil {
		t.Fatal(err)
	}
	if len(set) == 0 {
		return string(bytes.TrimSpace(b))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m[k] = v
	}
	out, _ := json.Marshal(m)
	return string(out)
}

func waitingLine(id, payload string) []byte {
	return []byte(fmt.Sprintf(`{"hook":%q,"wait":true,"at":0,"payload":%s}`, id, payload))
}

// transcriptUpTo returns the lines of a captured transcript up to and
// including the first that contains marker.
func transcriptUpTo(t *testing.T, file, marker string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(captures, file))
	if err != nil {
		t.Fatal(err)
	}
	ls := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i, l := range ls {
		if bytes.Contains(l, []byte(marker)) {
			return ls[:i+1]
		}
	}
	t.Fatalf("no %q in %s", marker, file)
	return nil
}

const interruptMark = "[Request interrupted by user"

// The interrupt capture's prompt, which the question capture's
// AskUserQuestion call is moved into: the call of an interrupted turn.
func askInInterruptedTurn(t *testing.T) string {
	var p struct {
		PromptID  string `json:"prompt_id"`
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(captured(t, "interrupt/hooks/02-UserPromptSubmit.json", nil)), &p)
	return captured(t, "question/hooks/03-PreToolUse.json", map[string]string{"prompt_id": p.PromptID, "session_id": p.SessionID})
}

// A PreToolUse for AskUserQuestion, a tool's PreToolUse and a
// PermissionRequest, then the transcript's interrupt line, all new in one
// step: the interrupt is read first, and the calls of the interrupted turn
// open nothing.
func TestCallsOfAnInterruptedTurnOpenNothing(t *testing.T) {
	x := newReader(t, claudeCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.lv.files[transcript] = 0
	x.hooks(hookLine("c1", captured(t, "interrupt/hooks/01-SessionStart.json", nil)),
		hookLine("c2", captured(t, "interrupt/hooks/02-UserPromptSubmit.json", nil)))
	evs := x.step()

	x.appendTo(transcript, transcriptUpTo(t, "interrupt/transcript.jsonl", interruptMark)...)
	x.hooks(hookLine("c3", captured(t, "interrupt/hooks/03-PreToolUse.json", nil)),
		waitingLine("c4", captured(t, "interrupt/hooks/04-PermissionRequest.json", nil)),
		waitingLine("c5", askInInterruptedTurn(t)))
	b := x.r.read(x.lv)
	evs = append(evs, b.events...)

	open := map[string]bool{}
	ended := false
	for _, e := range evs {
		switch e.Kind {
		case agent.TurnEnded:
			ended = true
		case agent.Request:
			open[e.Request.ID] = true
		case agent.RequestWithdrawn:
			delete(open, e.Request.ID)
		case agent.ToolStarted:
			if ended {
				t.Errorf("a tool started after the turn's end: %+v", e)
			}
		}
	}
	if !ended {
		t.Fatalf("the turn did not end: %v", kinds(evs))
	}
	if len(open) != 0 {
		t.Errorf("open requests %v, want none", open)
	}
	for _, a := range b.actions {
		if len(a.Reply) > 0 {
			t.Errorf("an abandoned call was answered: %+v", a.Reply)
		}
	}
}

func claudeCodec(t *testing.T) agent.Codec {
	t.Helper()
	ad, err := claudecode.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return ad.NewCodec(agent.Spec{})
}

// storedReader is a reader whose steps are stored as the runner's commit
// stores them, in a real store.
func storedReader(t *testing.T, codec agent.Codec) *reader {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'owner', 'running', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'test', 'running', 0, 0)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	x := newReader(t, codec)
	x.r.Store, x.lv.id = st, 1
	return x
}

// commitStep reads what is new and stores it as a commit does.
func (x *reader) commitStep() batch {
	x.t.Helper()
	b := x.r.read(x.lv)
	ctx := context.Background()
	if err := x.r.Store.Update(ctx, func(tx *store.Tx) error { return saveRead(ctx, tx, x.lv.id, b) }); err != nil {
		x.t.Fatal(err)
	}
	x.lv.offset = b.offset
	for p, o := range b.files {
		x.lv.files[p] = o
	}
	return b
}

// rebuild makes the session's live state again from the store, as follow
// does after a restart, with codec, and warms it.
func (x *reader) rebuild(codec agent.Codec) *live {
	x.t.Helper()
	lv := x.load(codec)
	x.r.warm(lv)
	return lv
}

// load makes the session's live state from the store, with codec.
func (x *reader) load(codec agent.Codec) *live {
	x.t.Helper()
	lv := &live{id: x.lv.id, dir: x.lv.dir, codec: codec, files: map[string]int64{}}
	db := x.r.Store.DB
	if err := db.QueryRow(`SELECT log_offset, log_start FROM sessions WHERE id = ?`, lv.id).Scan(&lv.offset, &lv.start); err != nil {
		x.t.Fatal(err)
	}
	rows, err := db.Query(`SELECT path, offset FROM session_files WHERE session_id = ?`, lv.id)
	if err != nil {
		x.t.Fatal(err)
	}
	for rows.Next() {
		var p string
		var o int64
		rows.Scan(&p, &o)
		lv.files[p] = o
	}
	rows.Close()
	return lv
}

// An interrupted turn, then the next prompt's question, each read in its
// own step. Replaying every hook call before every transcript line would
// give the interrupt after the question and withdraw it; the rebuilt codec
// is given the steps in their order and holds the question open, as
// committed.
func TestRebuiltCodecHoldsTheCommittedRequests(t *testing.T) {
	x := storedReader(t, claudeCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.lv.files[transcript] = 0
	x.hooks(hookLine("c1", captured(t, "interrupt/hooks/01-SessionStart.json", nil)),
		hookLine("c2", captured(t, "interrupt/hooks/02-UserPromptSubmit.json", nil)))
	evs := x.commitStep().events
	x.appendTo(transcript, transcriptUpTo(t, "interrupt/transcript.jsonl", interruptMark)...)
	evs = append(evs, x.commitStep().events...)
	x.hooks(hookLine("c3", captured(t, "question/hooks/02-UserPromptSubmit.json", nil)),
		waitingLine("c4", captured(t, "question/hooks/03-PreToolUse.json", nil)))
	evs = append(evs, x.commitStep().events...)

	open := map[string]bool{}
	for _, e := range evs {
		switch e.Kind {
		case agent.Request:
			open[e.Request.ID] = true
		case agent.RequestWithdrawn:
			delete(open, e.Request.ID)
		}
	}
	if len(open) != 1 || !open["c4"] {
		t.Fatalf("committed open requests %v, want c4", open)
	}

	lv := x.rebuild(claudeCodec(t))
	if _, err := lv.codec.Answer(agent.Answer{Request: "c4", Choices: []string{"red"}}); err != nil {
		t.Fatalf("the rebuilt codec does not hold the open question: %v", err)
	}
}

// After a sleep the woken agent's codec reads only what came after the
// skip; a codec rebuilt later is given the same, not the lines of the agent
// put to sleep nor those written while it was stopped.
func TestWarmAfterASleepGivesOnlyTheWokenAgentsLines(t *testing.T) {
	x := storedReader(t, fakeTerminalCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
		hookLine("h2", `{"t":"turn","by":"caller"}`))
	x.appendTo(transcript, []byte(`{"t":"text","text":"one"}`))
	x.commitStep()
	// Written while the session was put to sleep, then skipped.
	x.appendTo(transcript, []byte(`{"t":"text","text":"skipped"}`))
	x.hooks(hookLine("h3", `{"t":"end","status":"completed"}`))
	if err := x.r.skipRest(context.Background(), x.lv.id, x.lv.dir); err != nil {
		t.Fatal(err)
	}
	// The woken agent, with a new codec.
	x.lv = x.load(fakeTerminalCodec(t))
	x.hooks(hookLine("h4", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
		hookLine("h5", `{"t":"turn","by":"caller"}`))
	x.appendTo(transcript, []byte(`{"t":"text","text":"two"}`))
	x.commitStep()

	rec := &lineRecorder{Codec: fakeTerminalCodec(t)}
	x.rebuild(rec)
	var got []string
	for _, l := range rec.got {
		for _, w := range []string{"h1", "h2", "h3", "h4", "h5", "one", "skipped", "two"} {
			if strings.Contains(l, `"`+w+`"`) {
				got = append(got, w)
			}
		}
	}
	if want := "two h4 h5"; strings.Join(got, " ") != want {
		t.Fatalf("replayed %q, want %q", strings.Join(got, " "), want)
	}
}

// lineRecorder is a codec that records what it is given, in order.
type lineRecorder struct {
	agent.Codec
	mu  sync.Mutex
	got []string
}

func (c *lineRecorder) Decode(src agent.Source, line []byte) agent.Actions {
	c.mu.Lock()
	c.got = append(c.got, string(line))
	c.mu.Unlock()
	return c.Codec.Decode(src, line)
}

// A session stored before ranges were recorded: the lines up to its
// offsets come first, in the order used then (the log, then each file),
// then the ranges recorded since, in their order.
func TestWarmWithLinesReadBeforeRangesWereStored(t *testing.T) {
	x := storedReader(t, fakeTerminalCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
		hookLine("h2", `{"t":"turn","by":"caller"}`))
	x.appendTo(transcript, []byte(`{"t":"text","text":"one"}`))
	x.commitStep()
	// As a database from before: the first step's ranges were not stored.
	if _, err := x.r.Store.DB.Exec(`DELETE FROM session_reads`); err != nil {
		t.Fatal(err)
	}
	x.appendTo(transcript, []byte(`{"t":"text","text":"two"}`))
	x.hooks(hookLine("h3", `{"t":"end","status":"completed"}`))
	x.commitStep()

	rec := &lineRecorder{Codec: fakeTerminalCodec(t)}
	x.rebuild(rec)
	var got []string
	for _, l := range rec.got {
		switch {
		case strings.Contains(l, `"h1"`):
			got = append(got, "h1")
		case strings.Contains(l, `"h2"`):
			got = append(got, "h2")
		case strings.Contains(l, `"h3"`):
			got = append(got, "h3")
		case strings.Contains(l, `"one"`):
			got = append(got, "one")
		case strings.Contains(l, `"two"`):
			got = append(got, "two")
		}
	}
	// Live, the second step read "two" (the file) before h3 (the log).
	if want := "h1 h2 one two h3"; strings.Join(got, " ") != want {
		t.Fatalf("replayed %q, want %q", strings.Join(got, " "), want)
	}
}

// What each step read is stored in the step's own commit: a commit that
// fails stores no range, and the stored ranges of each file always run
// without a gap from where they begin to the stored offset.
func TestReadRangesAreStoredWithTheirCommit(t *testing.T) {
	f := newFixture(t, options{terminal: true, poll: 100 * time.Millisecond})
	if _, err := f.st.DB.Exec(`CREATE TRIGGER fail_tool BEFORE INSERT ON events WHEN NEW.kind = 'tool_started' BEGIN SELECT RAISE(ABORT, 'test: no tool'); END`); err != nil {
		t.Fatal(err)
	}
	f.start("tool Bash 1s\nreply hello")
	waitFor(t, "a failed commit", 15*time.Second, func() bool { return strings.Contains(f.log.String(), "test: no tool") })
	checkRanges(t, f.st)
	if _, err := f.st.DB.Exec(`DROP TRIGGER fail_tool`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the end of the turn", 15*time.Second, f.has("turn_ended", 1))
	checkRanges(t, f.st)
	var n int
	f.st.DB.QueryRow(`SELECT count(*) FROM session_reads WHERE path = ''`).Scan(&n)
	if n == 0 {
		t.Fatal("no range of the log was stored")
	}
}

// checkRanges checks, for every session, that the stored ranges of the
// log and of each followed file run without a gap and end at the stored
// offset.
func checkRanges(t *testing.T, st *store.Store) {
	t.Helper()
	offsets := map[string]int64{}
	rows, err := st.DB.Query(`SELECT '', log_offset FROM sessions UNION ALL SELECT path, offset FROM session_files`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		var o int64
		rows.Scan(&p, &o)
		offsets[p] = o
	}
	rows.Close()
	ends := map[string]int64{}
	rows, err = st.DB.Query(`SELECT path, start, end FROM session_reads ORDER BY session_id, n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var s, e int64
		rows.Scan(&p, &s, &e)
		if prev, ok := ends[p]; ok && s != prev {
			t.Errorf("%q: a range starts at %d after one that ended at %d", p, s, prev)
		}
		ends[p] = e
	}
	for p, o := range offsets {
		if o > 0 && ends[p] != o {
			t.Errorf("%q: stored offset %d, stored ranges end at %d", p, o, ends[p])
		}
	}
}
