package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

const sum = "9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08"

var scopes = []string{"c1", "c2", "c3"}

// newPlugin writes manifest, unless it is empty, next to the path of a
// plugin's executable in a new directory, and returns that path. The
// executable itself is not created.
func newPlugin(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "plugin")
}

// entries is the config's "plugins" list, with each "PATH" replaced by
// path.
func entries(t *testing.T, list, path string) []json.RawMessage {
	t.Helper()
	p, _ := json.Marshal(path)
	var out []json.RawMessage
	if err := json.Unmarshal([]byte(strings.ReplaceAll(list, `"PATH"`, string(p))), &out); err != nil {
		t.Fatalf("entries %s: %v", list, err)
	}
	return out
}

const everyField = `{
	"id": "scanner",
	"version": "1.2.0",
	"protocol": "3.1",
	"operations": ["Threads", "Thread", "Events", "Send"],
	"events": ["turn_started", "text", "turn_ended"],
	"hooks": [
		{"point": "before_agent_run", "priority": 5, "timeout": "3s"},
		{"point": "message_sending", "priority": -1},
		{"point": "before_prompt_build"}
	],
	"conversation": true,
	"config": {"type": "object", "properties": {"words": {"type": "array"}}, "default": null}
}`

func TestGrant(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, config string
		want                   Grant
	}{
		{"every field", everyField, `[{
			"id": " scanner ", "path": "PATH", "sha256": "` + sum + `", "external": false,
			"scopes": ["c1", "c3"],
			"operations": ["Threads", "Events", "Send", "Thread"],
			"events": ["turn_ended", "turn_started", "text"],
			"hooks": [
				{"point": "message_sending", "timeout": "20s", "on_failure": "block"},
				{"point": "before_agent_run"},
				{"point": "before_prompt_build", "on_failure": "skip"}
			],
			"conversation": true,
			"settings": {"words": ["secret"], "level": null}
		}]`, Grant{
			ID: "scanner", Version: "1.2.0", Protocol: Protocol{3, 1},
			SHA256: strings.ToLower(sum), Scopes: []string{"c1", "c3"},
			Operations: []string{"Threads", "Events", "Send", "Thread"},
			Events:     []control.EventKind{"turn_ended", "turn_started", "text"},
			Hooks: []Hook{
				{Point: "message_sending", Priority: -1, Timeout: 20 * time.Second, OnFailure: Block},
				{Point: "before_agent_run", Priority: 5, Timeout: 3 * time.Second, OnFailure: Block},
				{Point: "before_prompt_build", Timeout: DefaultTimeout, OnFailure: Skip},
			},
			Conversation: true,
			Settings:     json.RawMessage(`{"words": ["secret"], "level": null}`),
			Schema:       json.RawMessage(`{"type": "object", "properties": {"words": {"type": "array"}}, "default": null}`),
		}},
		{"external", everyField, `[{"id": "scanner", "path": "PATH", "external": true, "token_file": "plugins/scanner-token", "scopes": ["c2"]}]`, Grant{
			ID: "scanner", Version: "1.2.0", Protocol: Protocol{3, 1},
			External: true, TokenFile: "plugins/scanner-token", Scopes: []string{"c2"},
			Operations: []string{"Threads", "Thread", "Events", "Send"},
			Events:     []control.EventKind{"turn_started", "text", "turn_ended"},
			Hooks: []Hook{
				{Point: "before_agent_run", Priority: 5, Timeout: 3 * time.Second, OnFailure: Block},
				{Point: "message_sending", Priority: -1, Timeout: DefaultTimeout, OnFailure: Skip},
				{Point: "before_prompt_build", Timeout: DefaultTimeout, OnFailure: Skip},
			},
			Settings: json.RawMessage(`{}`),
			Schema:   json.RawMessage(`{"type": "object", "properties": {"words": {"type": "array"}}, "default": null}`),
		}},
		{"narrowed", everyField, `[{"id": "scanner", "path": "PATH", "sha256": "` + sum + `", "scopes": ["c1"],
			"operations": ["Threads"], "events": [], "hooks": [{"point": "message_sending", "timeout": "1m"}], "conversation": true}]`, Grant{
			ID: "scanner", Version: "1.2.0", Protocol: Protocol{3, 1},
			SHA256: strings.ToLower(sum), Scopes: []string{"c1"},
			Operations:   []string{"Threads"},
			Hooks:        []Hook{{Point: "message_sending", Priority: -1, Timeout: time.Minute, OnFailure: Skip}},
			Conversation: true,
			Settings:     json.RawMessage(`{}`),
			Schema:       json.RawMessage(`{"type": "object", "properties": {"words": {"type": "array"}}, "default": null}`),
		}},
		{"conversation asked for and not granted", `{"id": "reader", "version": "0.1", "protocol": "3.0",
			"operations": ["Events"], "events": ["text"], "conversation": true}`,
			`[{"id": "reader", "path": "PATH", "sha256": "` + sum + `", "scopes": ["c1"]}]`, Grant{
				ID: "reader", Version: "0.1", Protocol: Protocol{3, 0},
				SHA256: strings.ToLower(sum), Scopes: []string{"c1"},
				Operations: []string{"Events"}, Events: []control.EventKind{"text"},
				Settings: json.RawMessage(`{}`),
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := newPlugin(t, tc.manifest)
			grants, err := Load(entries(t, tc.config, path), scopes)
			if err != nil {
				t.Fatal(err)
			}
			tc.want.Path = path
			if len(grants) != 1 || !reflect.DeepEqual(grants[0], tc.want) {
				t.Fatalf("grants:\n%+v\nwant:\n%+v", grants, tc.want)
			}
		})
	}
}

const manifest = `{"id": "scan", "version": "1.0.0", "protocol": "3.0", "operations": ["Threads", "Events"],
	"events": ["turn_started", "turn_ended"], "hooks": [{"point": "message_sending", "priority": 10}]}`

const entry = `{"id": "scan", "path": "PATH", "sha256": "` + sum + `", "scopes": ["c1"]`

// Each of these stops the daemon's start with one line that names the
// key: in the config, and in the manifest as well for a manifest's error.
func TestRefused(t *testing.T) {
	with := func(old, new string) string { return strings.Replace(manifest, old, new, 1) }
	for _, tc := range []struct {
		name, manifest, config string
		keys                   []string
	}{
		{"no manifest", "", `[` + entry + `}]`, []string{`"plugins[0].path"`, "no manifest"}},
		{"manifest not JSON", `{"id": "scan",`, `[` + entry + `}]`, []string{`"plugins[0].path"`, "not valid JSON"}},
		{"manifest not one object", manifest + ` {}`, `[` + entry + `}]`, []string{`"plugins[0].path"`, "after the top-level object"}},
		{"unknown field in the manifest", with(`"version"`, `"colour": "red", "version"`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `unknown key "colour"`}},
		{"unknown field in a manifest hook", with(`"priority": 10`, `"priority": 10, "when": "now"`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `unknown key "hooks[0].when"`}},
		{"unknown field in the config", manifest, `[` + entry + `, "colour": "red"}]`, []string{`unknown key "plugins[0].colour"`}},
		{"unknown field in a config hook", manifest, `[` + entry + `, "hooks": [{"point": "message_sending", "when": "now"}]}]`, []string{`unknown key "plugins[0].hooks[0].when"`}},
		{"unknown operation in the manifest", with(`"Events"]`, `"Events", "Fly"]`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `"operations[2]"`, `"Fly"`}},
		{"unknown operation in the config", manifest, `[` + entry + `, "operations": ["Fly"]}]`, []string{`"plugins[0].operations[0]"`, `"Fly" is not an operation`}},
		{"unknown event kind in the manifest", with(`"turn_ended"]`, `"turn_ended", "post_sent"]`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `"events[2]"`, `"post_sent"`}},
		{"unknown event kind in the config", manifest, `[` + entry + `, "events": ["thread_deleted"]}]`, []string{`"plugins[0].events[0]"`, "not an event kind"}},
		{"unknown hook point in the manifest", with(`"message_sending"`, `"inbound_claim"`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `"hooks[0].point"`}},
		{"unknown hook point in the config", manifest, `[` + entry + `, "hooks": [{"point": "agent_end"}]}]`, []string{`"plugins[0].hooks[0].point"`, "not a hook point"}},
		{"operation wider than the manifest", manifest, `[` + entry + `, "operations": ["Threads", "Send"]}]`, []string{`"plugins[0].operations[1]"`, `"Send"`}},
		{"event kind wider than the manifest", manifest, `[` + entry + `, "events": ["text"]}]`, []string{`"plugins[0].events[0]"`, `"text"`}},
		{"hook point wider than the manifest", manifest, `[` + entry + `, "hooks": [{"point": "before_agent_run"}]}]`, []string{`"plugins[0].hooks[0].point"`}},
		{"conversation wider than the manifest", manifest, `[` + entry + `, "conversation": true}]`, []string{`"plugins[0].conversation"`}},
		{"SignInLink", with(`"Threads",`, `"Threads", "SignInLink",`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `"operations[1]"`, "never"}},
		{"HostTerminals", with(`"Threads",`, `"HostTerminals", "Threads",`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `"operations[0]"`, "never"}},
		{"HostScreen", with(`"Threads",`, `"Threads", "HostScreen",`), `[` + entry + `}]`, []string{`"plugins[0].path"`, `"operations[1]"`, "never"}},
		{"HostScreen in the config", manifest, `[` + entry + `, "operations": ["HostScreen"]}]`, []string{`"plugins[0].operations[0]"`, "never"}},
		{"relative path", manifest, `[{"id": "scan", "path": "bin/scan", "sha256": "` + sum + `", "scopes": ["c1"]}]`, []string{`"plugins[0].path"`}},
		{"short sha256", manifest, `[{"id": "scan", "path": "PATH", "sha256": "` + sum[:63] + `", "scopes": ["c1"]}]`, []string{`"plugins[0].sha256"`}},
		{"sha256 not hexadecimal", manifest, `[{"id": "scan", "path": "PATH", "sha256": "` + sum[:63] + `g", "scopes": ["c1"]}]`, []string{`"plugins[0].sha256"`}},
		{"two plugins with one id", manifest, `[` + entry + `}, ` + entry + `}]`, []string{`"plugins[1].id"`}},
		{"scope not listed", manifest, `[{"id": "scan", "path": "PATH", "sha256": "` + sum + `", "scopes": ["c1", "c9"]}]`, []string{`"plugins[0].scopes[1]"`}},
		{"token file outside config/", manifest, `[{"id": "scan", "path": "PATH", "external": true, "token_file": "../token", "scopes": ["c1"]}]`, []string{`"plugins[0].token_file"`}},
		{"token file an absolute path", manifest, `[{"id": "scan", "path": "PATH", "external": true, "token_file": "/etc/token", "scopes": ["c1"]}]`, []string{`"plugins[0].token_file"`}},
		{"token file the config directory", manifest, `[{"id": "scan", "path": "PATH", "external": true, "token_file": "a/..", "scopes": ["c1"]}]`, []string{`"plugins[0].token_file"`}},
		{"settings not an object", manifest, `[` + entry + `, "settings": ["a"]}]`, []string{`"plugins[0].settings"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := newPlugin(t, tc.manifest)
			_, err := Load(entries(t, tc.config, path), scopes)
			if err == nil {
				t.Fatal("Load succeeded")
			}
			msg := err.Error()
			if strings.Contains(msg, "\n") {
				t.Fatalf("error %q is more than one line", msg)
			}
			for _, k := range tc.keys {
				if !strings.Contains(msg, k) {
					t.Fatalf("error %q does not name %s", msg, k)
				}
			}
		})
	}
}

// The config is read without running the plugin and without reading any
// file of it but the manifest: its executable can be neither run nor read.
func TestReadsOnlyTheManifest(t *testing.T) {
	path := newPlugin(t, manifest)
	marker := filepath.Join(filepath.Dir(path), "ran")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	grants, err := Load(entries(t, `[`+entry+`}]`, path), scopes)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Path != path {
		t.Fatalf("grants: %+v", grants)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the plugin ran")
	}
}

func TestNoPlugins(t *testing.T) {
	if grants, err := Load(nil, scopes); err != nil || grants != nil {
		t.Fatalf("Load(nil) = %v, %v", grants, err)
	}
}

// The operations a manifest may name are those of docs/protocol.md.
func TestOperationsAreTheProtocols(t *testing.T) {
	doc, err := os.ReadFile("../../docs/protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, m := range regexp.MustCompile("(?m)^### Operation `([A-Za-z]+)`$").FindAllStringSubmatch(string(doc), -1) {
		ops = append(ops, m[1])
	}
	slices.Sort(ops)
	if !slices.Equal(ops, operations) {
		t.Fatalf("docs/protocol.md has the operations %q, the package %q", ops, operations)
	}
}
