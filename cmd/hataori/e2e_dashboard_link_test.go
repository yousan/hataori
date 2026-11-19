package main

import (
	"bytes"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #259: a dashboard sign-in link asked for in
// chat is sent in a direct message only.

// linkRoot is a chat root with a dashboard on loopback, an owner ada, and
// grace, allowed in c1 but not to get a link.
func linkRoot(t *testing.T, dashboard bool) *chatRoot {
	return newChatRoot(t, func(cfg map[string]any) {
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "grace", "ids": []string{fakeSurface + ":g-1"}},
		}
		sc := cfg["scopes"].([]map[string]any)[0]
		delete(sc, "posters")
		sc["people"] = []string{"grace"}
		if dashboard {
			cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		}
	})
}

// directPosts returns what was posted to who in a direct conversation.
func directPosts(d *fakesurface.Driver, who string) []fakesurface.Call {
	conv, _ := d.OpenDirect(nil, who)
	var out []fakesurface.Call
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpPost {
			out = append(out, c)
		}
	}
	return out
}

var linkRE = regexp.MustCompile(`https?://\S+/signin#t=\S+`)

// askLink asks for a link as the owner, with the command or with the words
// after the trigger, and returns the link sent in the direct message.
func askLink(t *testing.T, r *chatRoot, words bool) string {
	t.Helper()
	before := len(directPosts(r.driver, e2eOwner))
	if words {
		r.driver.Inject(c1, fakesurface.Person(e2eOwner), "!go dashboard")
	} else {
		r.driver.InjectCommand(c1, fakesurface.Person(e2eOwner), "dashboard")
	}
	var link string
	eventually(t, "the direct message", 15*time.Second, func() bool {
		p := directPosts(r.driver, e2eOwner)
		if len(p) > before {
			link = linkRE.FindString(p[len(p)-1].Message.Text)
		}
		return len(p) > before
	})
	time.Sleep(300 * time.Millisecond)
	if n := len(directPosts(r.driver, e2eOwner)) - before; n != 1 || link == "" {
		t.Fatalf("%d direct messages, link %q; want one with a link", n, link)
	}
	return link
}

// nothingIn fails when anything was posted, edited or lamped in conv.
func nothingIn(t *testing.T, d *fakesurface.Driver, conv surface.Conversation) {
	t.Helper()
	for _, c := range d.Calls(conv) {
		if c.Op != fakesurface.OpOpenThread {
			t.Fatalf("in %+v: %+v", conv, c)
		}
	}
}

func TestTheOwnersDashboardCommandSendsOneDirectMessageThatSignsIn(t *testing.T) {
	for _, words := range []bool{false, true} {
		r := linkRoot(t, true)
		startDaemon(t, r.root)
		r.connected(t)
		link := askLink(t, r, words)
		openLink(t, link)
		nothingIn(t, r.driver, c1)
	}
}

func TestDashboardCommandOfAGuestOrSomeoneElseSendsNothing(t *testing.T) {
	r := linkRoot(t, true)
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	for _, who := range []string{"x-9", "g-1"} {
		before := strings.Count(logged(), "id="+who)
		r.driver.InjectCommand(c1, fakesurface.Person(who), "dashboard")
		eventually(t, "the refusal in the log", 5*time.Second, func() bool { return strings.Count(logged(), "id="+who) > before })
		time.Sleep(300 * time.Millisecond)
		if n := strings.Count(logged(), "id="+who) - before; n != 1 {
			t.Fatalf("%s: %d log lines with the id, want 1:\n%s", who, n, logged())
		}
		if p := directPosts(r.driver, who); len(p) != 0 {
			t.Fatalf("%s got direct messages %+v", who, p)
		}
	}
	nothingIn(t, r.driver, c1)
	if p := directPosts(r.driver, e2eOwner); len(p) != 0 {
		t.Fatalf("the owner got %+v", p)
	}
}

func TestTheLinksTokenIsInNoFileAndNoLog(t *testing.T) {
	r := linkRoot(t, true)
	stop, logged := runDaemonLog(t, r.root)
	r.connected(t)
	link := askLink(t, r, false)
	openLink(t, link)
	u, _ := url.Parse(link)
	token := strings.TrimPrefix(u.Fragment, "t=")
	if len(token) < 32 {
		t.Fatalf("token %q", token)
	}
	stop() // the database and the logs as the daemon left them
	if strings.Contains(logged(), token) {
		t.Fatal("the daemon logged the token")
	}
	files := 0
	filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files++
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("the token is in %s", strings.TrimPrefix(path, r.root))
		}
		return nil
	})
	if files == 0 {
		t.Fatal("no file read")
	}
}

func TestDashboardCommandWithoutADashboardAnswersOneLine(t *testing.T) {
	r := linkRoot(t, false)
	startDaemon(t, r.root)
	r.connected(t)
	c := r.driver.InjectCommand(c1, fakesurface.Person(e2eOwner), "dashboard")
	answer := r.posted(t, c1, "This hataori instance has no dashboard.")
	if answer.Message.ReplyTo == nil || *answer.Message.ReplyTo != *c.Reply {
		t.Fatalf("answer %+v, want the command's", answer.Message)
	}
	time.Sleep(300 * time.Millisecond)
	posts := 0
	for _, x := range r.driver.Calls(c1) {
		if x.Op == fakesurface.OpPost {
			posts++
		}
	}
	if p := directPosts(r.driver, e2eOwner); posts != 1 || len(p) != 0 {
		t.Fatalf("%d posts in the channel and direct messages %+v; want the one line only", posts, p)
	}
}
