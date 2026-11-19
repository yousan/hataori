package claudecode

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func pluginAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := New([]byte(`{"hataori":"/opt/hataori/bin/hataori","hook_timeout":"90s"}`))
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Adapter)
}

// The plugin registers the hook events of hataori's own settings file, with
// the same matchers, the same waiting hooks and the same timeout, and
// nothing else: each command is `hataori hook --window` with the root.
func TestPluginHooksAreTheSettingsHooks(t *testing.T) {
	a := pluginAdapter(t)
	root := t.TempDir()
	dir := filepath.Join(root, "claude-plugin")
	p := Plugin{Dir: dir, Marketplace: "hataori-test"}
	if err := a.WritePlugin(root, p); err != nil {
		t.Fatal(err)
	}

	b, err := a.settings(agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var settings struct{ Hooks map[string][]hookMatcher }
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	var plugin map[string]json.RawMessage
	if err := json.Unmarshal(readFile(t, dir, PluginHooksFile), &plugin); err != nil {
		t.Fatal(err)
	}
	if len(plugin) != 1 || plugin["hooks"] == nil {
		t.Fatalf("hooks.json has keys other than hooks: %s", readFile(t, dir, PluginHooksFile))
	}
	var hooks map[string][]hookMatcher
	if err := json.Unmarshal(plugin["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}

	own := shellQuote("/opt/hataori/bin/hataori") + " hook"
	window := own + " --window --root " + shellQuote(root)
	// The plugin's hooks with its commands written as the settings' are
	// the settings' hooks, event for event and matcher for matcher.
	for _, ms := range hooks {
		for i := range ms {
			for j, h := range ms[i].Hooks {
				if !strings.HasPrefix(h.Command, window) || !strings.HasSuffix(h.Command, PluginHookTail) {
					t.Fatalf("plugin hook command %q, want it to start with %q and end with %q", h.Command, window, PluginHookTail)
				}
				ms[i].Hooks[j].Command = own + strings.TrimSuffix(strings.TrimPrefix(h.Command, window), PluginHookTail)
			}
		}
	}
	if !reflect.DeepEqual(hooks, settings.Hooks) {
		t.Fatalf("plugin hooks:\n%+v\nsettings hooks:\n%+v", hooks, settings.Hooks)
	}
	if got := slices.Sorted(maps.Keys(hooks)); !slices.Equal(got, slices.Sorted(slices.Values(hookEvents))) {
		t.Fatalf("plugin events %v, the codec reads %v", got, hookEvents)
	}
	// The two that decide something wait, with the configured timeout.
	waits := 0
	for ev, ms := range hooks {
		for _, m := range ms {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, "--wait") {
					waits++
					if !(ev == "PermissionRequest" && m.Matcher == "" || ev == "PreToolUse" && m.Matcher == "AskUserQuestion") ||
						h.Timeout != 90 || !strings.HasSuffix(h.Command, " --wait --timeout 90s") {
						t.Fatalf("waiting hook %s %q: %+v", ev, m.Matcher, h)
					}
				}
			}
		}
	}
	if waits != 2 {
		t.Fatalf("%d waiting hooks, want 2", waits)
	}
}

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The plugin is the three files of a marketplace with one plugin: no status
// line, tool, skill or MCP server, and no option.
func TestPluginFilesAreOnlyHooks(t *testing.T) {
	a := pluginAdapter(t)
	files, err := a.PluginFiles(t.TempDir(), Plugin{Dir: "/x", Marketplace: "hataori-test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, slices.Sorted(slices.Values([]string{MarketplaceFile, PluginFile, PluginHooksFile}))) {
		t.Fatalf("files %v", got)
	}
	var market struct {
		Name    string
		Plugins []map[string]string
	}
	if err := json.Unmarshal(files[MarketplaceFile], &market); err != nil {
		t.Fatal(err)
	}
	if market.Name != "hataori-test" || len(market.Plugins) != 1 || market.Plugins[0]["name"] != PluginName ||
		market.Plugins[0]["source"] != "./"+PluginName {
		t.Fatalf("marketplace: %s", files[MarketplaceFile])
	}
	var plugin map[string]any
	if err := json.Unmarshal(files[PluginFile], &plugin); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(slices.Sorted(maps.Keys(plugin)), []string{"description", "name"}) || plugin["name"] != PluginName {
		t.Fatalf("plugin.json: %s", files[PluginFile])
	}
	if got := (Plugin{Dir: "/srv/my root/claude-plugin", Marketplace: "hataori-test"}).Commands(); !slices.Equal(got, []string{
		"claude plugin marketplace add '/srv/my root/claude-plugin'", "claude plugin install hataori@hataori-test"}) {
		t.Fatalf("commands %q", got)
	}
}

// A hataori binary named without a path is the one on the PATH, absolute.
func TestPluginHataoriIsAbsolute(t *testing.T) {
	a, err := New([]byte(`{"hataori":"hataori"}`))
	if err != nil {
		t.Fatal(err)
	}
	cc := a.(*Adapter)
	if _, err := cc.PluginFiles(t.TempDir(), Plugin{Dir: "/x", Marketplace: "m"}); err == nil {
		t.Fatal("a plugin with a relative hataori was written")
	}
	if err := cc.ResolveHataori(func(string) (string, error) { return "/usr/local/bin/hataori", nil }); err != nil {
		t.Fatal(err)
	}
	files, err := cc.PluginFiles(t.TempDir(), Plugin{Dir: "/x", Marketplace: "m"})
	if err != nil || !strings.Contains(string(files[PluginHooksFile]), `"'/usr/local/bin/hataori' hook --window --root '/`) {
		t.Fatalf("hooks: %s, %v", files[PluginHooksFile], err)
	}
}

// A plugin hook whose hataori binary is gone prints nothing and exits 0:
// any other status shows the person an error in their window.
func TestPluginHookWithoutTheBinary(t *testing.T) {
	a, err := New([]byte(`{"hataori":"/nonexistent/hataori/bin/hataori"}`))
	if err != nil {
		t.Fatal(err)
	}
	files, err := a.(*Adapter).PluginFiles(t.TempDir(), Plugin{Dir: "/x", Marketplace: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var plugin struct{ Hooks map[string][]hookMatcher }
	if err := json.Unmarshal(files[PluginHooksFile], &plugin); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"SessionStart", "PermissionRequest"} {
		cmd := exec.Command("sh", "-c", plugin.Hooks[ev][0].Hooks[0].Command)
		cmd.Stdin = strings.NewReader(`{"session_id":"s"}`)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil || out.Len() != 0 || errOut.Len() != 0 {
			t.Fatalf("%s: %v, printed %q and %q", ev, err, out.String(), errOut.String())
		}
	}
}
