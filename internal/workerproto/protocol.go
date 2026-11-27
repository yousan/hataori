package workerproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const Version = "1.1"

const (
	MaxFrame = 1 << 20 // Includes the terminating newline.
	MaxChunk = 64 << 10
)

// Binding is registered out of band. A hello never creates a binding.
type Binding struct {
	Main   string `json:"main"`
	Worker string `json:"worker"`
}

// Target identifies a reserved host run, including before its host exists.
// It deliberately carries neither a PID nor a filesystem/socket address.
type Target struct {
	Thread  uint64 `json:"thread"`
	Session string `json:"session"`
	Run     string `json:"run"`
}

type Hello struct {
	Version      string   `json:"version"`
	Binding      Binding  `json:"binding"`
	Capabilities []string `json:"capabilities"`
	Required     []string `json:"required,omitempty"`
}

type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return "worker protocol: " + r.Code + ": " + r.Message }

func refuse(code, message string) error { return &Refusal{Code: code, Message: message} }

// VersionRefusal is safe to return even before a connection is negotiated.
type VersionRefusal struct {
	Local   string  `json:"local"`
	Peer    string  `json:"peer"`
	Refusal Refusal `json:"error"`
}

func (r *VersionRefusal) Error() string {
	return fmt.Sprintf("worker protocol: incompatible versions %s and %s", r.Local, r.Peer)
}

// Connection contains only verified binding and negotiated capabilities. It
// cannot be constructed by decoding a peer's JSON into an exported struct.
type Connection struct {
	binding Binding
	version string
	caps    map[string]bool
}

func version(s string) (int, int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	n := [2]int{}
	for i, p := range parts {
		if p == "" || len(p) > 6 || (len(p) > 1 && p[0] == '0') {
			return 0, 0, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, 0, false
			}
		}
		var err error
		n[i], err = strconv.Atoi(p)
		if err != nil {
			return 0, 0, false
		}
	}
	return n[0], n[1], n[0] > 0
}

func identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ValidID checks the syntax of an opaque identifier only. It grants no access
// to a handle, installation or session; those need their own binding checks.
func ValidID(s string) bool { return identifier(s) }

func validBinding(b Binding) bool { return identifier(b.Main) && identifier(b.Worker) }
func validTarget(t Target) bool {
	return t.Thread != 0 && identifier(t.Session) && identifier(t.Run)
}

// Negotiate checks both hellos against the receiver's durable registration.
// Call it before constructing a dispatcher or resolving any peer resource.
func Negotiate(registered Binding, local, peer Hello) (*Connection, error) {
	if !validBinding(registered) || local.Binding != registered || peer.Binding != registered {
		return nil, refuse("binding", "registered installation binding does not match")
	}
	major, minor, ok := version(local.Version)
	pmajor, pminor, pok := version(peer.Version)
	if !ok || !pok {
		return nil, refuse("invalid_message", "invalid protocol version")
	}
	if major != pmajor {
		return nil, &VersionRefusal{Local: local.Version, Peer: peer.Version,
			Refusal: Refusal{Code: "incompatible_major", Message: "protocol majors differ"}}
	}
	if pminor < minor {
		minor = pminor
	}
	offered := make(map[string]bool)
	for _, cap := range local.Capabilities {
		if !identifier(cap) || offered[cap] {
			return nil, refuse("invalid_message", "invalid local capability offer")
		}
		offered[cap] = true
	}
	caps, seen := make(map[string]bool), make(map[string]bool)
	for _, cap := range peer.Capabilities {
		if !identifier(cap) || seen[cap] {
			return nil, refuse("invalid_message", "invalid peer capability offer")
		}
		seen[cap] = true
		if offered[cap] && knownCapability(cap) {
			caps[cap] = true
		}
	}
	for _, requirements := range [][]string{local.Required, peer.Required} {
		for _, cap := range requirements {
			if !caps[cap] {
				return nil, refuse("unsupported_capability", "required capability was not negotiated")
			}
		}
	}
	for _, op := range operations {
		if op.Durable && caps[op.Capability] && !caps["receipts"] {
			return nil, refuse("unsupported_capability", "durable capabilities require receipts")
		}
	}
	return &Connection{binding: registered, version: fmt.Sprintf("%d.%d", major, minor), caps: caps}, nil
}

func (c *Connection) Hello() Hello {
	caps := make([]string, 0, len(c.caps))
	for cap := range c.caps {
		caps = append(caps, cap)
	}
	sort.Strings(caps)
	return Hello{Version: c.version, Binding: c.binding, Capabilities: caps}
}

// Message.Body is decoded only after envelope authority checks. Dispatchers
// must use the operation's documented body and must not interpret extensions.
type Message struct {
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Binding Binding         `json:"binding"`
	Target  *Target         `json:"target,omitempty"`
	Op      string          `json:"op"`
	Key     string          `json:"key,omitempty"`
	Body    json.RawMessage `json:"body"`
}

// CheckTarget requires equality with a receiver-owned record, never a target
// supplied by the peer as both arguments.
func CheckTarget(expected, received Target) error {
	if !validTarget(expected) || expected != received {
		return refuse("wrong_target", "thread, session or host run does not match")
	}
	return nil
}

// CheckInventory refuses unregistered reports. expected comes from durable
// receiver records, not from the inventory. An incomplete inventory never
// proves a missing host is gone.
func CheckInventory(expected []Target, received Inventory) error {
	known := make(map[Target]bool, len(expected))
	for _, t := range expected {
		if !validTarget(t) {
			return refuse("wrong_target", "invalid registered inventory target")
		}
		known[t] = true
	}
	for _, h := range received.Hosts {
		if !known[h.Target] {
			return refuse("wrong_target", "inventory names an unregistered host")
		}
	}
	return nil
}

// DecodeHello applies the same bounded JSON and reserved-field checks as frames.
func DecodeHello(data []byte) (Hello, error) {
	var h Hello
	if err := decode(data, &h); err != nil {
		return Hello{}, err
	}
	if _, _, ok := version(h.Version); !ok || !validBinding(h.Binding) {
		return Hello{}, refuse("invalid_message", "invalid hello")
	}
	return h, nil
}

// Decode checks authority and shape, but is not a durable receipt store. A nil
// expected target permits connection operations only. A session dispatcher
// passes a target from its own registration, even for replies and streams.
func (c *Connection) Decode(data []byte, expected *Target) (Message, error) {
	var m Message
	if err := decode(data, &m); err != nil {
		return Message{}, err
	}
	if c == nil || c.binding != m.Binding || !validBinding(m.Binding) {
		return Message{}, refuse("binding", "registered installation binding does not match")
	}
	// An error reply must remain decodable when it refuses an unknown operation
	// or an unnegotiated capability. Its authority comes from the outstanding
	// request's receiver-owned target, not from the rejected operation name.
	if m.Kind == "refusal" {
		if expected == nil && m.Target != nil || expected != nil && m.Target == nil {
			return Message{}, refuse("wrong_target", "refusal does not match the outstanding request scope")
		}
		if expected != nil {
			if err := CheckTarget(*expected, *m.Target); err != nil {
				return Message{}, err
			}
		}
		if !identifier(m.ID) || !identifier(m.Op) || m.Key != "" && !identifier(m.Key) {
			return Message{}, refuse("invalid_message", "invalid refusal envelope")
		}
		if err := validateBody(m); err != nil {
			return Message{}, err
		}
		return m, nil
	}
	op, ok := operations[m.Op]
	if !ok {
		return Message{}, refuse("unsupported_operation", "operation is not supported")
	}
	_, minor, _ := version(c.version)
	if minor < op.Since {
		return Message{}, refuse("unsupported_operation", "operation is unavailable at the negotiated minor")
	}
	if !c.caps[op.Capability] {
		return Message{}, refuse("unsupported_capability", "operation capability was not negotiated")
	}
	if op.Session {
		if expected == nil || m.Target == nil {
			return Message{}, refuse("wrong_target", "a registered session target is required")
		}
		if err := CheckTarget(*expected, *m.Target); err != nil {
			return Message{}, err
		}
	} else if expected != nil || m.Target != nil {
		return Message{}, refuse("wrong_target", "connection operation cannot address a session")
	}
	if !identifier(m.ID) || (m.Key != "" && !identifier(m.Key)) || (op.Durable && !identifier(m.Key)) {
		return Message{}, refuse("invalid_message", "invalid request or operation key")
	}
	if err := validateBody(m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Encode supplies the newline counted by MaxFrame. Decode must still be used
// against the receiver's own binding/target before dispatch.
func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > MaxFrame {
		return nil, refuse("limit", "frame exceeds limit")
	}
	return b, nil
}

// Reserved address/authority fields are refused at any JSON object depth.
// Unknown optional extensions otherwise remain inert. No parser in this package
// opens a resource, so even failures cannot probe a supplied address.
var reserved = map[string]bool{
	"path": true, "socket": true, "address": true, "main_socket": true,
	"worker_socket": true, "command": true, "env": true, "token": true,
	"owner": true, "person": true, "principal": true, "supervisor": true,
	"caller": true, "role": true, "pid": true,
}

func decode(data []byte, dst any) error {
	if len(data) == 0 || len(data) > MaxFrame {
		return refuse("limit", "frame exceeds limit or is empty")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := object(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return refuse("invalid_message", "trailing JSON")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return refuse("invalid_message", "invalid JSON message")
	}
	return nil
}

func object(d *json.Decoder, depth int) error {
	if depth > 32 {
		return refuse("limit", "JSON nesting exceeds limit")
	}
	tok, err := d.Token()
	if err != nil {
		return refuse("invalid_message", "invalid JSON")
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim || delim != '{' {
		return refuse("invalid_message", "expected JSON object")
	}
	return members(d, depth)
}

func members(d *json.Decoder, depth int) error {
	seen := make(map[string]bool)
	for d.More() {
		tok, err := d.Token()
		key, ok := tok.(string)
		if err != nil || !ok || !fieldName(key) || seen[key] || reserved[key] {
			return refuse("invalid_message", "noncanonical, duplicate or reserved JSON field")
		}
		seen[key] = true
		if err := value(d, depth+1); err != nil {
			return err
		}
	}
	if tok, err := d.Token(); err != nil || tok != json.Delim('}') {
		return refuse("invalid_message", "invalid JSON object")
	}
	return nil
}

// encoding/json also matches struct fields using Unicode simple folding.
// Enforce the protocol's canonical spelling before Unmarshal so a differently
// cased or Unicode alias can neither supply nor overwrite a known field. This
// applies recursively to envelopes, bodies and unknown optional extensions.
func fieldName(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func value(d *json.Decoder, depth int) error {
	if depth > 32 {
		return refuse("limit", "JSON nesting exceeds limit")
	}
	tok, err := d.Token()
	if err != nil {
		return refuse("invalid_message", "invalid JSON value")
	}
	switch tok {
	case json.Delim('{'):
		return members(d, depth)
	case json.Delim('['):
		for d.More() {
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		if tok, err := d.Token(); err != nil || tok != json.Delim(']') {
			return refuse("invalid_message", "invalid JSON array")
		}
	case json.Delim('}'), json.Delim(']'):
		return refuse("invalid_message", "unexpected JSON delimiter")
	}
	return nil
}
