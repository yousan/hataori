// Package instance opens one hataori instance: its root directory, its
// config and its lock. An instance reads and writes only under its root.
package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// Agent is "agent": the adapter new threads run unless their scope or
	// order names another. The one adapter when the key is absent and only
	// one is configured.
	Agent   string
	Trigger string        // the word that starts a thread
	Idle    time.Duration // idle period before a session is put to sleep
	Model   string        // default model of a scope without one; empty for the agent's own default
	Effort  string        // default effort of a scope without one; empty for the agent's own default
	// ContextBytes caps, in bytes, the thread's earlier inputs and final
	// answers given to a session that replaces one whose conversation the
	// agent no longer has (section 4.5). "context_bytes"; 8192 when absent.
	ContextBytes int
	Scopes       []Scope
	// Machines includes the implicit local machine. These are configuration
	// and identity records; worker execution and placement are separate.
	Machines []Machine
	// LoginRelay is whether a login outage starts the agent's login
	// command for the owner (section 4.7); "login_relay": false switches it
	// off, and the alert stays.
	LoginRelay bool
	// HostTerminals is whether the owner may list and read the terminals on
	// the host that hataori does not own (section 7.2); "host_terminals",
	// off when absent.
	HostTerminals bool
	// HostInput is whether the owner may type into tmux panes on the host
	// that hataori does not own (section 7.2); "host_input", off when
	// absent. It needs HostTerminals.
	HostInput bool
	// Holder is what holds the terminal of the agents hataori starts in
	// one (design, section 4.3): "holder", HolderHost (hataori's own
	// session host) when absent, or HolderTmux. An agent on pipes is always
	// hataori's own host's.
	Holder string
	// HostInputIdle ends an operation of a pane after this long without a
	// key: "host_input_idle", 5 minutes when absent, at least one second.
	HostInputIdle time.Duration
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

	// Drivers, Adapters and Forges hold each driver's, adapter's and forge's
	// own section of the config, by registered name, unparsed.
	Drivers  map[string][]byte
	Adapters map[string][]byte
	Forges   map[string][]byte
	// AgentDefaults holds the model and effort keys of each adapter section,
	// by adapter name, taken out of Adapters.
	AgentDefaults map[string]AgentDefaults

	// Plugins is the "plugins" list, each entry unparsed: internal/plugin
	// reads and checks it. Nil when the config has none.
	Plugins []json.RawMessage
}

// The holders of the "holder" key.
const (
	HolderHost = "host" // hataori's own session host
	HolderTmux = "tmux" // a window of hataori's own on the default tmux server
)

// AgentDefaults are the model and effort keys read from an adapter section.
type AgentDefaults struct{ Model, Effort string }

// Scope is the config of one scope.
type Scope struct {
	Key     string   // opaque; for chat, a channel
	Name    string   // what the dashboard calls it; empty for its channel's name, else its key
	Client  string   // name of the client that owns the scope
	Enabled bool     // whether threads may be started in it
	Agent   string   // "agent": the adapter of threads started here; empty for Config.Agent
	Model   string   // default model; empty for the agent's own default
	Effort  string   // default effort; empty for the agent's own default
	People  []string // names of the people who may act here besides the owner
	// Machines restricts ordinary starts. Nil permits all configured machines;
	// a non-nil empty list permits none. Confined threads stay local.
	Machines []string
	// ShowIn lists the channels, besides the scope's own, where its threads
	// may be shown, each as "<client>:<channel>" (design, section 5.2).
	ShowIn []string
	// TrustFiles are the files, such as a project settings file, that a
	// start-up dialog of a new workspace may name and that hataori may let
	// this scope's threads past it for (agent.Spec.TrustFiles); empty for
	// none.
	TrustFiles []string
	// Supervisor is the scope's supervisor (design, section 7.1); nil when
	// the scope names none.
	Supervisor *Supervisor
	// ArchiveWhenAsleep archives the chat threads that hataori opened for
	// the scope's threads when the thread goes asleep or is closed, where
	// the chat service can archive (surface.Limits.SetThread).
	ArchiveWhenAsleep bool
}

// Supervisor is the "supervisor" section of a scope.
type Supervisor struct {
	Agent        string // "agent": the adapter that runs it; the instance's when empty
	Model        string // "model": what the default agent's model order gives a thread of the scope when empty
	Effort       string // "effort": supervisor.DefaultEffort when empty
	Instructions string // "instructions": the config's instructions, last in the config's layer of the supervisor's (#656)
	// Quiet is how long the supervisor's waker waits after the first event
	// that wakes it, folding the events until then into one message.
	// "quiet"; 30 seconds when absent.
	Quiet time.Duration
	// OutputMax caps, in bytes, each thread's last output in a wake
	// message. "output_max"; 1024 when absent.
	OutputMax int
	// ThreadsMax is the most threads a wake message shows; the others are
	// counted in one line. "threads_max"; 20 when absent.
	ThreadsMax int
	// The budget of the scope's supervisor (design, section 7.1); 0 turns
	// a limit off. TurnsPerHour: its turns per hour, "turns_per_hour", 12
	// when absent. SendsPerHour: its sends and starts per hour,
	// "sends_per_hour", 20 when absent. TokensPerDay: its tokens per day,
	// "tokens_per_day", 2,000,000 when absent.
	TurnsPerHour int
	SendsPerHour int
	TokensPerDay int64
	// LoopCycles is how many cycles with the supervisor a thread goes
	// through, with no person's message in between, before it no longer
	// wakes it. "loop_cycles"; 3 when absent; 0 turns the guard off.
	LoopCycles int
	// RemindersMax is the most open reminders of the scope, "reminders_max";
	// 10 when absent; with 0 the supervisor sets none.
	RemindersMax int
	// Conversations lets people of the scope start conversations with the
	// supervisor, "conversations"; true when absent. One that exists goes
	// on without.
	Conversations bool
	// ConversationsAtOnce is how many of its conversations run a turn at a
	// time, "conversations_at_once"; 3 when absent, at least 1.
	ConversationsAtOnce int
	// Repos are the repositories the supervisor works on, "repos": the
	// only ones its Start may clone. Empty: it starts no thread.
	Repos []Repo
	// Autonomy is what the supervisor does on its own (design, section
	// 7.1): AutonomyWatch, AutonomyPropose or AutonomyAct. "autonomy";
	// AutonomyPropose when absent.
	Autonomy string
	// What the daemon does with its posts (design, section 7.1, "What it
	// posts"). Report: which of a wake's final answers are posted,
	// "report": ReportEvery (the default), ReportNeeded or ReportDigest.
	// ReportEvery is how often a digest is posted, "report_every"; an
	// hour when absent. Mention: whether "@<name>" mentions, "mention":
	// MentionNamed (the default) or MentionNever. QuietHours: when no post
	// mentions anyone, "quiet_hours"; nil for never.
	Report      string
	ReportEvery time.Duration
	Mention     string
	QuietHours  *QuietHours
	// TrustedAuthors are the logins on a forge whose text the supervisor
	// is given though they have no write access to the repository,
	// "trusted_authors"; empty for none.
	TrustedAuthors []string
	// Files are host paths, "files": before each launch of the
	// supervisor's session and before each wake, the daemon copies each
	// into docs/ of the scope's directory under its base name (design,
	// section 7.1, "Its directory and its notes"). Each is absolute, and no
	// two have the same base name, also when told apart by case only.
	Files []string
}

// Repo is one entry of a supervisor's repos.
type Repo struct {
	// Address is the entry as written, as an order's repo: names the
	// repository; what a Start of it clones.
	Address string
	// Forge is its forge name, "<owner>/<name>", or empty for an address
	// that has none, such as a file:// one (ForgeName).
	Forge string
}

// forgePart is one part of a forge name: a name or the owner it belongs to.
var forgePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// scpAddress is git's SSH form [user@]host:path, whose host has no '/'.
var scpAddress = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+:([^\s]+)$`)

// ForgeName returns the forge name of a repository's address, the
// "<owner>/<name>" that a forge knows it by: the path of a URL with a host
// (https, http, ssh or git), or of git's form [user@]host:path with a path
// that does not start with "/", when it is two parts, without a trailing
// ".git" or "/". Any other address, such as a
// local one, has none, and ForgeName returns "".
func ForgeName(address string) string {
	var at string
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return ""
		}
		at = u.Path
	} else if m := scpAddress.FindStringSubmatch(address); m != nil && !strings.HasPrefix(m[1], "/") {
		at = m[1] // a path from "/", as of "file:/a/b", is no forge's
	} else {
		return ""
	}
	at = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(at, "/"), "/"), ".git")
	owner, name, ok := strings.Cut(at, "/")
	if !ok || !forgePart.MatchString(owner) || !forgePart.MatchString(name) ||
		owner == "." || owner == ".." || name == "." || name == ".." {
		return ""
	}
	return owner + "/" + name
}

// The values of a supervisor's autonomy.
const (
	// AutonomyWatch: it reads, and gives no thread input; it may not ask to
	// interrupt either.
	AutonomyWatch = "watch"
	// AutonomyPropose: its sends and starts are proposals that a person of
	// the scope approves before they run.
	AutonomyPropose = "propose"
	// AutonomyAct: its sends and starts run on their own.
	AutonomyAct = "act"
)

// The values of Supervisor.Report and Supervisor.Mention.
const (
	ReportEvery  = "every"
	ReportNeeded = "needed"
	ReportDigest = "digest"

	MentionNamed = "named"
	MentionNever = "never"

	DefaultReportEvery = time.Hour
)

// QuietHours is a daily span of a supervisor's thread with no mention,
// "quiet_hours": From and To are minutes after midnight in Zone; a span
// with From after To goes across midnight. From never equals To.
type QuietHours struct {
	From, To int
	Zone     *time.Location
}

// The defaults of a supervisor's wake (decision 5 of #267).
const (
	DefaultQuiet      = 30 * time.Second
	DefaultOutputMax  = 1024
	DefaultThreadsMax = 20

	DefaultTurnsPerHour = 12
	DefaultSendsPerHour = 20
	DefaultTokensPerDay = 2_000_000

	DefaultLoopCycles = 3

	DefaultRemindersMax = 10

	DefaultConversationsAtOnce = 3
)

type fileSupervisor struct {
	Agent          string   `json:"agent"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Instructions   string   `json:"instructions"`
	Quiet          string   `json:"quiet"`
	OutputMax      *int     `json:"output_max"`
	ThreadsMax     *int     `json:"threads_max"`
	TurnsPerHour   *int     `json:"turns_per_hour"`
	SendsPerHour   *int     `json:"sends_per_hour"`
	TokensPerDay   *int64   `json:"tokens_per_day"`
	LoopCycles     *int     `json:"loop_cycles"`
	RemindersMax   *int     `json:"reminders_max"`
	Conversations  *bool    `json:"conversations"`
	AtOnce         *int     `json:"conversations_at_once"`
	Repos          []string `json:"repos"`
	Autonomy       string   `json:"autonomy"`
	Report         string   `json:"report"`
	ReportEvery    string   `json:"report_every"`
	Mention        string   `json:"mention"`
	TrustedAuthors []string `json:"trusted_authors"`
	Files          []string `json:"files"`
	QuietHours     *struct {
		From string `json:"from"`
		To   string `json:"to"`
		Zone string `json:"zone"`
	} `json:"quiet_hours"`
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
	Agent      string                     `json:"agent"`
	People     *[]filePerson              `json:"people"`
	Trigger    string                     `json:"trigger"`
	Idle       string                     `json:"idle"`
	Model      string                     `json:"model"`
	Effort     string                     `json:"effort"`
	Context    *int                       `json:"context_bytes"`
	LoginRelay *bool                      `json:"login_relay"`
	HostTerms  bool                       `json:"host_terminals"`
	HostInput  bool                       `json:"host_input"`
	InputIdle  string                     `json:"host_input_idle"`
	Holder     string                     `json:"holder"`
	TermSize   *fileTerminalSize          `json:"terminal_size"`
	Scopes     []fileScope                `json:"scopes"`
	Machines   []fileMachine              `json:"machines"`
	Dashboard  *fileDashboard             `json:"dashboard"`
	Cleanup    *fileCleanup               `json:"cleanup"`
	Forges     map[string]json.RawMessage `json:"forges"`
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
	Name       string          `json:"name"`
	Client     string          `json:"client"`
	Enabled    bool            `json:"enabled"`
	Agent      string          `json:"agent"`
	Model      string          `json:"model"`
	Effort     string          `json:"effort"`
	People     []string        `json:"people"`
	Posters    []string        `json:"posters"`
	Supervisor *fileSupervisor `json:"supervisor"`
	ShowIn     []string        `json:"show_in"`
	TrustFiles []string        `json:"trust_files"`
	Machines   []string        `json:"machines"`
	// ArchiveWhenAsleep is "archive_when_asleep".
	ArchiveWhenAsleep bool `json:"archive_when_asleep"`
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
		HostInput:     f.HostInput,
		HostInputIdle: 5 * time.Minute,
		Owner:         strings.TrimSpace(f.Owner),
		Agent:         strings.TrimSpace(f.Agent),
		Trigger:       strings.TrimSpace(f.Trigger),
		Model:         strings.TrimSpace(f.Model),
		Effort:        strings.TrimSpace(f.Effort),
		Forges:        raw(f.Forges),
		Drivers:       raw(f.Drivers),
		Adapters:      raw(f.Adapters),
		Plugins:       f.Plugins,
	}
	if c.Agent == "" && len(c.Adapters) == 1 {
		for name := range c.Adapters {
			c.Agent = name
		}
	}
	if c.Agent == "" && len(c.Adapters) > 1 {
		return nil, fmt.Errorf("config: key %q: name the default agent adapter; %q names %d", "agent", "adapters", len(c.Adapters))
	}
	if c.Agent != "" {
		if _, ok := c.Adapters[c.Agent]; !ok {
			return nil, fmt.Errorf("config: key %q: %q is not configured under %q", "agent", c.Agent, "adapters")
		}
	}
	c.AgentDefaults = map[string]AgentDefaults{}
	for name, section := range c.Adapters {
		var obj map[string]json.RawMessage
		if json.Unmarshal(section, &obj) != nil || obj == nil {
			continue
		}
		var defaults AgentDefaults
		found := false
		for _, field := range []struct {
			key  string
			dest *string
		}{{"model", &defaults.Model}, {"effort", &defaults.Effort}} {
			value, ok := obj[field.key]
			if !ok {
				continue
			}
			found = true
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, field.dest) != nil {
				return nil, fmt.Errorf("config: key %q: not a string", "adapters."+name+"."+field.key)
			}
			*field.dest = strings.TrimSpace(*field.dest)
			delete(obj, field.key)
		}
		if found {
			c.AgentDefaults[name] = defaults
			c.Adapters[name], _ = json.Marshal(obj)
		}
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
	if c.HostInput && !c.HostTerminals {
		return nil, fmt.Errorf("config: key %q is true, which needs key %q to be true as well", "host_input", "host_terminals")
	}
	switch h := strings.TrimSpace(f.Holder); h {
	case "", HolderHost:
		c.Holder = HolderHost
	case HolderTmux:
		c.Holder = HolderTmux
	default:
		return nil, fmt.Errorf("config: key %q: %q is not a holder: say %q or %q", "holder", f.Holder, HolderHost, HolderTmux)
	}
	if f.InputIdle != "" {
		v := strings.TrimSpace(f.InputIdle)
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d%time.Second != 0 {
			return nil, fmt.Errorf("config: key %q: %q is not a whole number of seconds, at least one, such as \"5m\" or \"90s\"", "host_input_idle", v)
		}
		c.HostInputIdle = d
	}
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
	if c.Machines, err = machineConfig(f.Machines, c.Adapters); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for i, s := range f.Scopes {
		key := fmt.Sprintf("scopes[%d]", i)
		sc := Scope{
			Key:     strings.TrimSpace(s.Key),
			Name:    strings.TrimSpace(s.Name),
			Client:  strings.TrimSpace(s.Client),
			Enabled: s.Enabled,
			Agent:   strings.TrimSpace(s.Agent),

			ArchiveWhenAsleep: s.ArchiveWhenAsleep,
			Model:             strings.TrimSpace(s.Model),
			Effort:            strings.TrimSpace(s.Effort),
		}
		if sc.Machines, err = machineReferences(s.Machines, c.Machines, key+".machines"); err != nil {
			return nil, err
		}
		if sc.Agent != "" {
			if _, ok := c.Adapters[sc.Agent]; !ok {
				return nil, fmt.Errorf("config: key %q: %q is not configured under %q", key+".agent", sc.Agent, "adapters")
			}
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
				Effort: strings.TrimSpace(sv.Effort), Instructions: strings.TrimSpace(sv.Instructions),
				Quiet: DefaultQuiet, OutputMax: DefaultOutputMax, ThreadsMax: DefaultThreadsMax,
				TurnsPerHour: DefaultTurnsPerHour, SendsPerHour: DefaultSendsPerHour, TokensPerDay: DefaultTokensPerDay,
				LoopCycles: DefaultLoopCycles, RemindersMax: DefaultRemindersMax, Conversations: true,
				ConversationsAtOnce: DefaultConversationsAtOnce, Autonomy: AutonomyPropose}
			switch a := strings.TrimSpace(sv.Autonomy); a {
			case "":
			case AutonomyWatch, AutonomyPropose, AutonomyAct:
				sc.Supervisor.Autonomy = a
			default:
				return nil, fmt.Errorf("config: key %q: %q is not one of %q, %q and %q", key+".supervisor.autonomy", a, AutonomyWatch, AutonomyPropose, AutonomyAct)
			}
			if a := sc.Supervisor.Agent; a != "" {
				if err := registry.ValidName(a); err != nil {
					return nil, fmt.Errorf("config: key %q: %v", key+".supervisor.agent", err)
				}
			}
			if q := strings.TrimSpace(sv.Quiet); q != "" {
				d, err := time.ParseDuration(q)
				if err != nil || d <= 0 {
					return nil, fmt.Errorf("config: key %q: %q is not a positive duration such as \"30s\"", key+".supervisor.quiet", q)
				}
				sc.Supervisor.Quiet = d
			}
			for _, n := range []struct {
				name string
				v    *int
				to   *int
			}{{"output_max", sv.OutputMax, &sc.Supervisor.OutputMax}, {"threads_max", sv.ThreadsMax, &sc.Supervisor.ThreadsMax},
				{"conversations_at_once", sv.AtOnce, &sc.Supervisor.ConversationsAtOnce}} {
				if n.v == nil {
					continue
				}
				if *n.v <= 0 {
					return nil, fmt.Errorf("config: key %q: %d is not a positive number", key+".supervisor."+n.name, *n.v)
				}
				*n.to = *n.v
			}
			for _, n := range []struct {
				name string
				v    *int
				to   *int
			}{{"turns_per_hour", sv.TurnsPerHour, &sc.Supervisor.TurnsPerHour}, {"sends_per_hour", sv.SendsPerHour, &sc.Supervisor.SendsPerHour},
				{"loop_cycles", sv.LoopCycles, &sc.Supervisor.LoopCycles}} {
				if n.v == nil {
					continue
				}
				if *n.v < 0 {
					return nil, fmt.Errorf("config: key %q: %d is not a number of 0 or more (0 turns the limit off)", key+".supervisor."+n.name, *n.v)
				}
				*n.to = *n.v
			}
			if v := sv.RemindersMax; v != nil {
				if *v < 0 {
					return nil, fmt.Errorf("config: key %q: %d is not a number of 0 or more (0: no reminder)", key+".supervisor.reminders_max", *v)
				}
				sc.Supervisor.RemindersMax = *v
			}
			if v := sv.Conversations; v != nil {
				sc.Supervisor.Conversations = *v
			}
			if v := sv.TokensPerDay; v != nil {
				if *v < 0 {
					return nil, fmt.Errorf("config: key %q: %d is not a number of 0 or more (0 turns the limit off)", key+".supervisor.tokens_per_day", *v)
				}
				sc.Supervisor.TokensPerDay = *v
			}
			// The entries are named by their keys, not their text, which
			// may hold a credential.
			forges := map[string]string{}
			for j, e := range sv.Repos {
				k := fmt.Sprintf("%s.supervisor.repos[%d]", key, j)
				if e == "" || strings.TrimSpace(e) != e || strings.ContainsAny(e, " \t\r\n") {
					return nil, fmt.Errorf("config: key %q: not a repository's address: it is empty or has spaces", k)
				}
				r := Repo{Address: e, Forge: ForgeName(e)}
				for i, had := range sc.Supervisor.Repos {
					if had.Address == e {
						return nil, fmt.Errorf("config: key %q: the same address as %s.supervisor.repos[%d]", k, key, i)
					}
				}
				if r.Forge != "" {
					f := strings.ToLower(r.Forge)
					if other, ok := forges[f]; ok {
						return nil, fmt.Errorf("config: key %q: the forge name %q, as %s has: the forge tools could not tell them apart", k, r.Forge, other)
					}
					forges[f] = k
				}
				sc.Supervisor.Repos = append(sc.Supervisor.Repos, r)
			}
			for j, l := range sv.TrustedAuthors {
				k := fmt.Sprintf("%s.supervisor.trusted_authors[%d]", key, j)
				if l == "" || strings.TrimSpace(l) != l || strings.ContainsAny(l, " \t\r\n/@") {
					return nil, fmt.Errorf("config: key %q: %q is not a login: it is empty or has spaces, a slash or an @", k, l)
				}
				sc.Supervisor.TrustedAuthors = append(sc.Supervisor.TrustedAuthors, l)
			}
			if err := sc.Supervisor.posts(sv, key+".supervisor"); err != nil {
				return nil, err
			}
			bases := map[string]int{}
			for j, f := range sv.Files {
				k := fmt.Sprintf("%s.supervisor.files[%d]", key, j)
				if f == "" || strings.TrimSpace(f) != f || !filepath.IsAbs(f) {
					return nil, fmt.Errorf("config: key %q: %q is not an absolute path", k, f)
				}
				base := strings.ToLower(filepath.Base(f))
				if i, ok := bases[base]; ok {
					return nil, fmt.Errorf("config: key %q: %q has the base name of %s.supervisor.files[%d], %q: both would be docs/%s",
						k, f, key, i, sv.Files[i], filepath.Base(f))
				}
				bases[base] = j
				sc.Supervisor.Files = append(sc.Supervisor.Files, filepath.Clean(f))
			}
		}
		for j, e := range s.ShowIn {
			k := fmt.Sprintf("%s.show_in[%d]", key, j)
			client, channel, ok := strings.Cut(strings.TrimSpace(e), ":")
			if !ok || channel == "" || strings.TrimSpace(channel) != channel {
				return nil, fmt.Errorf("config: key %q: %q is not <client>:<channel>", k, e)
			}
			if err := registry.ValidName(client); err != nil {
				return nil, fmt.Errorf("config: key %q: %v", k, err)
			}
			entry := client + ":" + channel
			if client == sc.Client && channel == sc.Key {
				return nil, fmt.Errorf("config: key %q: %q is the scope's own channel", k, entry)
			}
			if slices.Contains(sc.ShowIn, entry) {
				return nil, fmt.Errorf("config: key %q: %q is listed twice", k, entry)
			}
			sc.ShowIn = append(sc.ShowIn, entry)
		}
		for j, f := range s.TrustFiles {
			k := fmt.Sprintf("%s.trust_files[%d]", key, j)
			f = strings.TrimSpace(f)
			if f == "" || strings.ContainsAny(f, " ()") {
				return nil, fmt.Errorf("config: key %q: %q is not a file name as the agent's start-up dialog writes it, a path with no space or parenthesis", k, f)
			}
			if slices.Contains(sc.TrustFiles, f) {
				return nil, fmt.Errorf("config: key %q: %q is listed twice", k, f)
			}
			sc.TrustFiles = append(sc.TrustFiles, f)
		}
		c.Scopes = append(c.Scopes, sc)
	}
	for _, section := range []struct {
		key string
		m   map[string][]byte
	}{{"drivers", c.Drivers}, {"adapters", c.Adapters}, {"forges", c.Forges}} {
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
const OldOwner = people.OldOwner

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
			name := people.OldPoster(sc.Client, p)
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
			if key == "" && (k == "drivers" || k == "adapters" || k == "forges" || k == "plugins") {
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

// posts reads the keys of what the daemon does with the supervisor's posts
// from sv, under key.
func (s *Supervisor) posts(sv *fileSupervisor, key string) error {
	s.Report, s.ReportEvery, s.Mention = ReportEvery, DefaultReportEvery, MentionNamed
	switch r := strings.TrimSpace(sv.Report); r {
	case "":
	case ReportEvery, ReportNeeded, ReportDigest:
		s.Report = r
	default:
		return fmt.Errorf("config: key %q: %q is not one of \"every\", \"needed\" and \"digest\"", key+".report", sv.Report)
	}
	if e := strings.TrimSpace(sv.ReportEvery); e != "" {
		d, err := time.ParseDuration(e)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: key %q: %q is not a positive duration such as \"1h\"", key+".report_every", sv.ReportEvery)
		}
		s.ReportEvery = d
	}
	switch m := strings.TrimSpace(sv.Mention); m {
	case "":
	case MentionNamed, MentionNever:
		s.Mention = m
	default:
		return fmt.Errorf("config: key %q: %q is not one of \"named\" and \"never\"", key+".mention", sv.Mention)
	}
	q := sv.QuietHours
	if q == nil {
		return nil
	}
	qh := &QuietHours{Zone: time.Local}
	for _, t := range []struct {
		name, v string
		to      *int
	}{{"from", q.From, &qh.From}, {"to", q.To, &qh.To}} {
		m, ok := clockTime(strings.TrimSpace(t.v))
		if !ok {
			return fmt.Errorf("config: key %q: %q is not a time of day such as \"22:00\"", key+".quiet_hours."+t.name, t.v)
		}
		*t.to = m
	}
	if qh.From == qh.To {
		return fmt.Errorf("config: key %q: it is the same time as %q; quiet hours need a span", key+".quiet_hours.to", key+".quiet_hours.from")
	}
	if z := strings.TrimSpace(q.Zone); z != "" {
		loc, err := time.LoadLocation(z)
		if err != nil {
			return fmt.Errorf("config: key %q: %q is not a time zone name such as \"Asia/Tokyo\"", key+".quiet_hours.zone", q.Zone)
		}
		qh.Zone = loc
	}
	s.QuietHours = qh
	return nil
}

// clockTime reads "HH:MM", 00:00 to 23:59, as minutes after midnight.
func clockTime(v string) (int, bool) {
	if len(v) != 5 || v[2] != ':' {
		return 0, false
	}
	h, err1 := strconv.Atoi(v[:2])
	m, err2 := strconv.Atoi(v[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 || v[0] == '+' || v[0] == '-' || v[3] == '+' || v[3] == '-' {
		return 0, false
	}
	return h*60 + m, true
}
