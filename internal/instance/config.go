// Package instance opens one hataori instance: its root directory, its
// config and its lock. An instance reads and writes only under its root.
package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/registry"
)

// ConfigFile is the config's path relative to the root.
const ConfigFile = "config/hataori.json"

// Config is an instance's config, parsed once at start-up.
type Config struct {
	Owner   string        // the principal that owns the instance
	Trigger string        // the word that starts a thread
	Idle    time.Duration // idle period before a session is put to sleep
	Scopes  []Scope
	// LoginRelay is whether a login outage starts the agent's login
	// command for the owner (section 4.7); "login_relay": false switches it
	// off, and the alert stays.
	LoginRelay bool

	// Drivers and Adapters hold each driver's and adapter's own section of
	// the config, by registered name, unparsed.
	Drivers  map[string][]byte
	Adapters map[string][]byte
}

// Scope is the config of one scope.
type Scope struct {
	Key     string   // opaque; for chat, a channel
	Client  string   // name of the client that owns the scope
	Enabled bool     // whether threads may be started in it
	Model   string   // default model; empty for the agent's own default
	Effort  string   // default effort; empty for the agent's own default
	Posters []string // principals that may post besides the owner
}

// The config as written in the file.
type fileConfig struct {
	Owner      string                     `json:"owner"`
	Trigger    string                     `json:"trigger"`
	Idle       string                     `json:"idle"`
	LoginRelay *bool                      `json:"login_relay"`
	Scopes     []fileScope                `json:"scopes"`
	Drivers    map[string]json.RawMessage `json:"drivers"`
	Adapters   map[string]json.RawMessage `json:"adapters"`
}

type fileScope struct {
	Key     string   `json:"key"`
	Client  string   `json:"client"`
	Enabled bool     `json:"enabled"`
	Model   string   `json:"model"`
	Effort  string   `json:"effort"`
	Posters []string `json:"posters"`
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
		LoginRelay: f.LoginRelay == nil || *f.LoginRelay,
		Owner:      strings.TrimSpace(f.Owner),
		Trigger:    strings.TrimSpace(f.Trigger),
		Drivers:    raw(f.Drivers),
		Adapters:   raw(f.Adapters),
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
		for _, p := range s.Posters {
			if p = strings.TrimSpace(p); p != "" {
				sc.Posters = append(sc.Posters, p)
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
	return c, nil
}

// rejectNull refuses a null anywhere outside the opaque sections, since
// encoding/json would read it as the zero value instead of a wrong type.
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
			if key == "" && (k == "drivers" || k == "adapters") {
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
