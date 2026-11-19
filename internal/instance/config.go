// Package instance opens one hataori instance: its root directory, its
// config and its lock. An instance reads and writes only under its root.
package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/registry"
)

// ConfigFile is the config's path relative to the root.
const ConfigFile = "config/hataori.json"

// Config is an instance's config, parsed once at start-up.
type Config struct {
	Owner string // the name of the person who owns the instance
	// People resolves ids to people and says who may act in a scope
	// (design, section 5.5). Never nil.
	People *people.Directory
	// OldForm is set when the config has no "people": its owner and
	// posters were raw ids, converted into People.
	OldForm bool
	Trigger string        // the word that starts a thread
	Idle    time.Duration // idle period before a session is put to sleep
	Model   string        // default model of a scope without one; empty for the agent's own default
	Effort  string        // default effort of a scope without one; empty for the agent's own default
	// ContextBytes caps, in bytes, the thread's earlier inputs and final
	// answers given to a session that replaces one whose conversation the
	// agent no longer has (section 4.5). "context_bytes"; 8192 when absent.
	ContextBytes int
	Scopes       []Scope
	// LoginRelay is whether a login outage starts the agent's login
	// command for the owner (section 4.7); "login_relay": false switches it
	// off, and the alert stays.
	LoginRelay bool
	// HostTerminals is whether the owner may list and read the terminals on
	// the host that hataori does not own (section 7.2); "host_terminals",
	// off when absent.
	HostTerminals bool
	// TerminalCols and TerminalRows are the size of the terminal an agent
	// session runs in, for an agent that runs in one: "terminal_size". Both
	// are 0 when the key is absent, and the session host then makes the
	// terminal 80×24.
	TerminalCols, TerminalRows int

	// Dashboard is how the instance serves its dashboard (section 7.2);
	// nil when the config has no "dashboard" section.
	Dashboard *Dashboard

	// Cleanup is how workspaces are removed (section 4.6): the "cleanup"
	// section, with its defaults when absent.
	Cleanup Cleanup

	// Drivers and Adapters hold each driver's and adapter's own section of
	// the config, by registered name, unparsed.
	Drivers  map[string][]byte
	Adapters map[string][]byte

	// Plugins is the "plugins" list, each entry unparsed: internal/plugin
	// reads and checks it. Nil when the config has none.
	Plugins []json.RawMessage
}

// Scope is the config of one scope.
type Scope struct {
	Key     string   // opaque; for chat, a channel
	Client  string   // name of the client that owns the scope
	Enabled bool     // whether threads may be started in it
	Model   string   // default model; empty for the agent's own default
	Effort  string   // default effort; empty for the agent's own default
	People  []string // names of the people who may act here besides the owner
	// Supervisor is the scope's supervisor (design, section 7.1); nil when
	// the scope names none.
	Supervisor *Supervisor
}

// Supervisor is the "supervisor" section of a scope.
type Supervisor struct {
	Agent        string // "agent": the adapter that runs it; the instance's when empty
	Model        string // "model": the adapter's cheapest when empty
	Effort       string // "effort": the scope's default when empty
	Instructions string // "instructions": appended to the agent's own instructions
}

type fileSupervisor struct {
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Instructions string `json:"instructions"`
}

// Dashboard is the "dashboard" section of the config: a loopback listener,
// a web listener, or both (design, section 7.2). Whether the addresses fit
// together is checked when the dashboard starts.
type Dashboard struct {
	Local *DashboardLocal // the loopback listener; nil for none
	Web   *DashboardWeb   // the web listener; nil for none
	// OldForm is true for a section in the form before the web listener:
	// "url", "tls_cert", "tls_key" or "proxy" directly in it. With TLS or a
	// proxy it is the web listener, otherwise the loopback listener.
	OldForm bool
}

// DashboardLocal is the loopback listener: "dashboard.listen".
type DashboardLocal struct {
	Listen string // the address; loopback, and the default when empty
	URL    string // the old form's "url": where its links point; made from Listen when empty
}

// DashboardWeb is the web listener: "dashboard.web", or, in the old form,
// the keys directly in "dashboard".
type DashboardWeb struct {
	Listen  string // "listen": the address to listen on
	URL     string // "url": the address people open
	TLSCert string // "tls_cert": a PEM certificate file under config/, with "tls_key"
	TLSKey  string // "tls_key": its PEM private key file under config/
	Proxy   string // "proxy": the IP address of the reverse proxy in front
	Key     string // the prefix of its keys in messages: "dashboard.web." or "dashboard."
}

// Cleanup is the "cleanup" section of the config.
type Cleanup struct {
	// Disposable lists the patterns of ignored files that are not work;
	// "disposable". Any other ignored file stops a removal.
	Disposable []string
	// TrashRetention is how long a removed workspace stays in trash/;
	// "trash_retention", seven days when absent.
	TrashRetention time.Duration
	// OtherRoots are the roots of the other hataori instances on this host;
	// "other_roots". A remote under one does not count for a removal.
	OtherRoots []string
	// RemoveClosedAfter turns on the automatic removal pass: the workspace
	// of a thread closed for longer is removed, through the same proof as
	// hataori remove; "remove_closed_after". Zero, when absent: off.
	RemoveClosedAfter time.Duration
}

// DefaultTrashRetention is the trash retention of a config that names none.
const DefaultTrashRetention = 7 * 24 * time.Hour

type fileCleanup struct {
	Disposable     []string `json:"disposable"`
	TrashRetention string   `json:"trash_retention"`
	OtherRoots     []string `json:"other_roots"`
	RemoveClosed   string   `json:"remove_closed_after"`
}

type fileDashboard struct {
	Listen  string   `json:"listen"`
	URL     string   `json:"url"`
	TLSCert string   `json:"tls_cert"`
	TLSKey  string   `json:"tls_key"`
	Proxy   string   `json:"proxy"`
	Web     *fileWeb `json:"web"`
}

type fileWeb struct {
	Listen  string `json:"listen"`
	URL     string `json:"url"`
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	Proxy   string `json:"proxy"`
}

// The config as written in the file.
type fileConfig struct {
	Owner      string                     `json:"owner"`
	People     *[]filePerson              `json:"people"`
	Trigger    string                     `json:"trigger"`
	Idle       string                     `json:"idle"`
	Model      string                     `json:"model"`
	Effort     string                     `json:"effort"`
	Context    *int                       `json:"context_bytes"`
	LoginRelay *bool                      `json:"login_relay"`
	HostTerms  bool                       `json:"host_terminals"`
	TermSize   *fileTerminalSize          `json:"terminal_size"`
	Scopes     []fileScope                `json:"scopes"`
	Dashboard  *fileDashboard             `json:"dashboard"`
	Cleanup    *fileCleanup               `json:"cleanup"`
	Drivers    map[string]json.RawMessage `json:"drivers"`
	Adapters   map[string]json.RawMessage `json:"adapters"`
	Plugins    []json.RawMessage          `json:"plugins"`
}

type fileTerminalSize struct {
	Cols *int `json:"cols"`
	Rows *int `json:"rows"`
}

// The bounds of "terminal_size": smaller is too small for an agent's own
// screen, larger is no screen anyone reads.
const (
	MinTerminalCols, MaxTerminalCols = 40, 500
	MinTerminalRows, MaxTerminalRows = 10, 200
)

type fileScope struct {
	Key        string          `json:"key"`
	Client     string          `json:"client"`
	Enabled    bool            `json:"enabled"`
	Model      string          `json:"model"`
	Effort     string          `json:"effort"`
	People     []string        `json:"people"`
	Posters    []string        `json:"posters"`
	Supervisor *fileSupervisor `json:"supervisor"`
}

type filePerson struct {
	Name string   `json:"name"`
	IDs  []string `json:"ids"`
}

// ParseConfig parses a config. Every error is one line that names the key.
// Spaces around a string value are ignored.
func ParseConfig(data []byte) (*Config, error) {
	if err := rejectNull(data); err != nil {
		return nil, err
	}
	var f fileConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, decodeError(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("config: unexpected data after the top-level object")
	}

	c := &Config{
		LoginRelay:    f.LoginRelay == nil || *f.LoginRelay,
		HostTerminals: f.HostTerms,
		Owner:         strings.TrimSpace(f.Owner),
		Trigger:       strings.TrimSpace(f.Trigger),
		Model:         strings.TrimSpace(f.Model),
		Effort:        strings.TrimSpace(f.Effort),
		Drivers:       raw(f.Drivers),
		Adapters:      raw(f.Adapters),
		Plugins:       f.Plugins,
	}
	if c.Owner == "" {
		return nil, missing("owner")
	}
	if c.Trigger == "" {
		return nil, missing("trigger")
	}
	idle := strings.TrimSpace(f.Idle)
	if idle == "" {
		return nil, missing("idle")
	}
	d, err := time.ParseDuration(idle)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("config: key %q: %q is not a positive duration such as \"30m\"", "idle", idle)
	}
	c.Idle = d
	c.ContextBytes = 8192
	if f.Context != nil {
		if *f.Context <= 0 {
			return nil, fmt.Errorf("config: key %q: %d is not a positive number of bytes", "context_bytes", *f.Context)
		}
		c.ContextBytes = *f.Context
	}
	if s := f.TermSize; s != nil {
		for _, d := range []struct {
			key      string
			v        *int
			min, max int
			to       *int
		}{
			{"terminal_size.cols", s.Cols, MinTerminalCols, MaxTerminalCols, &c.TerminalCols},
			{"terminal_size.rows", s.Rows, MinTerminalRows, MaxTerminalRows, &c.TerminalRows},
		} {
			if d.v == nil {
				return nil, missing(d.key)
			}
			if *d.v < d.min || *d.v > d.max {
				return nil, fmt.Errorf("config: key %q: %d is outside %d to %d", d.key, *d.v, d.min, d.max)
			}
			*d.to = *d.v
		}
	}

	if f.Dashboard != nil {
		if c.Dashboard, err = dashboardSection(f.Dashboard); err != nil {
			return nil, err
		}
	}

	if c.Cleanup, err = cleanup(f.Cleanup); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for i, s := range f.Scopes {
		key := fmt.Sprintf("scopes[%d]", i)
		sc := Scope{
			Key:     strings.TrimSpace(s.Key),
			Client:  strings.TrimSpace(s.Client),
			Enabled: s.Enabled,
			Model:   strings.TrimSpace(s.Model),
			Effort:  strings.TrimSpace(s.Effort),
		}
		if sc.Key == "" {
			return nil, missing(key + ".key")
		}
		if seen[sc.Key] {
			return nil, fmt.Errorf("config: key %q: scope %q is listed twice", key+".key", sc.Key)
		}
		seen[sc.Key] = true
		if sc.Client == "" {
			return nil, missing(key + ".client")
		}
		if err := registry.ValidName(sc.Client); err != nil {
			return nil, fmt.Errorf("config: key %q: %v", key+".client", err)
		}
		if sv := s.Supervisor; sv != nil {
			sc.Supervisor = &Supervisor{Agent: strings.TrimSpace(sv.Agent), Model: strings.TrimSpace(sv.Model),
				Effort: strings.TrimSpace(sv.Effort), Instructions: strings.TrimSpace(sv.Instructions)}
			if a := sc.Supervisor.Agent; a != "" {
				if err := registry.ValidName(a); err != nil {
					return nil, fmt.Errorf("config: key %q: %v", key+".supervisor.agent", err)
				}
			}
		}
		c.Scopes = append(c.Scopes, sc)
	}
	for _, section := range []struct {
		key string
		m   map[string][]byte
	}{{"drivers", c.Drivers}, {"adapters", c.Adapters}} {
		for name := range section.m {
			if err := registry.ValidName(name); err != nil {
				return nil, fmt.Errorf("config: key %q: %v", section.key+"."+name, err)
			}
		}
	}
	if f.People == nil {
		err = c.oldForm(f.Scopes)
	} else {
		err = c.people(*f.People, f.Scopes)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// cleanup reads the "cleanup" section.
func cleanup(f *fileCleanup) (Cleanup, error) {
	c := Cleanup{TrashRetention: DefaultTrashRetention}
	if f == nil {
		return c, nil
	}
	for i, p := range f.Disposable {
		key := fmt.Sprintf("cleanup.disposable[%d]", i)
		if p = strings.TrimSpace(p); strings.Trim(p, "/") == "" {
			return c, fmt.Errorf("config: key %q: a pattern may not be empty", key)
		}
		if _, err := path.Match(p, ""); err != nil {
			return c, fmt.Errorf("config: key %q: %q is not a pattern: %v", key, p, err)
		}
		c.Disposable = append(c.Disposable, p)
	}
	if r := strings.TrimSpace(f.TrashRetention); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("config: key %q: %q is not a positive duration such as \"168h\"", "cleanup.trash_retention", r)
		}
		c.TrashRetention = d
	}
	for i, r := range f.OtherRoots {
		key := fmt.Sprintf("cleanup.other_roots[%d]", i)
		if r = strings.TrimSpace(r); !filepath.IsAbs(r) {
			return c, fmt.Errorf("config: key %q: %q is not an absolute path", key, r)
		}
		c.OtherRoots = append(c.OtherRoots, filepath.Clean(r))
	}
	if r := strings.TrimSpace(f.RemoveClosed); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("config: key %q: %q is not a positive duration such as \"720h\"", "cleanup.remove_closed_after", r)
		}
		c.RemoveClosedAfter = d
	}
	return c, nil
}

// people reads the people, the owner and each scope's people.
func (c *Config) people(list []filePerson, scopes []fileScope) error {
	var ps []people.Person
	names, owners := map[string]bool{}, map[string]string{} // id → name
	for i, fp := range list {
		key := fmt.Sprintf("people[%d]", i)
		p := people.Person{Name: strings.TrimSpace(fp.Name)}
		switch {
		case p.Name == "":
			return missing(key + ".name")
		case strings.Contains(p.Name, ":"):
			return fmt.Errorf("config: key %q: name %q has a colon; a name is a word, an id is <client>:<id>", key+".name", p.Name)
		case names[p.Name]:
			return fmt.Errorf("config: key %q: person %q is listed twice", key+".name", p.Name)
		}
		names[p.Name] = true
		for j, id := range fp.IDs {
			idKey := fmt.Sprintf("%s.ids[%d]", key, j)
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			client, _, ok := people.Split(id)
			if !ok {
				return fmt.Errorf("config: key %q: %q is not an id of the form <client>:<id>", idKey, id)
			}
			if err := registry.ValidName(client); err != nil {
				return fmt.Errorf("config: key %q: %v", idKey, err)
			}
			if other, dup := owners[id]; dup {
				return fmt.Errorf("config: key %q: id %q is listed already, under %q", idKey, id, other)
			}
			owners[id] = p.Name
			p.IDs = append(p.IDs, id)
		}
		ps = append(ps, p)
	}
	if c.Owner == "" {
		return missing("owner")
	}
	if !names[c.Owner] {
		return fmt.Errorf("config: key %q: %q is not the name of a person under \"people\"", "owner", c.Owner)
	}
	byScope := map[string][]string{}
	for i, s := range scopes {
		key := fmt.Sprintf("scopes[%d]", i)
		if len(s.Posters) > 0 {
			return fmt.Errorf("config: key %q: a config with \"people\" names who may post in %q, not their ids", key+".posters", key+".people")
		}
		for j, n := range s.People {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			if !names[n] {
				return fmt.Errorf("config: key %q: %q is not the name of a person under \"people\"", fmt.Sprintf("%s.people[%d]", key, j), n)
			}
			c.Scopes[i].People = append(c.Scopes[i].People, n)
		}
		byScope[c.Scopes[i].Key] = c.Scopes[i].People
	}
	c.People = people.New(ps, c.Owner, byScope)
	return nil
}

// OldOwner is the name the owner of an old-form config gets.
const OldOwner = "owner"

// oldForm converts a config without "people", whose owner and posters are
// raw ids, so that it allows exactly the authors it allowed before: the
// owner, whose id every driver compared, on every driver, and each poster
// on the scope's client, in that scope (design, section 5.5). A poster is
// named after its id, as "<client>/<id>": a client name has no "/", so no
// two ids get one name.
func (c *Config) oldForm(scopes []fileScope) error {
	c.OldForm = true
	raw := c.Owner
	if raw == "" {
		return missing("owner")
	}
	var drivers []string
	for name := range c.Drivers {
		drivers = append(drivers, name)
	}
	sort.Strings(drivers)
	owner := people.Person{Name: OldOwner}
	isOwner := map[string]bool{}
	for _, d := range drivers {
		id := people.Join(d, raw)
		owner.IDs = append(owner.IDs, id)
		isOwner[id] = true
	}
	ps := []people.Person{owner}
	listed := map[string]bool{}
	byScope := map[string][]string{}
	for i, s := range scopes {
		key := fmt.Sprintf("scopes[%d]", i)
		if len(s.People) > 0 {
			return fmt.Errorf("config: key %q: names people, but the config has no \"people\"", key+".people")
		}
		sc := &c.Scopes[i]
		for _, p := range s.Posters {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			id := people.Join(sc.Client, p)
			if isOwner[id] {
				continue // the owner may act everywhere already
			}
			name := sc.Client + "/" + p
			if !listed[id] {
				listed[id] = true
				ps = append(ps, people.Person{Name: name, IDs: []string{id}})
			}
			if !slices.Contains(sc.People, name) {
				sc.People = append(sc.People, name)
			}
		}
		byScope[sc.Key] = sc.People
	}
	c.Owner = OldOwner
	c.People = people.New(ps, c.Owner, byScope)
	return nil
}

// rejectNull refuses a null anywhere outside the opaque sections, since
// encoding/json would read it as the zero value instead of a wrong type.
// The "plugins" entries are read, nulls included, by internal/plugin.
func rejectNull(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return decodeError(err)
	}
	return walkNull("", v)
}

func walkNull(key string, v any) error {
	switch v := v.(type) {
	case nil:
		return fmt.Errorf("config: key %q must not be null", key)
	case map[string]any:
		for k, e := range v {
			if key == "" && (k == "drivers" || k == "adapters" || k == "plugins") {
				if e == nil {
					return fmt.Errorf("config: key %q must not be null", k)
				}
				continue
			}
			sub := k
			if key != "" {
				sub = key + "." + k
			}
			if err := walkNull(sub, e); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range v {
			if err := walkNull(fmt.Sprintf("%s[%d]", key, i), e); err != nil {
				return err
			}
		}
	}
	return nil
}

func missing(key string) error {
	return fmt.Errorf("config: missing required key %q", key)
}

func raw(m map[string]json.RawMessage) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

func decodeError(err error) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return fmt.Errorf("config: key %q must be %s, not %s", te.Field, kindName(te.Type.Kind().String()), te.Value)
	}
	if msg, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fmt.Errorf("config: unknown key %s", msg)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("config: syntax error at byte %d: %v", se.Offset, err)
	}
	return fmt.Errorf("config: %v", err)
}

func kindName(k string) string {
	switch k {
	case "slice":
		return "a list"
	case "map", "struct":
		return "an object"
	case "bool":
		return "true or false"
	}
	return "a " + k
}

// dashboardSection reads the "dashboard" section: "listen" is the loopback
// listener and "web" the web listener. A section with "url", "tls_cert",
// "tls_key" or "proxy" directly in it is in the old form, one listener:
// with TLS or a proxy the web listener, otherwise the loopback listener.
func dashboardSection(d *fileDashboard) (*Dashboard, error) {
	trim := strings.TrimSpace
	old := trim(d.URL) != "" || trim(d.TLSCert) != "" || trim(d.TLSKey) != "" || trim(d.Proxy) != ""
	if old && d.Web != nil {
		return nil, errors.New(`config: key "dashboard.web": not with "url", "tls_cert", "tls_key" or "proxy" directly in "dashboard", the one-listener form: move them into "dashboard.web"`)
	}
	out := &Dashboard{OldForm: old}
	web := func(w fileWeb, key string) (*DashboardWeb, error) {
		x := &DashboardWeb{Listen: trim(w.Listen), URL: trim(w.URL), TLSCert: trim(w.TLSCert), TLSKey: trim(w.TLSKey),
			Proxy: trim(w.Proxy), Key: key}
		if (x.TLSCert == "") != (x.TLSKey == "") {
			return nil, fmt.Errorf("config: keys %q and %q go together: give both or neither", key+"tls_cert", key+"tls_key")
		}
		return x, nil
	}
	var err error
	switch {
	case old && (trim(d.TLSCert) != "" || trim(d.TLSKey) != "" || trim(d.Proxy) != ""):
		out.Web, err = web(fileWeb{Listen: d.Listen, URL: d.URL, TLSCert: d.TLSCert, TLSKey: d.TLSKey, Proxy: d.Proxy}, "dashboard.")
	case old:
		out.Local = &DashboardLocal{Listen: trim(d.Listen), URL: trim(d.URL)}
	default:
		if d.Web == nil || trim(d.Listen) != "" {
			out.Local = &DashboardLocal{Listen: trim(d.Listen)}
		}
		if d.Web != nil {
			if out.Web, err = web(*d.Web, "dashboard.web."); err == nil && out.Web.Listen == "" {
				err = errors.New(`config: key "dashboard.web.listen": missing: name the address the web listener listens on`)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
