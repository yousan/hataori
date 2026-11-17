package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/discord"
)

// A config that names the Discord driver gets a Discord front end, which
// owns the scopes whose client is discord. Nothing here connects: the
// driver's Run is never called.
func TestDiscordDriverInTheBinary(t *testing.T) {
	if _, ok := surface.Lookup(discord.Name); !ok {
		t.Fatalf("no surface driver %q registered", discord.Name)
	}

	root := t.TempDir()
	cfg := map[string]any{
		"owner": "owner", "trigger": "!go", "idle": "1h",
		"scopes": []map[string]any{
			{"key": "1000000000000000002", "client": discord.Name, "enabled": true},
			{"key": "other", "client": "chat", "enabled": true},
		},
		"drivers":  map[string]any{discord.Name: discord.Settings{BotID: "1000000000000000001", TokenFile: "discord-token"}},
		"adapters": map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"}}},
	}
	b, _ := json.Marshal(cfg)
	os.MkdirAll(filepath.Join(root, "config"), 0o700)
	if err := os.WriteFile(filepath.Join(root, instance.ConfigFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "discord-token"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := instance.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	confDir, err := in.Root.OpenRoot("config")
	if err != nil {
		t.Fatal(err)
	}
	defer confDir.Close()

	c := &core.Core{Owners: map[string]control.ScopeOwner{}}
	fronts, err := frontEnds(root, confDir, in.Config, nil, c, &relay.Relay{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("front ends: %v", err)
	}
	if len(fronts) != 1 || fronts[0].Name != discord.Name {
		t.Fatalf("front ends %v, want one named %q", fronts, discord.Name)
	}
	fe := fronts[0]
	if _, ok := fe.Driver.(*discord.Driver); !ok {
		t.Fatalf("driver %T, want *discord.Driver", fe.Driver)
	}
	if _, ok := fe.Scopes["1000000000000000002"]; !ok || len(fe.Scopes) != 1 {
		t.Fatalf("scopes %v, want only the discord one", fe.Scopes)
	}
	if c.Owners[discord.Name] != fe {
		t.Fatal("the front end does not own the discord scopes")
	}
}
