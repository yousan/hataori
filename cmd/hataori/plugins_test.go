package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/store"
)

// A plugin the config names but whose manifest cannot be read stops the
// daemon's start with one line that names the key, before the database
// is opened.
func TestRunRefusesAPluginWithoutAManifest(t *testing.T) {
	root := newRoot(t)
	file := filepath.Join(root, instance.ConfigFile)
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["plugins"] = []map[string]any{{"id": "scan", "path": filepath.Join(t.TempDir(), "scan"),
		"sha256": strings.Repeat("0", 64), "scopes": []string{"s"}}}
	if b, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// A daemon that started would run until the deadline, exit 0 and
	// leave its database behind.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	code := runCtx(ctx, []string{"run", "--root", root}, &out, &errOut)
	killSessions(root)
	if code == 0 || out.Len() != 0 || strings.Count(strings.TrimSpace(errOut.String()), "\n") != 0 ||
		!strings.Contains(errOut.String(), `"plugins[0].path"`) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, store.File)); err == nil {
		t.Fatal("the daemon opened its database")
	}
}
