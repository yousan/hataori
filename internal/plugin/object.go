package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// object is one JSON object of a config entry or a manifest, read key by
// key, so that every error names its key as the rest of the config does.
type object struct {
	prefix string // what every error starts with, such as "config: "
	key    string // where the object is, such as "plugins[2]"; empty at a file's top
	fields map[string]json.RawMessage
}

// readObject reads data as an object at key whose keys are all in known.
func readObject(prefix, key string, data json.RawMessage, known []string) (object, error) {
	o := object{prefix: prefix, key: key}
	if k := kind(data); k != "object" {
		if key == "" {
			return o, fmt.Errorf("%sis %s, not one JSON object", prefix, k)
		}
		if k == "null" {
			return o, fmt.Errorf("%skey %q must not be null", prefix, key)
		}
		return o, fmt.Errorf("%skey %q must be an object, not %s", prefix, key, k)
	}
	if err := json.Unmarshal(data, &o.fields); err != nil {
		return o, fmt.Errorf("%skey %q: %v", prefix, key, err)
	}
	var unknown []string
	for k := range o.fields {
		if !slices.Contains(known, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return o, fmt.Errorf("%sunknown key %q", prefix, o.at(unknown[0]))
	}
	return o, nil
}

// at is the full key of the object's key name.
func (o object) at(name string) string {
	if o.key == "" {
		return name
	}
	return o.key + "." + name
}

func (o object) fail(name, format string, args ...any) error {
	return fmt.Errorf("%skey %q: %s", o.prefix, o.at(name), fmt.Sprintf(format, args...))
}

func (o object) missing(name string) error {
	return fmt.Errorf("%smissing required key %q", o.prefix, o.at(name))
}

// value is the key's value, and whether the object has the key. A null is
// refused, as everywhere in the config.
func (o object) value(name string) (json.RawMessage, bool, error) {
	v, ok := o.fields[name]
	if !ok {
		return nil, false, nil
	}
	if kind(v) == "null" {
		return nil, true, fmt.Errorf("%skey %q must not be null", o.prefix, o.at(name))
	}
	return v, true, nil
}

// typed reads the key into v when its JSON kind is want.
func (o object) typed(name, want, what string, v any) (bool, error) {
	raw, ok, err := o.value(name)
	if !ok || err != nil {
		return ok, err
	}
	if k := kind(raw); k != want {
		return true, fmt.Errorf("%skey %q must be %s, not %s", o.prefix, o.at(name), what, k)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("%skey %q must be %s: %v", o.prefix, o.at(name), what, err)
	}
	return true, nil
}

// str reads a string, without the spaces around it.
func (o object) str(name string) (string, bool, error) {
	var s string
	ok, err := o.typed(name, "string", "a string", &s)
	return strings.TrimSpace(s), ok, err
}

// required reads a string that must be there and not be empty.
func (o object) required(name string) (string, error) {
	s, ok, err := o.str(name)
	if err != nil {
		return "", err
	}
	if !ok || s == "" {
		return "", o.missing(name)
	}
	return s, nil
}

func (o object) boolean(name string) (bool, error) {
	var b bool
	_, err := o.typed(name, "bool", "true or false", &b)
	return b, err
}

func (o object) integer(name string) (int, bool, error) {
	var n int
	ok, err := o.typed(name, "number", "an integer", &n)
	return n, ok, err
}

// duration reads a positive Go duration, such as "15s".
func (o object) duration(name string) (time.Duration, bool, error) {
	s, ok, err := o.str(name)
	if !ok || err != nil {
		return 0, ok, err
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, true, o.fail(name, "%q is not a positive duration such as \"15s\"", s)
	}
	return d, true, nil
}

// list reads a list, each element unread.
func (o object) list(name string) ([]json.RawMessage, bool, error) {
	var l []json.RawMessage
	ok, err := o.typed(name, "array", "a list", &l)
	return l, ok, err
}

// names reads a list of strings, each without the spaces around it, each
// one that check accepts (it says why not), and none twice.
func (o object) names(name string, check func(string) string) ([]string, bool, error) {
	l, ok, err := o.list(name)
	if !ok || err != nil {
		return nil, ok, err
	}
	out := []string{}
	for i, raw := range l {
		k := fmt.Sprintf("%s[%d]", name, i)
		var s string
		switch kind(raw) {
		case "string":
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, true, o.fail(k, "%v", err)
			}
		case "null":
			return nil, true, fmt.Errorf("%skey %q must not be null", o.prefix, o.at(k))
		default:
			return nil, true, fmt.Errorf("%skey %q must be a string, not %s", o.prefix, o.at(k), kind(raw))
		}
		s = strings.TrimSpace(s)
		if why := check(s); why != "" {
			return nil, true, o.fail(k, "%s", why)
		}
		if slices.Contains(out, s) {
			return nil, true, o.fail(k, "%q is listed twice", s)
		}
		out = append(out, s)
	}
	return out, true, nil
}

// kind is the JSON kind of a value, in encoding/json's words.
func kind(v json.RawMessage) string {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return "nothing"
	}
	switch v[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	}
	return "number"
}
