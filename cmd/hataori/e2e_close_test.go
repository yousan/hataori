package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/yousan/hataori/internal/control"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/workspace"
)

// idleThread starts a thread in c1 with one turn, waits until that turn has
// ended, and returns the chat thread, its session and its workspace.
func (r *chatRoot) idleThread(t *testing.T, n int, word string) (surface.Conversation, int64, string) {
	t.Helper()
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply "+word)
	th := r.thread(t, n)
	r.posted(t, th, word)
	eventually(t, "the end of the first turn", r.within(15*time.Second), func() bool {
		return len(r.events(t, "turn_ended", th)) == 1 && r.state(t, th) == "idle"
	})
	created := r.events(t, "thread_created", th)
	if len(created) != 1 {
		t.Fatalf("thread_created %v", created)
	}
	sid := int64(created[0]["session"].(float64))
	num, _ := strconv.ParseInt(numberOf(t, r.root, th), 10, 64)
	return th, sid, workspace.Path(r.root, control.ThreadID(num))
}

// state is the thread's state as `hataori thread` prints it.
func (r *chatRoot) state(t *testing.T, th surface.Conversation) string {
	t.Helper()
	code, out, errOut := cli(t, "thread", "--root", r.root, numberOf(t, r.root, th))
	if code != 0 {
		t.Fatalf("hataori thread: %d %s", code, errOut)
	}
	var l struct{ State string }
	json.Unmarshal([]byte(out), &l)
	return l.State
}

// snapshot is everything a workspace holds that a person could lose: every
// file outside .git with its mode and contents, and every ref and the
// stash.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		line := rel + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	out, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	return strings.Join(lines, "\n") + "\n--\n" + string(out) + string(head)
}

// hostEnded reports whether the session's host and the agent's process
// group are gone.
func hostEnded(root string, sid int64) bool {
	dir := runner.StateDir(root, sid)
	for _, f := range []string{host.PIDFile, host.HostPIDFile} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && alive(pid) && !zombie(pid) && !psZombie(pid) {
			return false
		}
	}
	return host.Gone(dir)
}

// psZombie is zombie where there is no /proc, as on macOS: the host, a
// child of the daemon in this test process, waits there to be reaped.
func psZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// hataori close on an idle thread ends its agent, closes the thread and
// leaves the workspace as it was; a message in the thread then starts no
// turn and gets one line. hataori remove on it moves the workspace into
// trash and the thread is removed; on an idle thread it is refused.
func TestCloseThenRemoveAThread(t *testing.T) {
	r := newChatRoot(t, nil)
	r.deadline = time.Now().Add(90 * time.Second)
	startDaemon(t, r.root)
	r.connected(t)
	th, sid, ws := r.idleThread(t, 0, "first")
	other, _, otherWS := r.idleThread(t, 1, "second")
	before := snapshot(t, ws)

	// Remove before close is refused, and changes nothing.
	code, _, errOut := cli(t, "remove", "--root", r.root, numberOf(t, r.root, other))
	if code != exitError || !strings.Contains(errOut, "not closed") {
		t.Fatalf("remove of an idle thread: %d %q", code, errOut)
	}
	if r.state(t, other) != "idle" {
		t.Fatalf("after a refused removal the idle thread is %s", r.state(t, other))
	}
	if _, err := os.Stat(otherWS); err != nil {
		t.Fatalf("the idle thread's workspace: %v", err)
	}
	if ev := r.events(t, "removal_refused", other); len(ev) != 1 || ev[0]["reason"] != "thread not closed" {
		t.Fatalf("removal_refused of the idle thread: %v", ev)
	}

	code, out, errOut := cli(t, "close", "--root", r.root, numberOf(t, r.root, th))
	if code != 0 || !strings.Contains(out, `"state":"closed"`) {
		t.Fatalf("hataori close: %d %q %q", code, out, errOut)
	}
	if !hostEnded(r.root, sid) {
		t.Fatal("hataori close returned while the agent's process runs")
	}
	if r.state(t, th) != "closed" {
		t.Fatalf("state %s, want closed", r.state(t, th))
	}
	if got := snapshot(t, ws); got != before {
		t.Fatalf("closing changed the workspace:\n%s\nwas\n%s", got, before)
	}

	// A message starts nothing and gets one line.
	turns := len(r.events(t, "turn_started", th))
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply too late")
	notice := r.posted(t, th, chat.ClosedLine)
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, c := range r.driver.Calls(th) {
		if c.Op == fakesurface.OpPost && c.Message.Text == chat.ClosedLine {
			n++
		}
	}
	if n != 1 || notice.Message.ReplyTo == nil {
		t.Fatalf("%d closed lines, reply to %v", n, notice.Message.ReplyTo)
	}
	if got := len(r.events(t, "turn_started", th)); got != turns || r.state(t, th) != "closed" {
		t.Fatalf("a message to the closed thread started %d turns; state %s", got-turns, r.state(t, th))
	}
	if !hostEnded(r.root, sid) {
		t.Fatal("a message woke the closed thread's agent")
	}

	code, out, errOut = cli(t, "remove", "--root", r.root, numberOf(t, r.root, th))
	if code != 0 || !strings.Contains(out, `"state":"removed"`) {
		t.Fatalf("hataori remove: %d %q %q", code, out, errOut)
	}
	if _, err := os.Lstat(ws); !os.IsNotExist(err) {
		t.Fatalf("the workspace is still in place: %v", err)
	}
	trashed, _ := filepath.Glob(filepath.Join(r.root, workspace.TrashDir, "*-"+filepath.Base(ws)))
	if len(trashed) != 1 {
		t.Fatalf("trash holds %v", trashed)
	}
	if got := snapshot(t, trashed[0]); got != before {
		t.Fatalf("the workspace in trash differs:\n%s\nwas\n%s", got, before)
	}
	if r.state(t, th) != "removed" {
		t.Fatalf("state %s, want removed", r.state(t, th))
	}
	var removed []map[string]any
	for _, e := range r.events(t, "state", th) {
		if e["to"] == "removed" {
			removed = append(removed, e)
		}
	}
	if len(removed) != 1 || removed[0]["reason"] != "command" || len(r.events(t, "removal_refused", th)) != 0 {
		t.Fatalf("removal events: %v, refused %v", removed, r.events(t, "removal_refused", th))
	}
}

// hataori remove on a closed thread whose workspace holds an unpushed
// commit is refused with the reason, writes one event, leaves the thread
// closed and the workspace unchanged.
func TestRemoveRefusesAnUnpushedCommit(t *testing.T) {
	r := newChatRoot(t, nil)
	r.deadline = time.Now().Add(90 * time.Second)
	startDaemon(t, r.root)
	r.connected(t)
	th, _, ws := r.idleThread(t, 0, "first")
	os.WriteFile(filepath.Join(ws, "work.txt"), []byte("only here\n"), 0o600)
	gitIn(t, ws, "add", "work.txt")
	gitIn(t, ws, "commit", "-q", "-m", "work that exists nowhere else")
	if code, _, errOut := cli(t, "close", "--root", r.root, numberOf(t, r.root, th)); code != 0 {
		t.Fatalf("hataori close: %d %q", code, errOut)
	}
	before := snapshot(t, ws)

	code, _, errOut := cli(t, "remove", "--root", r.root, numberOf(t, r.root, th))
	if code != exitError || !strings.Contains(errOut, "removal refused") || !strings.Contains(errOut, "unpushed commit") {
		t.Fatalf("hataori remove: %d %q", code, errOut)
	}
	if got := snapshot(t, ws); got != before {
		t.Fatalf("a refused removal changed the workspace:\n%s\nwas\n%s", got, before)
	}
	if r.state(t, th) != "closed" {
		t.Fatalf("state %s, want closed", r.state(t, th))
	}
	if trashed, _ := filepath.Glob(filepath.Join(r.root, workspace.TrashDir, "*")); len(trashed) != 0 {
		t.Fatalf("trash holds %v", trashed)
	}
	ev := r.events(t, "removal_refused", th)
	if len(ev) != 1 || ev[0]["reason"] != "unpushed commit on a branch" || ev[0]["trigger"] != "command" {
		t.Fatalf("removal_refused events: %v", ev)
	}
}

// With cleanup.remove_closed_after set, the daemon removes the workspace of
// a thread closed longer than that by itself, and says so in the state
// event; a thread that is not closed is left alone.
func TestRemovalPassRemovesAThreadClosedLongEnough(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["cleanup"] = map[string]any{"remove_closed_after": "1s"}
	})
	r.deadline = time.Now().Add(90 * time.Second)
	startDaemon(t, r.root)
	r.connected(t)
	th, _, ws := r.idleThread(t, 0, "first")
	open, _, openWS := r.idleThread(t, 1, "second")
	if code, _, errOut := cli(t, "close", "--root", r.root, numberOf(t, r.root, th)); code != 0 {
		t.Fatalf("hataori close: %d %q", code, errOut)
	}
	eventually(t, "the removal pass", r.within(30*time.Second), func() bool { return r.state(t, th) == "removed" })
	if _, err := os.Lstat(ws); !os.IsNotExist(err) {
		t.Fatalf("the workspace is still in place: %v", err)
	}
	var reason any
	for _, e := range r.events(t, "state", th) {
		if e["to"] == "removed" {
			reason = e["reason"]
		}
	}
	if reason != "closed age" {
		t.Fatalf("the removal's reason: %v", reason)
	}
	if r.state(t, open) != "idle" {
		t.Fatalf("the open thread is %s", r.state(t, open))
	}
	if _, err := os.Stat(openWS); err != nil {
		t.Fatalf("the open thread's workspace: %v", err)
	}
}
