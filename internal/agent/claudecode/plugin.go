package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PluginName is the name of the plugin `hataori plugin` writes, in its
// marketplace.
const PluginName = "hataori"

// Plugin is the Claude Code plugin of one instance (design, section 4.8):
// a directory that is a plugin marketplace with this one plugin in it. A
// person installs it with the two Commands; hataori writes nothing into
// Claude Code's configuration.
type Plugin struct {
	Dir         string // the marketplace's directory, absolute
	Marketplace string // the marketplace's name
}

// Commands are what the person runs to install the plugin.
func (p Plugin) Commands() []string {
	return []string{
		"claude plugin marketplace add " + shellQuote(p.Dir),
		"claude plugin install " + PluginName + "@" + p.Marketplace,
	}
}

// The plugin's files, relative to Plugin.Dir.
var (
	MarketplaceFile = filepath.Join(".claude-plugin", "marketplace.json")
	PluginFile      = filepath.Join(PluginName, ".claude-plugin", "plugin.json")
	PluginHooksFile = filepath.Join(PluginName, "hooks", "hooks.json")
)

// ResolveHataori makes the hataori binary the hooks run an absolute path:
// a configured name without a slash is looked up with lookPath, as a shell
// would on the PATH. The plugin's hooks run in the person's sessions, whose
// PATH is theirs.
func (a *Adapter) ResolveHataori(lookPath func(string) (string, error)) error {
	p := a.hataori
	if !strings.Contains(p, "/") {
		found, err := lookPath(p)
		if err != nil {
			return fmt.Errorf("%s: hataori %q: %w", Name, p, err)
		}
		p = found
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	a.hataori = abs
	return nil
}

// PluginHookTail ends every command of the plugin: a hook that cannot run,
// because the hataori binary moved or was removed, still prints nothing
// and exits 0, since anything else puts an error notice in the person's
// window (design, Appendix B).
const PluginHookTail = " 2>/dev/null || :"

// PluginFiles returns the files of the plugin for the instance at root, by
// their path under Plugin.Dir. It registers the hook events of hataori's
// own settings file, each with `hataori hook --window` and the instance's
// root, by absolute path; the waiting ones also get `--wait` and the
// configured timeout. It registers nothing else: no status line, tool,
// skill or MCP server.
func (a *Adapter) PluginFiles(root string, p Plugin) (map[string][]byte, error) {
	if !filepath.IsAbs(a.hataori) {
		return nil, fmt.Errorf("%s: plugin: the hataori binary %q is not an absolute path (ResolveHataori)", Name, a.hataori)
	}
	hataori := a.hataori
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%s: plugin: %w", Name, err)
	}
	const about = "Lists the Claude Code sessions you start in your own terminal in hataori's dashboard."
	files := map[string]any{
		MarketplaceFile: map[string]any{
			"name":  p.Marketplace,
			"owner": map[string]string{"name": "hataori"},
			"plugins": []map[string]string{{
				"name": PluginName, "source": "./" + PluginName, "description": about}},
		},
		PluginFile: map[string]string{"name": PluginName, "description": about},
		PluginHooksFile: map[string]any{
			"hooks": a.hooks(shellQuote(hataori)+" hook --window --root "+shellQuote(root), PluginHookTail)},
	}
	out := map[string][]byte{}
	for name, v := range files {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		out[name] = append(b, '\n')
	}
	return out, nil
}

// WritePlugin writes the plugin of the instance at root into p.Dir, each
// file whole or not at all. Files of an earlier plugin there are replaced.
func (a *Adapter) WritePlugin(root string, p Plugin) error {
	files, err := a.PluginFiles(root, p)
	if err != nil {
		return err
	}
	for name, b := range files {
		path := filepath.Join(p.Dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("%s: plugin: %w", Name, err)
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return fmt.Errorf("%s: plugin: %w", Name, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("%s: plugin: %w", Name, err)
		}
	}
	return nil
}
