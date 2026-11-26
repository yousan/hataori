package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #590 with the fake agent in a terminal, whose
// start-up dialog names a settings file the scope does not trust and whose
// repository has a hook of its own (fake.Config.ProjectHook). The blocked
// start's message says what to do next and offers three choices:
//
//   - "Start without this repository's settings", pressed by one of the
//     scope's people: the thread runs its first turn with the hook not
//     run, and still not after it sleeps and wakes;
//   - "Trust and start": a press by anyone but the owner records nothing;
//     the owner's records the trust and the thread runs, with the hook; a
//     later thread on that repository starts without asking, one on
//     another repository does not; withdrawn, the next start is blocked;
//   - "Do not start": a guest's press changes nothing; one of the scope's
//     people closes the thread.
func TestTheChoicesOfABlockedStart(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "2s"
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Terminal: true, Hataori: os.Args[0],
			Dialog: "Do you trust the files in this folder? Enter to go on", DialogTrust: []string{".claude/settings.json"},
			ProjectHook: true}}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	startDaemon(t, r.root)
	r.connected(t)

	// 1. Without the repository's settings.
	other := bareRepo(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+other+" reply first without")
	thA := r.thread(t, 0)
	msg := r.blockedMessage(t, thA)
	for _, want := range []string{"This thread could not start: \n" + r.driver.Verbatim("blocked at start"), "What to do next: choose below.",
		`"trust_files" of the scope "c1"`, ".claude/settings.json"} {
		if !strings.Contains(msg.Message.Text, want) {
			t.Fatalf("the message has no %q: %q", want, msg.Message.Text)
		}
	}
	if err := r.driver.Press(msg.Ref, e2eHook, choiceToken(t, msg, "Start without this repository's settings")); err != nil {
		t.Fatal(err)
	}
	r.posted(t, thA, "first without")
	r.edited(t, thA, msg, "Started without this repository's settings by ")
	if r.hookRan(t, thA) {
		t.Fatal("the repository's hook ran in a thread started without its settings")
	}
	eventually(t, "the thread to sleep", r.within(20*time.Second), func() bool { return r.state(t, thA) == "asleep" })
	r.driver.Inject(thA, fakesurface.Webhook(e2eHook), "reply after the wake")
	r.posted(t, thA, "after the wake")
	if r.hookRan(t, thA) {
		t.Fatal("the repository's hook ran after the thread woke")
	}

	// 2. Trust and start.
	repo := bareRepo(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply first answer")
	thB := r.thread(t, 1)
	msg = r.blockedMessage(t, thB)
	trust := choiceToken(t, msg, "Trust and start")
	if err := r.driver.Press(msg.Ref, e2eHook, trust); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := r.trusted(t); n != 0 || r.state(t, thB) != "failed" {
		t.Fatalf("a press by another person: %d files trusted, the thread %s", n, r.state(t, thB))
	}
	if err := r.driver.Press(msg.Ref, e2eOwner, trust); err != nil {
		t.Fatal(err)
	}
	r.posted(t, thB, "first answer")
	r.edited(t, thB, msg, "Trusted by owner")
	if n := r.trusted(t); n != 1 {
		t.Fatalf("%d files trusted, want 1", n)
	}
	if !r.hookRan(t, thB) {
		t.Fatal("the repository's hook did not run in the trusted thread: the check above proves nothing")
	}
	base, cookie := dashSignIn(t, r.root)
	if code, body := dashGet(t, base, cookie, "/api/trusts"); code != 200 || !strings.Contains(body, `"file":".claude/settings.json"`) {
		t.Fatalf("/api/trusts: %d %s", code, body)
	}
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply second answer")
	thC := r.thread(t, 2)
	r.posted(t, thC, "second answer")
	if r.hasButton(thC) {
		t.Fatal("a thread on the trusted repository got the choices")
	}
	code, out, errOut := cli(t, "trusts", "--root", r.root)
	var tf struct {
		ID   int64
		Repo string
	}
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &tf) != nil || tf.Repo != repo {
		t.Fatalf("hataori trusts: %d %q %q", code, out, errOut)
	}
	if code, out, errOut := cli(t, "untrust", strconv.FormatInt(tf.ID, 10), "--root", r.root); code != 0 {
		t.Fatalf("hataori untrust: %d %q %q", code, out, errOut)
	}

	// 3. Do not start, on the withdrawn repository: blocked again.
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply never")
	thD := r.thread(t, 3)
	msg = r.blockedMessage(t, thD)
	closing := choiceToken(t, msg, "Do not start")
	if err := r.driver.Press(msg.Ref, "guest-1", closing); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if s := r.state(t, thD); s != "failed" {
		t.Fatalf("after a guest's press the thread is %s", s)
	}
	if err := r.driver.Press(msg.Ref, e2eHook, closing); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread to close", r.within(10*time.Second), func() bool { return r.state(t, thD) == "closed" })
	r.edited(t, thD, msg, "Not started: closed by ")
}

// blockedMessage waits for the "could not start" message in th with its
// three choices.
func (r *chatRoot) blockedMessage(t *testing.T, th surface.Conversation) fakesurface.Call {
	t.Helper()
	var got fakesurface.Call
	eventually(t, "the choices", r.within(20*time.Second), func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 3 {
				got = c
				return true
			}
		}
		return false
	})
	return got
}

// edited waits for msg's edit without buttons, saying done.
func (r *chatRoot) edited(t *testing.T, th surface.Conversation, msg fakesurface.Call, done string) {
	t.Helper()
	eventually(t, "the buttons to go", r.within(10*time.Second), func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpEdit && c.Ref == msg.Ref && len(c.Message.Choices) == 0 && strings.Contains(c.Message.Text, done) {
				return true
			}
		}
		return false
	})
}

func (r *chatRoot) hasButton(th surface.Conversation) bool {
	for _, c := range r.driver.Calls(th) {
		if len(c.Message.Choices) > 0 {
			return true
		}
	}
	return false
}

func (r *chatRoot) trusted(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.db(t).QueryRow(`SELECT count(*) FROM trusted_files`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// hookRan reports whether the fake's project hook ran in th's workspace.
func (r *chatRoot) hookRan(t *testing.T, th surface.Conversation) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(r.workspace(t, th), fake.ProjectHookFile))
	return err == nil
}

// workspace is th's workspace.
func (r *chatRoot) workspace(t *testing.T, th surface.Conversation) string {
	t.Helper()
	var dir string
	if err := r.db(t).QueryRow(`SELECT workspace FROM threads WHERE id = ?`, numberOf(t, r.root, th)).Scan(&dir); err != nil || dir == "" {
		t.Fatalf("the thread's workspace: %q %v", dir, err)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(r.root, dir)
	}
	return dir
}
