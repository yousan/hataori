package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/wire"
)

// A plugin's connection, bound by a token to the caller its grant makes,
// gets what the grant names and nothing more: Threads and Events answer,
// with the text left out and only the granted event kinds; Thread and
// Terminal, which the manifest asks for and the config does not grant, are
// refused and logged with the operation and the scope.
func TestPluginConnectionIsItsGrant(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "scan")
	manifest := `{"id": "scan", "version": "0.1.0", "protocol": "3.0",
		"operations": ["Threads", "Thread", "Events", "Terminal"], "events": ["turn_started", "turn_ended", "text", "input"]}`
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["plugins"] = []map[string]any{{"id": "scan", "path": exe, "sha256": strings.Repeat("ab", 32),
			"scopes": []string{"c1"}, "operations": []string{"Threads", "Events"}, "events": []string{"turn_started", "turn_ended"}}}
	})
	var issue func(string) (string, error)
	pluginHook = func(i func(string) (string, error), _ func(string)) { issue = i }
	t.Cleanup(func() { pluginHook = nil })
	logged := startDaemonLog(t, r.root)
	r.connected(t)

	ctx := context.Background()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	const marker = "MARKER-p348"
	id, err := owner.Start(ctx, control.StartRequest{Key: "k-a", Scope: "c1", Thread: "ta", Repo: bareRepo(t), Input: control.Input{Text: "reply " + marker}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn ended", 20*time.Second, func() bool { return len(threadEvents(t, owner, id, "turn_ended")) > 0 })

	if _, err := issue("other"); err == nil {
		t.Fatal("a token for a plugin the config does not list")
	}
	token, err := issue("scan")
	if err != nil {
		t.Fatal(err)
	}
	p, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ts, err := p.Threads(ctx, control.Filter{})
	if err != nil || len(ts) != 1 || ts[0].ID != id {
		t.Fatalf("Threads: %+v, %v", ts, err)
	}
	ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ch, err := p.Events(ectx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []control.EventKind
	var all []control.Event
	for e := range ch {
		if e.Kind == control.CaughtUp {
			break
		}
		kinds = append(kinds, e.Kind)
		all = append(all, e)
	}
	cancel()
	if !slices.Contains(kinds, "turn_started") || !slices.Contains(kinds, "turn_ended") {
		t.Fatalf("the plugin read the kinds %v", kinds)
	}
	for _, k := range kinds {
		if k != "turn_started" && k != "turn_ended" {
			t.Fatalf("the plugin read an event of kind %s, not granted", k)
		}
	}
	if b, _ := json.Marshal([]any{ts, all}); strings.Contains(string(b), marker) {
		t.Fatalf("a plugin without \"conversation\" read the marker: %s", b)
	}

	if _, err := p.Thread(ctx, id, 100, 10); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Thread, not granted: %v", err)
	}
	if _, err := p.Terminal(ctx, id, "", 0); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Terminal, not granted: %v", err)
	}
	if err := p.Send(ctx, id, "s1", control.Input{Text: "x"}, control.Refuse); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Send, not asked for: %v", err)
	}
	eventually(t, "the refusals logged", 5*time.Second, func() bool {
		l := logged()
		return refusedLine(l, "Thread", "c1") && refusedLine(l, "Terminal", "c1") && refusedLine(l, "Send", "c1") &&
			strings.Contains(l, "from=plugin:scan")
	})
}
