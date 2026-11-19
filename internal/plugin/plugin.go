// Package plugin reads what each plugin of an instance asks for, its
// manifest, and what the operator grants it, its entry in the config's
// "plugins" list, and makes of the two the plugin's effective grant
// (design, section 7.3). It runs nothing: of a plugin's files it reads
// only the manifest.
package plugin

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/registry"
)

// DefaultTimeout is how long a hook handler has when neither the manifest
// nor the config names a timeout.
const DefaultTimeout = 15 * time.Second

// Failure is what happens at a hook point when a handler fails or times
// out.
type Failure string

const (
	Block Failure = "block" // what passes the point is stopped (fail closed)
	Skip  Failure = "skip"  // the handler is skipped and the other results are kept
)

// points are the hook points, each with its failure policy when the config
// names none.
var points = map[string]Failure{
	"before_agent_run":    Block,
	"before_prompt_build": Skip,
	"message_sending":     Skip,
}

// operations are the operations of the control protocol, as
// docs/protocol.md lists them; a test keeps the two equal.
var operations = []string{"Answer", "AttachTerminal", "Close", "Events", "HostScreen", "HostTerminals", "Limits", "Link",
	"Remove", "Scopes", "Send", "SetName", "SignInLink", "Start", "Stop", "Terminal", "Thread", "ThreadEvents", "Threads", "Workspace"}

// never are the operations no plugin is given, whatever its manifest and
// its config say: the dashboard's sign-in and the terminals on the host.
var never = []string{"SignInLink", "HostTerminals", "HostScreen"}

// Grant is one plugin's effective grant: what its manifest asks for and
// its config entry grants, and nothing more.
type Grant struct {
	ID       string   // the plugin's name, as the config and the manifest both give it
	Version  string   // the manifest's
	Protocol Protocol // the manifest's
	// Path is the plugin's executable, absolute and clean. Its manifest is
	// next to it. Nothing here reads, hashes or runs it. For an External
	// plugin only its directory, the manifest's, is used.
	Path string
	// SHA256 is the executable's hash as the config gives it, 64 lowercase
	// hexadecimal digits; empty for an External plugin.
	SHA256 string
	// External is a program hataori does not start. It connects by itself
	// with the token in TokenFile, a path relative to <root>/config/ that
	// stays inside it.
	External  bool
	TokenFile string
	Scopes    []string // the scopes it may see and act in
	// Operations are the control operations it may call, Events the event
	// kinds it may read, and Hooks the hook points it handles.
	Operations []string
	Events     []control.EventKind
	Hooks      []Hook
	// Conversation is whether it may read message text: the manifest asks
	// for it and the config grants it.
	Conversation bool
	// Settings is the config's "settings", a JSON object that hataori
	// passes on and does not read; "{}" when the config has none.
	Settings json.RawMessage
	// Schema is the manifest's "config", a JSON Schema for Settings, not
	// interpreted; nil when the manifest has none.
	Schema json.RawMessage
}

// Hook is one hook point a plugin handles, as granted.
type Hook struct {
	Point     string
	Priority  int           // the manifest's; higher runs first
	Timeout   time.Duration // the config's, or else the manifest's, or else DefaultTimeout
	OnFailure Failure       // the config's, or else the point's own
}

const prefix = "config: "

var (
	entryKeys = []string{"id", "path", "sha256", "external", "token_file", "scopes",
		"operations", "events", "hooks", "conversation", "settings"}
	entryHookKeys = []string{"point", "timeout", "on_failure"}
)

// Load makes the effective grant of each entry of the config's "plugins"
// list, in the config's order. scopes are the keys of the config's
// "scopes". It reads each plugin's manifest and nothing else, and runs
// nothing. An error is one line that names the key, and refuses the whole
// list.
func Load(entries []json.RawMessage, scopes []string) ([]Grant, error) {
	var grants []Grant
	ids := map[string]bool{}
	for i, raw := range entries {
		g, err := load(fmt.Sprintf("plugins[%d]", i), raw, scopes, ids)
		if err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, nil
}

func load(key string, raw json.RawMessage, scopes []string, ids map[string]bool) (Grant, error) {
	o, err := readObject(prefix, key, raw, entryKeys)
	if err != nil {
		return Grant{}, err
	}
	var g Grant
	if g.ID, err = o.required("id"); err != nil {
		return g, err
	}
	if err := registry.ValidName(g.ID); err != nil {
		return g, o.fail("id", "%s", strings.TrimPrefix(err.Error(), "registry: "))
	}
	if ids[g.ID] {
		return g, o.fail("id", "plugin %q is listed twice", g.ID)
	}
	ids[g.ID] = true
	path, err := o.required("path")
	if err != nil {
		return g, err
	}
	if !filepath.IsAbs(path) {
		return g, o.fail("path", "%q is not an absolute path", path)
	}
	g.Path = filepath.Clean(path)
	if err := g.start(o); err != nil {
		return g, err
	}
	if g.Scopes, err = grantScopes(o, scopes); err != nil {
		return g, err
	}

	m, err := ReadManifest(filepath.Join(filepath.Dir(g.Path), ManifestFile))
	if err != nil {
		return g, fmt.Errorf("%skey %q: %v", prefix, o.at("path"), err)
	}
	if m.ID != g.ID {
		return g, o.fail("id", "%q, but the plugin's manifest says %q", g.ID, m.ID)
	}
	g.Version, g.Protocol, g.Schema = m.Version, m.Protocol, m.Config

	if g.Operations, err = narrow(o, "operations", m.Operations, checkOperation); err != nil {
		return g, err
	}
	events, err := narrow(o, "events", m.Events, checkEvent)
	if err != nil {
		return g, err
	}
	for _, k := range events {
		g.Events = append(g.Events, control.EventKind(k))
	}
	if g.Hooks, err = grantHooks(o, m.Hooks); err != nil {
		return g, err
	}
	conversation, err := o.boolean("conversation")
	if err != nil {
		return g, err
	}
	if conversation && !m.Conversation {
		return g, o.fail("conversation", "the manifest does not ask for \"conversation\": the config can only narrow what the manifest asks for")
	}
	g.Conversation = conversation

	g.Settings = json.RawMessage("{}")
	if s, ok, err := o.value("settings"); err != nil {
		return g, err
	} else if ok {
		if k := kind(s); k != "object" {
			return g, o.fail("settings", "must be an object, not %s", k)
		}
		g.Settings = s
	}
	return g, nil
}

// start reads how the plugin is started: by hataori, when the executable
// has the config's hash, or by itself, with a token from a file.
func (g *Grant) start(o object) error {
	var err error
	if g.External, err = o.boolean("external"); err != nil {
		return err
	}
	sum, hasSum, err := o.str("sha256")
	if err != nil {
		return err
	}
	token, hasToken, err := o.str("token_file")
	if err != nil {
		return err
	}
	if g.External {
		if hasSum {
			return o.fail("sha256", "hataori does not start an external plugin, so it checks no hash")
		}
		if !hasToken || token == "" {
			return o.missing("token_file")
		}
		if filepath.IsAbs(token) || !filepath.IsLocal(token) || filepath.Clean(token) == "." {
			return o.fail("token_file", "%q is not a file name inside the config directory", token)
		}
		g.TokenFile = filepath.Clean(token)
		return nil
	}
	if hasToken {
		return o.fail("token_file", "only an external plugin has a token file; a plugin hataori starts gets a token at each start")
	}
	if !hasSum {
		return o.missing("sha256")
	}
	if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
		return o.fail("sha256", "%q is not a SHA-256: 64 hexadecimal digits", sum)
	}
	g.SHA256 = strings.ToLower(sum)
	return nil
}

func grantScopes(o object, scopes []string) ([]string, error) {
	list, ok, err := o.names("scopes", func(s string) string {
		if !slices.Contains(scopes, s) {
			return fmt.Sprintf("%q is not a scope under \"scopes\"", s)
		}
		return ""
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, o.missing("scopes")
	}
	if len(list) == 0 {
		return nil, o.fail("scopes", "names no scope; a plugin sees and acts only in the scopes listed here")
	}
	return list, nil
}

// narrow is what the config's list grants of what the manifest asks for:
// all of it when the config has no such list.
func narrow(o object, name string, asked []string, check func(string) string) ([]string, error) {
	list, ok, err := o.names(name, check)
	if err != nil {
		return nil, err
	}
	if !ok {
		return slices.Clone(asked), nil
	}
	for i, s := range list {
		if !slices.Contains(asked, s) {
			return nil, o.fail(fmt.Sprintf("%s[%d]", name, i),
				"%q is not in the manifest's %q: the config can only narrow what the manifest asks for", s, name)
		}
	}
	return list, nil
}

// grantHooks is what the config's "hooks" grants of the manifest's: all
// of them, with their defaults, when the config has no "hooks".
func grantHooks(o object, asked []ManifestHook) ([]Hook, error) {
	grant := func(h ManifestHook) Hook {
		g := Hook{Point: h.Point, Priority: h.Priority, Timeout: h.Timeout, OnFailure: points[h.Point]}
		if g.Timeout == 0 {
			g.Timeout = DefaultTimeout
		}
		return g
	}
	list, ok, err := o.list("hooks")
	if err != nil {
		return nil, err
	}
	var hooks []Hook
	if !ok {
		for _, h := range asked {
			hooks = append(hooks, grant(h))
		}
		return hooks, nil
	}
	for i, raw := range list {
		ho, err := readObject(prefix, o.at(fmt.Sprintf("hooks[%d]", i)), raw, entryHookKeys)
		if err != nil {
			return nil, err
		}
		point, err := ho.required("point")
		if err != nil {
			return nil, err
		}
		if why := checkPoint(point); why != "" {
			return nil, ho.fail("point", "%s", why)
		}
		j := slices.IndexFunc(asked, func(h ManifestHook) bool { return h.Point == point })
		if j < 0 {
			return nil, ho.fail("point", "%q is not in the manifest's \"hooks\": the config can only narrow what the manifest asks for", point)
		}
		if slices.ContainsFunc(hooks, func(h Hook) bool { return h.Point == point }) {
			return nil, ho.fail("point", "%q is listed twice", point)
		}
		h := grant(asked[j])
		if d, ok, err := ho.duration("timeout"); err != nil {
			return nil, err
		} else if ok {
			h.Timeout = d
		}
		if f, ok, err := ho.str("on_failure"); err != nil {
			return nil, err
		} else if ok {
			if Failure(f) != Block && Failure(f) != Skip {
				return nil, ho.fail("on_failure", "%q is neither \"block\" nor \"skip\"", f)
			}
			h.OnFailure = Failure(f)
		}
		hooks = append(hooks, h)
	}
	return hooks, nil
}

// checkOperation, checkEvent and checkPoint say why a name is not one a
// plugin may ask for, or nothing.
func checkOperation(op string) string {
	if slices.Contains(never, op) {
		return fmt.Sprintf("%q is never given to a plugin", op)
	}
	if !slices.Contains(operations, op) {
		return fmt.Sprintf("%q is not an operation of the control protocol", op)
	}
	return ""
}

func checkEvent(k string) string {
	if _, ok := control.EventKinds[control.EventKind(k)]; !ok {
		return fmt.Sprintf("%q is not an event kind of the control protocol", k)
	}
	return ""
}

func checkPoint(p string) string {
	if _, ok := points[p]; !ok {
		return fmt.Sprintf("%q is not a hook point: before_agent_run, before_prompt_build or message_sending", p)
	}
	return ""
}
