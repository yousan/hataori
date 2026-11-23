package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/workspace"
)

// reopenRoot is a chat root whose fake agent keeps its conversations, so
// that a resumed one remembers its turns (the script command recall) and
// one whose file is gone cannot be resumed, and records each start, so
// that a test sees the directory it started in and whether it resumed.
type reopenRoot struct {
	*chatRoot
	conversations, record string
}

func newReopenRoot(t *testing.T, change func(cfg map[string]any)) *reopenRoot {
	t.Helper()
	conv := filepath.Join(t.TempDir(), "conversations")
	record := filepath.Join(t.TempDir(), "record")
	os.MkdirAll(conv, 0o700)
	os.MkdirAll(record, 0o700)
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv, Record: record}}
		if change != nil {
			change(cfg)
		}
	})
	r.deadline = time.Now().Add(150 * time.Second)
	startDaemon(t, r.root)
	r.connected(t)
	return &reopenRoot{chatRoot: r, conversations: conv, record: record}
}

// ownerPerson is the person the old form's owner becomes.
const ownerPerson = "owner"

// start is one start of the fake agent, as it recorded it.
type start struct {
	Args []string
	Dir  string
}

// starts are the agent's starts so far, oldest first.
func (r *reopenRoot) starts(t *testing.T) []start {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(r.record, "start-*.json"))
	type file struct {
		at time.Time
		s  start
	}
	var fs []file
	for _, f := range files {
		b, err := os.ReadFile(f)
		st, serr := os.Stat(f)
		if err != nil || serr != nil {
			t.Fatal(err, serr)
		}
		var s start
		json.Unmarshal(b, &s)
		fs = append(fs, file{st.ModTime(), s})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].at.Before(fs[j].at) })
	var out []start
	for _, f := range fs {
		out = append(out, f.s)
	}
	return out
}

// lastStart is the one start that came after the first n, once it is
// there.
func (r *reopenRoot) lastStart(t *testing.T, n int) start {
	t.Helper()
	eventually(t, "a new start of the agent", r.within(15*time.Second), func() bool { return len(r.starts(t)) > n })
	ss := r.starts(t)
	if len(ss) != n+1 {
		t.Fatalf("%d starts, want %d", len(ss), n+1)
	}
	return ss[n]
}

// resumes reports whether s resumed a conversation.
func (s start) resumes() bool { return slicesIndex(s.Args, "--resume") >= 0 }

func slicesIndex(xs []string, x string) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

// closed starts a thread with one turn, closes it with hataori close and
// returns its chat thread, its number and its workspace.
func (r *reopenRoot) closed(t *testing.T, n int, word string) (surface.Conversation, string, string) {
	t.Helper()
	th, _, ws := r.idleThread(t, n, word)
	num := numberOf(t, r.root, th)
	if code, _, errOut := cli(t, "close", "--root", r.root, num); code != 0 {
		t.Fatalf("hataori close: %d %q", code, errOut)
	}
	return th, num, ws
}

// reopen runs hataori reopen on num and returns what it printed.
func (r *reopenRoot) reopen(t *testing.T, num string) map[string]any {
	t.Helper()
	code, out, errOut := cli(t, "reopen", "--root", r.root, num)
	if code != 0 {
		t.Fatalf("hataori reopen: %d %q %q", code, out, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("hataori reopen printed %q", out)
	}
	return got
}

// before reports whether the post with text a came before the one with b
// in th.
func (r *reopenRoot) before(t *testing.T, th surface.Conversation, a, b string) bool {
	t.Helper()
	ia, ib := -1, -1
	for i, c := range r.driver.Calls(th) {
		if c.Op != fakesurface.OpPost {
			continue
		}
		if c.Message.Text == a && ia < 0 {
			ia = i
		}
		if c.Message.Text == b && ib < 0 {
			ib = i
		}
	}
	return ia >= 0 && ib >= 0 && ia < ib
}

// hataori reopen on a closed thread takes it back to asleep, and the next
// message in its chat thread resumes the same conversation, in the same
// directory: the agent remembers the earlier turn. A thread that is not
// closed is refused.
func TestHostReopenResumesTheConversation(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, num, ws := r.closed(t, 0, "first")
	other, _, _ := r.idleThread(t, 1, "open")
	if code, _, errOut := cli(t, "reopen", "--root", r.root, numberOf(t, r.root, other)); code != exitError ||
		!strings.Contains(errOut, "not reopened: the thread is idle, not closed") {
		t.Fatalf("reopen of an idle thread: %d %q", code, errOut)
	}
	before := snapshot(t, ws)
	n := len(r.starts(t))

	got := r.reopen(t, num)
	if got["state"] != "asleep" || got["workspace"] != "kept" || got["conversation"] != "resume" {
		t.Fatalf("hataori reopen printed %v", got)
	}
	if r.state(t, th) != "asleep" {
		t.Fatalf("state %s, want asleep", r.state(t, th))
	}
	var reopened []map[string]any
	for _, e := range r.events(t, "state", th) {
		if e["from"] == "closed" && e["to"] == "asleep" {
			reopened = append(reopened, e)
		}
	}
	if len(reopened) != 1 || reopened[0]["reason"] != "reopen" {
		t.Fatalf("state events of the reopen: %v", reopened)
	}
	if ev := r.events(t, "reopened", th); len(ev) != 1 || ev[0]["principal"] != ownerPerson {
		t.Fatalf("reopened events: %v", ev)
	}
	r.posted(t, th, chat.ReopenedLine)

	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
	r.posted(t, th, "recall: reply first")
	s := r.lastStart(t, n)
	if !s.resumes() || !sameDir(s.Dir, ws) {
		t.Fatalf("the agent started in %s with %v, want a resume in %s", s.Dir, s.Args, ws)
	}
	if got := snapshot(t, ws); got != before {
		t.Fatalf("the reopen changed the workspace:\n%s\nwas\n%s", got, before)
	}
	if len(r.events(t, "session_replaced", th)) != 0 {
		t.Fatal("the conversation was replaced")
	}
}

// The owner's message in a closed thread is held and gets a Reopen button;
// a press by anyone else changes nothing, and the owner's press reopens
// the thread and delivers the held message. Anyone else's message gets
// the closed line alone.
func TestReopenButtonDeliversTheHeldMessage(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, _, _ := r.closed(t, 0, "first")

	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply not the owner's")
	plain := r.posted(t, th, chat.ClosedLine)
	if len(plain.Message.Choices) != 0 {
		t.Fatalf("a message not the owner's got choices %v", plain.Message.Choices)
	}
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "recall")
	reply := r.posted(t, th, chat.ClosedLine+"\n"+chat.ReopenLine)
	if len(reply.Message.Choices) != 1 || reply.Message.Choices[0].Label != "Reopen" {
		t.Fatalf("the owner's reply has choices %v", reply.Message.Choices)
	}
	token := reply.Message.Choices[0].Token
	turns := len(r.events(t, "turn_started", th))

	if err := r.driver.Press(reply.Ref, e2eHook, token); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if r.state(t, th) != "closed" || len(r.events(t, "reopened", th)) != 0 || len(r.events(t, "turn_started", th)) != turns {
		t.Fatalf("a press not the owner's: state %s, reopened %v", r.state(t, th), r.events(t, "reopened", th))
	}

	if err := r.driver.Press(reply.Ref, e2eOwner, token); err != nil {
		t.Fatal(err)
	}
	answer := r.posted(t, th, "recall: reply first")
	if !r.before(t, th, chat.ReopenedLine, "recall: reply first") {
		t.Fatal("the reopened line did not come before the answer")
	}
	if ev := r.events(t, "reopened", th); len(ev) != 1 || ev[0]["principal"] != ownerPerson {
		t.Fatalf("reopened events: %v", ev)
	}
	var inputs []string
	for _, e := range r.events(t, "input", th) {
		inputs = append(inputs, e["text"].(string))
	}
	if strings.Join(inputs, ",") != "reply first,recall" {
		t.Fatalf("inputs %v: the held message, and only it, should be sent", inputs)
	}
	_ = answer

	// A second press sends it no second time.
	if err := r.driver.Press(reply.Ref, e2eOwner, token); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(r.events(t, "input", th)); n != 2 {
		t.Fatalf("%d inputs after a second press", n)
	}

	// Held again, and reopened on the host meanwhile: a press by anyone but
	// the owner still sends nothing, the owner's sends the held message.
	num := numberOf(t, r.root, th)
	eventually(t, "the turn's end", r.within(15*time.Second), func() bool { return r.state(t, th) == "idle" })
	if code, _, errOut := cli(t, "close", "--root", r.root, num); code != 0 {
		t.Fatalf("hataori close: %d %q", code, errOut)
	}
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "reply held again")
	var again fakesurface.Call
	eventually(t, "the second Reopen button", r.within(15*time.Second), func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 1 && c.Ref != reply.Ref {
				again = c
				return true
			}
		}
		return false
	})
	r.reopen(t, num)
	if err := r.driver.Press(again.Ref, e2eHook, again.Message.Choices[0].Token); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(r.events(t, "input", th)); n != 2 {
		t.Fatalf("a press not the owner's sent the held message: %d inputs", n)
	}
	if err := r.driver.Press(again.Ref, e2eOwner, again.Message.Choices[0].Token); err != nil {
		t.Fatal(err)
	}
	r.posted(t, th, "held again")
	if ev := r.events(t, "reopened", th); len(ev) != 2 {
		t.Fatalf("reopened events %v: the owner's press should only send", ev)
	}
}

// A closed thread whose recorded conversation was deleted reopens with a
// new session in the same directory and the same chat thread, after one
// line that says so; the new conversation's first prompt says that the
// chat thread holds the earlier exchange.
func TestReopenWhenTheConversationIsGone(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, num, ws := r.closed(t, 0, "first")
	files, _ := filepath.Glob(filepath.Join(r.conversations, "*"))
	for _, f := range files {
		os.Remove(f) // as the agent's own retention does
	}
	n := len(r.starts(t))
	if got := r.reopen(t, num); got["workspace"] != "kept" || got["conversation"] != "resume" {
		t.Fatalf("hataori reopen printed %v", got)
	}

	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply again")
	r.posted(t, th, "again")
	if !r.before(t, th, chat.ReplacedLine, "again") {
		t.Fatal("no line before the new conversation's first answer")
	}
	if ev := r.events(t, "session_replaced", th); len(ev) != 1 {
		t.Fatalf("session_replaced %v", ev)
	}
	ss := r.starts(t)
	if len(ss) != n+2 { // the resume that failed, and the new conversation
		t.Fatalf("%d starts after the reopen, want 2", len(ss)-n)
	}
	for _, s := range ss[n:] {
		if !sameDir(s.Dir, ws) {
			t.Fatalf("a start in %s, want %s", s.Dir, ws)
		}
	}
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
	recall := r.postedWith(t, th, "recall: ")
	if !strings.Contains(recall.Message.Text, "The chat thread holds the earlier exchange") {
		t.Fatalf("the new conversation's first prompt began %q", recall.Message.Text)
	}
}

// A removed thread whose directory is still in trash reopens with its
// directory back in place, as it was, and its conversation resumed.
func TestReopenARemovedThreadFromTrash(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, num, ws := r.closed(t, 0, "first")
	before := snapshot(t, ws)
	if code, _, errOut := cli(t, "remove", "--root", r.root, num); code != 0 {
		t.Fatalf("hataori remove: %d %q", code, errOut)
	}
	if _, err := os.Lstat(ws); !os.IsNotExist(err) {
		t.Fatalf("the workspace is in place: %v", err)
	}
	n := len(r.starts(t))
	if got := r.reopen(t, num); got["workspace"] != "restored" || got["conversation"] != "resume" {
		t.Fatalf("hataori reopen printed %v", got)
	}
	if got := snapshot(t, ws); got != before {
		t.Fatalf("the restored workspace differs:\n%s\nwas\n%s", got, before)
	}
	if trashed, _ := filepath.Glob(filepath.Join(r.root, workspace.TrashDir, "*")); len(trashed) != 0 {
		t.Fatalf("trash still holds %v", trashed)
	}
	r.posted(t, th, chat.ReopenedLine+" "+chat.RestoredLine)
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
	r.posted(t, th, "recall: reply first")
	if s := r.lastStart(t, n); !s.resumes() || !sameDir(s.Dir, ws) {
		t.Fatalf("the agent started in %s with %v, want a resume in %s", s.Dir, s.Args, ws)
	}
}

// A removed thread whose directory the trash pass deleted reopens with its
// directory made again, a fresh clone of its repository, and a new
// conversation, after one line that says so. A removed thread whose place
// something else took is refused, and nothing is overwritten.
func TestReopenARemovedThreadAfterThePurge(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, num, ws := r.closed(t, 0, "first")
	taken, takenNum, takenWS := r.closed(t, 1, "second")
	for _, n := range []string{num, takenNum} {
		if code, _, errOut := cli(t, "remove", "--root", r.root, n); code != 0 {
			t.Fatalf("hataori remove: %d %q", code, errOut)
		}
	}
	trashed, _ := filepath.Glob(filepath.Join(r.root, workspace.TrashDir, "*-"+filepath.Base(ws)))
	if len(trashed) != 1 {
		t.Fatalf("trash holds %v", trashed)
	}
	if err := os.RemoveAll(trashed[0]); err != nil { // as the purge does
		t.Fatal(err)
	}
	// Something else at the place of the other thread's directory.
	if err := os.MkdirAll(takenWS, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(takenWS, "mine.txt"), []byte("not the thread's\n"), 0o600)
	code, _, errOut := cli(t, "reopen", "--root", r.root, takenNum)
	if code != exitError || !strings.Contains(errOut, "taken by something else") {
		t.Fatalf("reopen over a taken place: %d %q", code, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(takenWS, "mine.txt")); err != nil || string(b) != "not the thread's\n" {
		t.Fatalf("the directory at the taken place: %q %v", b, err)
	}
	if r.state(t, taken) != "removed" || len(r.events(t, "reopened", taken)) != 0 {
		t.Fatalf("a refused reopen: state %s", r.state(t, taken))
	}

	n := len(r.starts(t))
	if got := r.reopen(t, num); got["workspace"] != "remade" || got["conversation"] != "new" {
		t.Fatalf("hataori reopen printed %v", got)
	}
	if out, err := exec.Command("git", "-C", ws, "status", "--porcelain").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("the workspace made again: %q %v", out, err)
	}
	line := chat.ReopenedLine + " " + chat.RemadeLine + " " + chat.NewConversationLine
	r.posted(t, th, line)
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
	recall := r.postedWith(t, th, "recall: ")
	if !r.before(t, th, line, recall.Message.Text) {
		t.Fatal("the line did not come before the new conversation's first answer")
	}
	if recall.Message.Text != "recall: nothing" {
		t.Fatalf("the new conversation remembers the old one: %q", recall.Message.Text)
	}
	if s := r.lastStart(t, n); s.resumes() || !sameDir(s.Dir, ws) {
		t.Fatalf("the agent started in %s with %v, want a new conversation in %s", s.Dir, s.Args, ws)
	}
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
	eventually(t, "the second recall", r.within(15*time.Second), func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "The chat thread holds the earlier exchange") {
				return true
			}
		}
		return false
	})
}

// A chat thread archived while its thread was closed is open again after
// a reopen, also in a scope that does not archive: there the chat service
// archived it.
func TestReopenUnarchivesTheChatThread(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newReopenRoot(t, func(cfg map[string]any) {
			cfg["scopes"].([]map[string]any)[0]["archive_when_asleep"] = on
		})
		th, num, _ := r.closed(t, 0, "first")
		if on {
			eventually(t, "the chat thread archived", r.within(10*time.Second), func() bool { return r.driver.Archived(th) })
		} else if err := r.driver.SetThread(t.Context(), th, surface.ThreadMeta{Archived: true}); err != nil { // as the service does
			t.Fatal(err)
		}
		r.reopen(t, num)
		eventually(t, "the chat thread open again", r.within(10*time.Second), func() bool { return !r.driver.Archived(th) })
	}
}

// A reopen that put a removed thread's directory back and stopped before
// it recorded the reopen, as when the daemon died in between, is finished
// by the next one: it knows the directory at the place for the one it put
// there, by the identity it recorded first, and the conversation resumes.
// A directory of anyone else at that place is still refused.
func TestReopenAfterACrashKnowsItsDirectory(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, num, ws := r.closed(t, 0, "first")
	if code, _, errOut := cli(t, "remove", "--root", r.root, num); code != 0 {
		t.Fatalf("hataori remove: %d %q", code, errOut)
	}
	trashed, _ := filepath.Glob(filepath.Join(r.root, workspace.TrashDir, "*-"+filepath.Base(ws)))
	if len(trashed) != 1 {
		t.Fatalf("trash holds %v", trashed)
	}
	// What the reopen did before it stopped: the record, then the rename.
	st, err := os.Lstat(trashed[0])
	if err != nil {
		t.Fatal(err)
	}
	dev, ino := devIno(t, st)
	rec, _ := json.Marshal(map[string]any{"dir": ws, "dev": dev, "inode": ino, "workspace": "restored"})
	db := r.writableDB(t)
	if _, err := db.Exec(`UPDATE threads SET reopen_placed = ? WHERE id = ?`, string(rec), num); err != nil {
		t.Fatal(err)
	}
	// Someone else's directory at the place first: still refused.
	os.MkdirAll(ws, 0o700)
	if code, _, errOut := cli(t, "reopen", "--root", r.root, num); code != exitError || !strings.Contains(errOut, "taken by something else") {
		t.Fatalf("reopen over a directory that is not the recorded one: %d %q", code, errOut)
	}
	os.Remove(ws)
	if err := os.Rename(trashed[0], ws); err != nil {
		t.Fatal(err)
	}
	n := len(r.starts(t))
	if got := r.reopen(t, num); got["workspace"] != "restored" || got["conversation"] != "resume" {
		t.Fatalf("hataori reopen printed %v", got)
	}
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
	r.posted(t, th, "recall: reply first")
	if s := r.lastStart(t, n); !s.resumes() || !sameDir(s.Dir, ws) {
		t.Fatalf("the agent started in %s with %v, want a resume in %s", s.Dir, s.Args, ws)
	}
	var left string
	db.QueryRow(`SELECT reopen_placed FROM threads WHERE id = ?`, num).Scan(&left)
	if left != "" {
		t.Fatalf("the record stays after the reopen: %s", left)
	}
}

// A removed workspace whose entry in trash was not recorded, as by a daemon
// that died in between or one of before it was recorded, is taken back all
// the same. A closed thread whose place holds something that is no
// directory is refused, and that is left as it is.
func TestReopenFindsAnUnrecordedTrashEntry(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, num, ws := r.closed(t, 0, "first")
	before := snapshot(t, ws)
	away := ws + "-away"
	if err := os.Rename(ws, away); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(away, ws); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, "reopen", "--root", r.root, num); code != exitError || !strings.Contains(errOut, "no directory") {
		t.Fatalf("reopen with a link at the place: %d %q", code, errOut)
	}
	if st, err := os.Lstat(ws); err != nil || st.Mode()&os.ModeSymlink == 0 || r.state(t, th) != "closed" {
		t.Fatalf("after the refusal: %v %v, state %s", st, err, r.state(t, th))
	}
	os.Remove(ws)
	if err := os.Rename(away, ws); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, "remove", "--root", r.root, num); code != 0 {
		t.Fatalf("hataori remove: %d %q", code, errOut)
	}
	// As a daemon that died between the move and its record left it.
	db := r.writableDB(t)
	for _, q := range []string{`UPDATE threads SET trash = '', state = 'closed'`, `UPDATE sessions SET state = 'closed'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.reopen(t, num); got["workspace"] != "restored" || got["conversation"] != "resume" {
		t.Fatalf("hataori reopen printed %v", got)
	}
	if got := snapshot(t, ws); got != before {
		t.Fatalf("the restored workspace differs:\n%s\nwas\n%s", got, before)
	}
}

// Two held messages whose buttons are pressed right after each other are
// both sent, though the thread is still starting for the first.
func TestTwoHeldMessagesAreBothSent(t *testing.T) {
	r := newReopenRoot(t, nil)
	th, _, _ := r.closed(t, 0, "first")
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "reply one")
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "reply two")
	var buttons []fakesurface.Call
	eventually(t, "two Reopen buttons", r.within(15*time.Second), func() bool {
		buttons = nil
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 1 && c.Message.Choices[0].Label == "Reopen" {
				buttons = append(buttons, c)
			}
		}
		return len(buttons) == 2
	})
	for _, b := range buttons {
		if err := r.driver.Press(b.Ref, e2eOwner, b.Message.Choices[0].Token); err != nil {
			t.Fatal(err)
		}
	}
	r.posted(t, th, "one")
	r.posted(t, th, "two")
	if ev := r.events(t, "reopened", th); len(ev) != 1 {
		t.Fatalf("reopened events %v", ev)
	}
	if ev := r.events(t, "input", th); len(ev) != 3 {
		t.Fatalf("%d inputs, want the first and the two held", len(ev))
	}
}

// writableDB opens the instance's database to change it beside the daemon,
// as a test that stands for what a daemon that died left behind.
func (r *chatRoot) writableDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// devIno is the device and inode of st.
func devIno(t *testing.T, st os.FileInfo) (uint64, uint64) {
	t.Helper()
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no device and inode")
	}
	return uint64(sys.Dev), uint64(sys.Ino)
}

// sameDir reports whether a and b are one directory, their links resolved:
// on macOS a temporary directory under /var is /private/var to the agent.
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
