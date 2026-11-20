package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #590 with the fake agent in a terminal, whose
// start-up dialog names a file the scope does not trust: the start is
// blocked and its message carries the trust button; a press by anyone but
// the owner records nothing; the owner's press records the trust, and the
// thread starts and runs its first turn; a later thread on the same
// repository starts without asking, one on another repository does not;
// and once the trust is withdrawn the next start is blocked again.
func TestTrustTheFilesOfABlockedStartFromChat(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Terminal: true, Hataori: os.Args[0],
			Dialog: "Do you trust the files in this folder? Enter to go on", DialogTrust: []string{".mcp.json"}}}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	startDaemon(t, r.root)
	r.connected(t)
	repo := bareRepo(t)

	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply first answer")
	th := r.thread(t, 0)
	msg, token := r.trustButton(t, th)
	if !strings.Contains(msg.Message.Text, "This thread could not start: blocked at start") ||
		!strings.Contains(msg.Message.Text, ".mcp.json") || !strings.Contains(msg.Message.Text, repo) {
		t.Fatalf("the message: %q", msg.Message.Text)
	}

	// Pressed by one of the scope's people who is not the owner: nothing.
	if err := r.driver.Press(msg.Ref, e2eHook, token); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := r.trusted(t); n != 0 {
		t.Fatalf("a press by another person recorded %d files", n)
	}
	if s := r.state(t, th); s != "failed" {
		t.Fatalf("after another person's press the thread is %s", s)
	}

	// The owner's press: recorded, and the thread runs its first turn.
	if err := r.driver.Press(msg.Ref, e2eOwner, token); err != nil {
		t.Fatal(err)
	}
	r.posted(t, th, "first answer")
	if n := r.trusted(t); n != 1 {
		t.Fatalf("%d files trusted, want 1", n)
	}
	eventually(t, "the button to go", 10*time.Second, func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpEdit && c.Ref == msg.Ref && len(c.Message.Choices) == 0 &&
				strings.Contains(c.Message.Text, "Trusted by owner") {
				return true
			}
		}
		return false
	})

	// The dashboard lists it.
	base, cookie := dashSignIn(t, r.root)
	if code, body := dashGet(t, base, cookie, "/api/trusts"); code != 200 || !strings.Contains(body, `"file":".mcp.json"`) ||
		!strings.Contains(body, `"by":"owner"`) {
		t.Fatalf("/api/trusts: %d %s", code, body)
	}

	// The same repository: no dialog to stop it.
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply second answer")
	th2 := r.thread(t, 1)
	r.posted(t, th2, "second answer")
	if r.hasButton(th2) {
		t.Fatal("a thread on the trusted repository got the button")
	}

	// Another repository is not trusted.
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply third answer")
	r.trustButton(t, r.thread(t, 2))

	// Withdrawn: the next start on the first repository is blocked again.
	code, out, errOut := cli(t, "trusts", "--root", r.root)
	var tf struct {
		ID   int64
		Repo string
		File string
	}
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &tf) != nil || tf.Repo != repo || tf.File != ".mcp.json" {
		t.Fatalf("hataori trusts: %d %q %q", code, out, errOut)
	}
	if code, out, errOut := cli(t, "untrust", strconv.FormatInt(tf.ID, 10), "--root", r.root); code != 0 {
		t.Fatalf("hataori untrust: %d %q %q", code, out, errOut)
	}
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply fourth answer")
	r.trustButton(t, r.thread(t, 3))
}

// trustButton waits for the "could not start" message in th with its trust
// button, and returns it and the button's token.
func (r *chatRoot) trustButton(t *testing.T, th surface.Conversation) (fakesurface.Call, string) {
	t.Helper()
	var got fakesurface.Call
	eventually(t, "the trust button", r.within(20*time.Second), func() bool {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 1 && strings.HasPrefix(c.Message.Choices[0].Label, "Trust") {
				got = c
				return true
			}
		}
		return false
	})
	return got, got.Message.Choices[0].Token
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
