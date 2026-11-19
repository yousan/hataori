package control

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// FieldType is the JSON type of an event field.
type FieldType string

// The types of event fields.
const (
	String  FieldType = "string"
	Integer FieldType = "integer"
	Boolean FieldType = "boolean"
	Array   FieldType = "array"
)

// Field is one field of an event's data.
type Field struct {
	Name     string
	Type     FieldType
	Optional bool // may be left out (or null)
	Text     bool // text written by a person or an agent; always optional
}

func req(name string, t FieldType) Field { return Field{Name: name, Type: t} }
func opt(name string, t FieldType) Field { return Field{Name: name, Type: t, Optional: true} }
func text(name string, t FieldType) Field {
	return Field{Name: name, Type: t, Optional: true, Text: true}
}

// EventKinds is the schema of every event kind in the event log: the fields
// of its data. docs/protocol.md describes the same, field by field, and a
// test keeps the two equal. The store refuses an event that does not match.
var EventKinds = map[EventKind][]Field{
	"thread_created": {req("thread", Integer), req("session", Integer), req("repo", String),
		req("principal", String), opt("from", String), req("model", String), req("effort", String)},
	"state":        {req("session", Integer), req("from", String), req("to", String), opt("reason", String), text("detail", String)},
	"turn_started": {req("session", Integer), req("turn", Integer), req("by", String), opt("principal", String), opt("from", String), opt("key", String)},
	"turn_ended": {req("session", Integer), req("turn", Integer), req("by", String), opt("principal", String), opt("from", String),
		req("outcome", String), opt("reason", String), req("background", Integer)},
	"input":             {req("session", Integer), req("turn", Integer), text("text", String), opt("principal", String), opt("from", String)},
	"answer":            {req("session", Integer), req("turn", Integer), text("text", String), opt("principal", String), opt("from", String)},
	"text":              {req("session", Integer), text("text", String), req("final", Boolean)},
	"tool_started":      {req("session", Integer), req("id", String), req("tool", String), opt("parent", String)},
	"tool_ended":        {req("session", Integer), req("id", String), req("tool", String)},
	"subagent_started":  {req("session", Integer), req("id", String), opt("name", String)},
	"subagent_ended":    {req("session", Integer), req("id", String)},
	"request":           {req("session", Integer), req("request", String), req("approval", Boolean), text("text", String), text("choices", Array)},
	"request_withdrawn": {req("session", Integer), req("request", String)},
	"file_out":          {req("session", Integer), req("file", String), opt("more", Array), text("caption", String)},
	"usage": {req("session", Integer), opt("turn", Integer), opt("model", String), opt("input", Integer), opt("output", Integer),
		opt("cache_read", Integer), opt("cache_write", Integer), opt("context_used", Integer), opt("context_size", Integer)},
	"login_lost":       {req("agent", String), req("outage", Integer), req("thread", Integer)},
	"login_restored":   {req("agent", String), req("outage", Integer), req("threads", Array), req("scopes", Array)},
	"session_replaced": {req("session", Integer), req("new_session", Integer), req("reason", String)},
	"removal_refused": {req("session", Integer), req("trigger", String), opt("principal", String), req("reason", String),
		opt("repo", String), opt("kind", String), text("detail", String)},
	// named: the name the chat service shows for a conversation, a scope's
	// channel or a linked thread, changed (SetName). Since 3.5.
	"named":  {req("client", String), req("channel", String), opt("thread", String), text("name", String)},
	CaughtUp: {},
}

// KindNames returns the event kinds, sorted.
func KindNames() []EventKind {
	var ks []EventKind
	for k := range EventKinds {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

// CheckEvent reports whether e's kind is in EventKinds and its data has
// exactly the fields of that kind, each of its type.
func CheckEvent(e Event) error {
	fields, ok := EventKinds[e.Kind]
	if !ok {
		return fmt.Errorf("control: event kind %q is not in the schema", e.Kind)
	}
	data := map[string]any{}
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return fmt.Errorf("control: event %s: data is not an object: %w", e.Kind, err)
		}
	}
	known := map[string]Field{}
	for _, f := range fields {
		known[f.Name] = f
		v, present := data[f.Name]
		if !present || v == nil {
			if !f.Optional {
				return fmt.Errorf("control: event %s: field %q is missing", e.Kind, f.Name)
			}
			continue
		}
		if !hasType(v, f.Type) {
			return fmt.Errorf("control: event %s: field %q is not of type %s", e.Kind, f.Name, f.Type)
		}
	}
	for name := range data {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("control: event %s: field %q is not in the schema", e.Kind, name)
		}
	}
	return nil
}

// WithoutText returns e with the text fields of its kind left out of its
// data, for a caller that may not read text (Caller.NoText).
func WithoutText(e Event) Event {
	var m map[string]json.RawMessage
	if len(e.Data) == 0 || json.Unmarshal(e.Data, &m) != nil {
		return e
	}
	cut := false
	for _, f := range EventKinds[e.Kind] {
		if _, ok := m[f.Name]; ok && f.Text {
			delete(m, f.Name)
			cut = true
		}
	}
	if cut {
		e.Data, _ = json.Marshal(m)
	}
	return e
}

func hasType(v any, t FieldType) bool {
	switch t {
	case String:
		_, ok := v.(string)
		return ok
	case Boolean:
		_, ok := v.(bool)
		return ok
	case Integer:
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case Array:
		_, ok := v.([]any)
		return ok
	}
	return false
}
