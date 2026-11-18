package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/instance"
)

// withDashboard adds a "dashboard" section to root's config.
func withDashboard(t *testing.T, root string, section map[string]any) {
	t.Helper()
	p := filepath.Join(root, instance.ConfigFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["dashboard"] = section
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// `hataori signin` prints a link that signs in once.
func TestSignInThroughTheDaemon(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)

	code, out, errOut := cli(t, "signin", "--root", root)
	ls := lines(t, out)
	if code != 0 || len(ls) != 1 {
		t.Fatalf("signin: exit %d, %q, %q", code, out, errOut)
	}
	link, _ := ls[0]["link"].(string)
	if !strings.HasPrefix(link, "http://127.0.0.1:") {
		t.Fatalf("link %q", link)
	}
	base := link[:strings.Index(link, "/signin")]
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(url string, cookies ...*http.Cookie) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		resp, err := hc.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get(base + "/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("before sign-in: %s", resp.Status)
	}
	resp := get(link)
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 || !resp.Cookies()[0].HttpOnly {
		t.Fatalf("sign-in: %s, cookies %v", resp.Status, resp.Header["Set-Cookie"])
	}
	cookie := resp.Cookies()[0]
	if resp := get(base+"/", cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %s", resp.Status)
	}
	if resp := get(link); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the link a second time: %s", resp.Status)
	}
}

// Without a dashboard, signin says so; with an address the dashboard
// refuses, the daemon does not start and says why.
func TestSignInWithoutADashboard(t *testing.T) {
	root := newRoot(t)
	startDaemon(t, root)
	if code, _, errOut := cli(t, "signin", "--root", root); code != exitError || !strings.Contains(errOut, "no dashboard") {
		t.Fatalf("signin: exit %d, %q", code, errOut)
	}
}

func TestDaemonRefusesAnOpenDashboard(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "0.0.0.0:0"})
	code, _, errOut := cli(t, "run", "--root", root)
	var reason string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.HasPrefix(l, "hataori: ") {
			reason = l
		}
	}
	if code != exitError || !strings.Contains(reason, `"dashboard.listen"`) || !strings.Contains(reason, "not a loopback address") {
		t.Fatalf("run: exit %d, %q", code, errOut)
	}
}
