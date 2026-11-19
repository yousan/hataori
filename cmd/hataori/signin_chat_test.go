package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/wire"
)

// A person who asks in chat gets a link through the chat front end's caller
// (From set): the owner's link signs in once to a session that sees every
// scope; a guest and a person who may not sign in get none, each with one
// log line naming the client and the id. The token of the link made and
// used is found in no file under the root, the database included, and in
// nothing the daemon logged.
func TestSignInLinkForAPersonInChat(t *testing.T) {
	root := newRoot(t)
	p := filepath.Join(root, instance.ConfigFile)
	b, _ := os.ReadFile(p)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["people"] = []map[string]any{
		{"name": "ada", "ids": []string{"chat:111"}},
		{"name": "grace", "ids": []string{"chat:333"}},
	}
	cfg["owner"] = "ada"
	cfg["scopes"] = []map[string]any{
		{"key": "s", "client": "chat", "enabled": true},
		{"key": "x", "client": "chat", "enabled": true, "people": []string{"grace"}},
	}
	cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	b, _ = json.Marshal(cfg)
	os.WriteFile(p, b, 0o600)
	var c *core.Core
	coreHook = func(got *core.Core) { c = got }
	t.Cleanup(func() { coreHook = nil })
	stop, logged := runDaemonLog(t, root)

	ctx := context.Background()
	wc, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	repo := bareRepo(t)
	for _, s := range []control.StartRequest{
		{Key: "k1", Scope: "s", Thread: "t1", Repo: repo, Input: control.Input{Text: "reply one"}},
		{Key: "k2", Scope: "x", Thread: "t2", Repo: repo, Input: control.Input{Text: "reply two"}},
	} {
		if _, err := wc.Start(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	// As a chat front end asks: the author's id, and the front end's scopes.
	chat := func(from string) control.API {
		return c.As(control.Caller{From: from, Scopes: []string{"s"}, May: control.OpRead | control.OpSend | control.OpSignIn})
	}
	for _, from := range []string{"chat:999", "chat:333"} {
		before := strings.Count(logged(), "\n")
		if link, err := chat(from).SignInLink(ctx, ""); !errors.Is(err, control.ErrForbidden) {
			t.Fatalf("%s: %q, %v", from, link, err)
		}
		var refused []string
		for _, l := range strings.Split(logged(), "\n")[before:] {
			if strings.Contains(l, "SignInLink") {
				refused = append(refused, l)
			}
		}
		_, id, _ := strings.Cut(from, ":")
		if len(refused) != 1 || !strings.Contains(refused[0], "client=chat") || !strings.Contains(refused[0], "id="+id) {
			t.Fatalf("%s: logged %q", from, refused)
		}
	}

	link, err := chat("chat:111").SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	fragment, _ := url.ParseQuery(u.Fragment)
	token := fragment.Get("t")
	if err != nil || len(token) < 32 {
		t.Fatalf("link %q", link)
	}
	cookie := openLink(t, link)
	if code, _, s := tryLink(t, &http.Client{}, link); code != http.StatusUnauthorized || s != "" {
		t.Fatalf("the link a second time: %d", code)
	}
	base := link[:strings.Index(link, "/signin")]
	eventually(t, "both threads in the owner's session", 10*time.Second, func() bool {
		code, threads := dashGet(t, base, cookie, "/api/threads")
		return code == http.StatusOK && strings.Contains(threads, `"thread":"t1"`) && strings.Contains(threads, `"thread":"t2"`)
	})

	stop() // the database and the logs as the daemon left them
	logs := logged()
	if strings.Contains(logs, token) {
		t.Fatal("the daemon logged the token")
	}
	files := 0
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files++
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("the token is in %s", strings.TrimPrefix(path, root))
		}
		return nil
	})
	if files == 0 {
		t.Fatal("no file read")
	}
}
