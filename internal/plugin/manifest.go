package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/registry"
)

// ManifestFile is the manifest's name. It is in the directory of the
// plugin's executable.
const ManifestFile = "hataori-plugin.json"

// MaxManifest is the size, in bytes, of the largest manifest read.
const MaxManifest = 1 << 20

// Manifest is what a plugin says it is and asks for (design, section 7.3).
type Manifest struct {
	ID         string   // the plugin's name
	Version    string   // the plugin's version, as its author writes it
	Protocol   Protocol // the control protocol it speaks
	Operations []string // the control operations it calls
	Events     []string // the event kinds it reads
	Hooks      []ManifestHook
	// Conversation is whether it asks to read message text.
	Conversation bool
	// Config is a JSON Schema for the plugin's settings, an object or true
	// or false, kept as written and not interpreted; nil when the manifest
	// has none.
	Config json.RawMessage
}

// ManifestHook is a hook point a plugin handles, as its manifest asks.
type ManifestHook struct {
	Point    string
	Priority int           // higher runs first; 0 when the manifest names none
	Timeout  time.Duration // 0 when the manifest names none
}

// Protocol is the control protocol a plugin speaks: the major version,
// and the lowest minor version it needs.
type Protocol struct{ Major, Minor int }

func (p Protocol) String() string { return fmt.Sprintf("%d.%d", p.Major, p.Minor) }

var (
	manifestKeys     = []string{"id", "version", "protocol", "operations", "events", "hooks", "conversation", "config"}
	manifestHookKeys = []string{"point", "priority", "timeout"}
)

// ReadManifest reads and checks the manifest at path. It opens no other
// file and runs nothing. Every error is one line that names the manifest
// and the key.
func ReadManifest(path string) (*Manifest, error) {
	prefix := fmt.Sprintf("manifest %q: ", path)
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no manifest %q", path)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err // without the path, which the prefix quotes
	}
	if err != nil {
		return nil, fmt.Errorf("%s%v", prefix, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var top json.RawMessage
	if err := dec.Decode(&top); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return nil, fmt.Errorf("%snot valid JSON: syntax error at byte %d: %v", prefix, se.Offset, err)
		}
		return nil, fmt.Errorf("%snot valid JSON: %v", prefix, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%sunexpected data after the top-level object", prefix)
	}
	o, err := readObject(prefix, "", top, manifestKeys)
	if err != nil {
		return nil, err
	}

	m := &Manifest{}
	if m.ID, err = o.required("id"); err != nil {
		return nil, err
	}
	if err := registry.ValidName(m.ID); err != nil {
		return nil, o.fail("id", "%s", strings.TrimPrefix(err.Error(), "registry: "))
	}
	if m.Version, err = o.required("version"); err != nil {
		return nil, err
	}
	p, err := o.required("protocol")
	if err != nil {
		return nil, err
	}
	var ok bool
	if m.Protocol, ok = parseProtocol(p); !ok {
		return nil, o.fail("protocol", "%q is not a version such as \"3.0\": the major version and the lowest minor version the plugin needs", p)
	}
	if m.Operations, _, err = o.names("operations", checkOperation); err != nil {
		return nil, err
	}
	if m.Events, _, err = o.names("events", checkEvent); err != nil {
		return nil, err
	}
	hooks, _, err := o.list("hooks")
	if err != nil {
		return nil, err
	}
	for i, raw := range hooks {
		ho, err := readObject(prefix, o.at(fmt.Sprintf("hooks[%d]", i)), raw, manifestHookKeys)
		if err != nil {
			return nil, err
		}
		var h ManifestHook
		if h.Point, err = ho.required("point"); err != nil {
			return nil, err
		}
		if why := checkPoint(h.Point); why != "" {
			return nil, ho.fail("point", "%s", why)
		}
		for _, other := range m.Hooks {
			if other.Point == h.Point {
				return nil, ho.fail("point", "%q is listed twice", h.Point)
			}
		}
		if h.Priority, _, err = ho.integer("priority"); err != nil {
			return nil, err
		}
		if h.Timeout, _, err = ho.duration("timeout"); err != nil {
			return nil, err
		}
		m.Hooks = append(m.Hooks, h)
	}
	if m.Conversation, err = o.boolean("conversation"); err != nil {
		return nil, err
	}
	schema, ok, err := o.value("config")
	if err != nil {
		return nil, err
	}
	if ok {
		if k := kind(schema); k != "object" && k != "bool" {
			return nil, o.fail("config", "a JSON Schema is an object or true or false, not %s", k)
		}
		m.Config = schema
	}
	return m, nil
}

// readFile reads a regular file of at most MaxManifest bytes. It opens it
// without blocking, so that a named pipe in its place cannot hold the
// daemon's start.
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifest {
		return nil, fmt.Errorf("larger than %d bytes", MaxManifest)
	}
	return data, nil
}

// parseProtocol reads "<major>.<minor>", each a number without a sign or a
// leading zero.
func parseProtocol(s string) (Protocol, bool) {
	major, minor, ok := strings.Cut(s, ".")
	a, okA := number(major)
	b, okB := number(minor)
	return Protocol{a, b}, ok && okA && okB
}

func number(s string) (int, bool) {
	if s == "" || len(s) > 6 || len(s) > 1 && s[0] == '0' || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}
