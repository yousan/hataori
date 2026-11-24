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
	Object  FieldType = "object" // since 3.6
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
	"approval_asked": {req("request", String), req("action", String), req("target", Integer), req("turn", Integer), text("reason", String), opt("principal", String), opt("from", String)},
	"approval_answered": {req("request", String), req("allow", Boolean), req("outcome", String), req("target", Integer), opt("principal", String), opt("from", String), opt("link", Object),
		opt("via", String)},
	// proposal_made: a caller with Propose proposed a send or a start
	// (Caller.Propose), in the thread it asks in. proposal_answered: a
	// person decided it, and what came of it. Since 3.26.
	"proposal_made": {req("request", String), req("number", Integer), req("action", String), opt("target", Integer),
		opt("question", String), opt("repo", String), opt("agent", String), opt("model", String), opt("effort", String),
		text("text", String), opt("principal", String), opt("from", String), opt("research", Boolean)},
	"proposal_answered": {req("request", String), req("number", Integer), req("action", String), req("allow", Boolean),
		req("outcome", String), opt("target", Integer), opt("reason", String), opt("principal", String), opt("from", String),
		opt("link", Object), opt("via", String)},
	"thread_created": {req("thread", Integer), req("session", Integer), req("repo", String),
		req("principal", String), opt("from", String), req("model", String), req("effort", String), opt("via", String), opt("research", Boolean)},
	"state": {req("session", Integer), req("from", String), req("to", String), opt("reason", String), text("detail", String),
		text("trust", Array)},
	"turn_started": {req("session", Integer), req("turn", Integer), req("by", String), opt("principal", String), opt("from", String), opt("key", String)},
	"turn_ended": {req("session", Integer), req("turn", Integer), req("by", String), opt("principal", String), opt("from", String),
		req("outcome", String), opt("reason", String), req("background", Integer)},
	// The chat front end's (#352): a row of its outbox was posted, or a
	// plugin's message_sending handler cancelled it.
	"post_sent": {req("link", Object), req("event", Integer), req("part", Integer), req("message", String), text("text", String)},
	"post_cancelled": {req("link", Object), req("event", Integer), req("part", Integer), req("plugin", String),
		opt("failed", Boolean), opt("edit", Boolean), text("reason", String)},
	// post_refused: the chat service refused a row for good, so it was given
	// up (#569). Since 3.35.
	"post_refused": {req("link", Object), req("event", Integer), req("part", Integer), text("reason", String)},
	"input": {req("session", Integer), req("turn", Integer), text("text", String), opt("principal", String), opt("from", String),
		opt("link", Object), text("before", String), text("after", String), opt("via", String)},
	"answer": {req("session", Integer), req("turn", Integer), text("text", String), opt("principal", String), opt("from", String),
		opt("link", Object), opt("request", String), opt("via", String)},
	// stop: a Stop interrupted the running turn, written in the step that
	// interrupts it, once per such Stop: who asked, and the way of the
	// daemon's own it came through (Caller.Via). Since 3.23.
	"stop":              {req("session", Integer), req("turn", Integer), opt("principal", String), opt("from", String), opt("via", String)},
	"text":              {req("session", Integer), text("text", String), req("final", Boolean)},
	"tool_started":      {req("session", Integer), req("id", String), req("tool", String), opt("parent", String)},
	"tool_ended":        {req("session", Integer), req("id", String), req("tool", String)},
	"subagent_started":  {req("session", Integer), req("id", String), opt("name", String)},
	"subagent_ended":    {req("session", Integer), req("id", String)},
	"request":           {req("session", Integer), req("request", String), req("approval", Boolean), text("text", String), text("choices", Array), opt("multi", Boolean)},
	"request_withdrawn": {req("session", Integer), req("request", String)},
	"file_out":          {req("session", Integer), req("file", String), opt("more", Array), text("caption", String)},
	"usage": {req("session", Integer), opt("turn", Integer), opt("model", String), opt("input", Integer), opt("output", Integer),
		opt("cache_read", Integer), opt("cache_write", Integer), opt("context_used", Integer), opt("context_size", Integer)},
	"login_lost":       {req("agent", String), req("outage", Integer), req("thread", Integer)},
	"login_restored":   {req("agent", String), req("outage", Integer), req("threads", Array), req("scopes", Array)},
	"session_replaced": {req("session", Integer), req("new_session", Integer), req("reason", String)},
	// start_chosen: what a person chose for a start that files of the
	// repository blocked (Trust, StartWithoutSettings, CloseThread): the
	// blocked session, the position of the state event that says so, the
	// choice (StartChoice), the session started for it, if any, and who
	// chose. Written in the step that carries the choice out. Since 3.33.
	"start_chosen": {req("session", Integer), req("event", Integer), req("choice", String), opt("new_session", Integer),
		opt("principal", String), opt("from", String), opt("via", String)},
	// reopened: a closed or removed thread was taken back to asleep
	// (ReopenThread), after its state event: the session its next message
	// wakes, who reopened it, what became of its directory and whether the
	// conversation resumes (Reopened). Since 3.31.
	"reopened": {req("session", Integer), opt("principal", String), req("workspace", String), req("conversation", String)},
	"removal_refused": {req("session", Integer), req("trigger", String), opt("principal", String), req("reason", String),
		opt("repo", String), opt("kind", String), text("detail", String)},
	// named: the name the chat service shows for a conversation, a scope's
	// channel or a linked thread, changed (SetName). Since 3.5.
	"named": {req("client", String), req("channel", String), opt("thread", String), text("name", String)},
	// layout: a chat client's layout changed (SetLayout). It belongs to no
	// thread and no scope, holds nothing but the client's name, and every
	// caller that reads events gets it, so that a reader knows to read the
	// layout again. Since 3.21.
	"layout": {req("client", String)},
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
	case Object:
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}
