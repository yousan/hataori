package main

import (
	"context"
	"net/url"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// TestDashboardLanguage checks browser preferences, saved choices, and unknown URL values.
func TestDashboardLanguage(t *testing.T) {
	root := terminalRoot(t)
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	_, err = c.Start(ctx, control.StartRequest{Key: "language", Scope: "s", Thread: "language", Repo: bareRepo(t), Input: control.Input{Text: "reply done"}})
	if err != nil {
		t.Fatal(err)
	}
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	base := u.Scheme + "://" + u.Host
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__errors = []; window.addEventListener('error', e => window.__errors.push(e.message));`})
	language := func(value string) {
		b.call("Emulation.setUserAgentOverride", map[string]any{"userAgent": "Mozilla/5.0", "acceptLanguage": value})
	}
	check := func(want string) {
		t.Helper()
		expr := `document.documentElement.lang === ` + js(want) + ` && !!document.querySelector('.tab[data-tab=all] .tab-title')`
		b.until("dashboard language "+want, expr)
		var text string
		b.eval(`document.querySelector('.tab[data-tab=all] .tab-title').textContent`, &text)
		if want == "en" && text != "All threads" {
			t.Fatalf("English tab: %q", text)
		}
		if want == "ja" {
			var japanese bool
			b.eval(`/[\u3040-\u30ff\u3400-\u9fff]/u.test(document.querySelector('.tab[data-tab=all] .tab-title').textContent)`, &japanese)
			if !japanese {
				t.Fatalf("Japanese tab: %q", text)
			}
		}
	}
	navigate := func(path string) {
		t.Helper()
		b.eval(`window.__languageTestPrevious = true`, nil)
		b.call("Page.navigate", map[string]any{"url": base + path})
		b.until("new document", `!window.__languageTestPrevious && document.readyState === 'complete'`)
	}
	language("ja-JP,ja")
	b.call("Page.navigate", map[string]any{"url": link})
	check("ja")
	b.eval(`localStorage.removeItem('hataori.lang')`, nil)
	language("en-US,en")
	navigate("/")
	check("en")
	navigate("/?lang=ja")
	check("ja")
	navigate("/")
	check("ja")
	language("ja-JP,ja")
	navigate("/?lang=en")
	check("en")
	navigate("/")
	check("en")
	navigate("/?lang=xx")
	check("en")
	var saved string
	b.eval(`localStorage.getItem('hataori.lang')`, &saved)
	if saved != "en" {
		t.Fatalf("stored language after unknown choice: %q", saved)
	}
	var errors []string
	b.eval(`window.__errors`, &errors)
	if len(errors) != 0 {
		t.Fatalf("page errors: %v", errors)
	}
}
