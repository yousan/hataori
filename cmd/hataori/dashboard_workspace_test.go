package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/wire"
)

// workspaceDir is the workspace of the only session under root, as its
// session host was given it.
func workspaceDir(t *testing.T, root string) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	if len(dirs) != 1 {
		t.Fatalf("%d sessions", len(dirs))
	}
	spec, ok, err := host.ReadSpec(dirs[0])
	if err != nil || !ok || spec.Dir == "" {
		t.Fatalf("the session's spec: %+v %v %v", spec, ok, err)
	}
	return spec.Dir
}

// gitIn runs git in dir as a person working there would.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// rawGet is a GET of a dashboard route with the given Host and Origin,
// and the session cookie when c is not nil.
func rawGet(t *testing.T, method, base, path string, c *http.Cookie, hostHeader, origin string) (int, string) {
	t.Helper()
	r, _ := http.NewRequest(method, base+path, nil)
	if c != nil {
		r.AddCookie(c)
	}
	if hostHeader != "" {
		r.Host = hostHeader
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The workspace route answers the control API's Workspace for a real git
// workspace, clean, with changes and with unpushed commits; only to a
// signed-in session through the dashboard's own Host and Origin; a thread
// outside the caller's scopes as no thread; a git failure as an error,
// never zeros; and nothing but GET or HEAD.
func TestDashboardWorkspaceRoute(t *testing.T) {
	root := terminalRoot(t) // the page's caller sees scope s, not x
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo := bareRepo(t)
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: repo, Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread to be idle", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Idle
	})
	ws := workspaceDir(t, root)
	base, cookie := dashSignIn(t, root)
	path := func(id control.ThreadID) string {
		return "/api/workspace?" + url.Values{"scope": {id.Scope}, "key": {id.Key}}.Encode()
	}
	// state reads the route and the control API, and checks they agree.
	state := func(what string) control.WorkspaceState {
		t.Helper()
		code, body := dashGet(t, base, cookie, path(id))
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", what, code, body)
		}
		var got control.WorkspaceState
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: %s: %v", what, body, err)
		}
		want, err := c.Workspace(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != want.Branch || got.Base != want.Base || got.Added != want.Added || got.Removed != want.Removed ||
			got.Uncommitted != want.Uncommitted || got.Unpushed != want.Unpushed ||
			(got.Fetched == nil) != (want.Fetched == nil) || (got.Fetched != nil && !got.Fetched.Equal(*want.Fetched)) {
			t.Fatalf("%s: the route says %+v, Workspace %+v", what, got, want)
		}
		return got
	}

	// 1. Clean, then changed, then with an unpushed commit.
	if s := state("clean"); s.Branch == "" || s.Base == "" || s.Added != 0 || s.Removed != 0 || s.Uncommitted != 0 || s.Unpushed != 0 {
		t.Fatalf("a clean workspace: %+v", s)
	}
	os.WriteFile(filepath.Join(ws, "README"), []byte("changed\n"), 0o600)
	os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("new\n"), 0o600)
	if s := state("changed"); s.Added != 1 || s.Removed != 1 || s.Uncommitted != 2 || s.Unpushed != 0 {
		t.Fatalf("a changed workspace: %+v", s)
	}
	gitIn(t, ws, "add", "README", "notes.txt")
	gitIn(t, ws, "commit", "-q", "-m", "local work")
	if s := state("unpushed"); s.Unpushed != 1 || s.Uncommitted != 0 || s.Added != 2 || s.Removed != 1 {
		t.Fatalf("a workspace with an unpushed commit: %+v", s)
	}

	// 2. Signed out, an unknown Host, a foreign Origin.
	u, _ := url.Parse(base)
	if code, _ := rawGet(t, "GET", base, path(id), nil, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d, want 401", code)
	}
	if code, body := rawGet(t, "GET", base, path(id), cookie, "elsewhere.example:80", ""); code != http.StatusForbidden || strings.Contains(body, "branch") {
		t.Fatalf("an unknown Host: %d %s, want 403", code, body)
	}
	if code, body := rawGet(t, "GET", base, path(id), cookie, u.Host, "https://elsewhere.example"); code != http.StatusForbidden || strings.Contains(body, "branch") {
		t.Fatalf("a foreign Origin: %d %s, want 403", code, body)
	}

	// 5. Nothing but GET or HEAD.
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if code, body := rawGet(t, m, base, path(id), cookie, "", ""); code != http.StatusMethodNotAllowed || strings.Contains(body, "branch") {
			t.Fatalf("%s: %d %s, want 405", m, code, body)
		}
	}
	if code, _ := rawGet(t, "HEAD", base, path(id), cookie, "", ""); code != http.StatusOK {
		t.Fatalf("HEAD: %d", code)
	}

	// 4. A git failure is an error, never zeros.
	head := filepath.Join(ws, ".git", "HEAD")
	good, _ := os.ReadFile(head)
	if err := os.WriteFile(head, []byte("not a ref\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Workspace(ctx, id); err == nil {
		t.Fatal("Workspace read a broken repository: the check would test nothing")
	}
	code, body := rawGet(t, "GET", base, path(id), cookie, "", "")
	var zeros control.WorkspaceState
	if code < 500 || json.Unmarshal([]byte(body), &zeros) == nil {
		t.Fatalf("a broken workspace: %d %s, want an error and no state", code, body)
	}
	if strings.Contains(body, ws) || strings.Contains(body, filepath.Base(ws)) || strings.Contains(body, "fatal") || strings.Contains(body, "HEAD") {
		t.Fatalf("the error tells more than that the workspace cannot be read: %q", body)
	}
	if code, body := rawGet(t, "HEAD", base, path(id), cookie, "", ""); code < 500 || body != "" {
		t.Fatalf("HEAD of a broken workspace: %d %q, want an error and no body", code, body)
	}
	os.WriteFile(head, good, 0o600)
	state("repaired")

	// 3. A thread of another scope answers as one that does not exist.
	other, err := c.Start(ctx, control.StartRequest{Key: "k2", Scope: "x", Thread: "t2", Repo: repo, Input: control.Input{Text: "reply two"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the other thread to be idle", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, other, 0, 0)
		return err == nil && d.State == control.Idle
	})
	if _, err := c.Workspace(ctx, other); err != nil {
		t.Fatalf("the other thread's workspace cannot be read: the check would test nothing: %v", err)
	}
	outCode, outBody := dashGet(t, base, cookie, path(other))
	noneCode, noneBody := dashGet(t, base, cookie, path(control.ThreadID{Scope: "x", Key: "no-such-thread"}))
	inCode, inBody := dashGet(t, base, cookie, path(control.ThreadID{Scope: "s", Key: "no-such-thread"}))
	if outCode != http.StatusNotFound || outCode != noneCode || outBody != noneBody || inCode != noneCode || inBody != noneBody {
		t.Fatalf("another scope's thread: %d %q; no thread there: %d %q; no thread in the caller's scope: %d %q",
			outCode, outBody, noneCode, noneBody, inCode, inBody)
	}
	if code, _ := dashGet(t, base, cookie, "/api/workspace?scope=s"); code != http.StatusBadRequest {
		t.Fatalf("no key: %d, want 400", code)
	}
}
