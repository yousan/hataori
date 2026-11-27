package workerproto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

type Operation struct {
	Name       string
	Capability string
	Session    bool
	Durable    bool
	Since      int // first minor in major 1
}

var operations = map[string]Operation{
	"health":          {"health", "health", false, false, 0},
	"inventory":       {"inventory", "health", false, false, 0},
	"probe":           {"probe", "health", false, false, 0},
	"prepare":         {"prepare", "host", true, true, 0},
	"start":           {"start", "host", true, true, 0},
	"end_host":        {"end_host", "lifecycle", true, true, 1},
	"describe":        {"describe", "runner", true, false, 2},
	"bind_workspace":  {"bind_workspace", "runner", true, true, 2},
	"stop":            {"stop", "host", true, true, 0},
	"input":           {"input", "host", true, true, 0},
	"answer":          {"answer", "host", true, true, 0},
	"receipt":         {"receipt", "receipts", true, false, 0},
	"attach":          {"attach", "terminal", true, false, 0},
	"keys":            {"keys", "terminal", true, false, 0},
	"resize":          {"resize", "terminal", true, false, 0},
	"detach":          {"detach", "terminal", true, false, 0},
	"follow":          {"follow", "replay", true, false, 0},
	"read":            {"read", "replay", true, false, 0},
	"history_release": {"history_release", "replay", true, true, 0},
	"clone":           {"clone", "workspace", true, true, 0},
	"fetch":           {"fetch", "workspace", true, true, 0},
	"inspect":         {"inspect", "workspace", true, false, 0},
	"remove":          {"remove", "workspace", true, true, 0},
	"restore":         {"restore", "workspace", true, true, 0},
	"acknowledge":     {"acknowledge", "workspace", true, true, 0},
	"purge":           {"purge", "workspace", true, true, 0},
	"upload":          {"upload", "transfer", true, true, 0},
	"download":        {"download", "transfer", true, false, 0},
	"login_start":     {"login_start", "login", false, false, 0},
	"login_input":     {"login_input", "login", false, false, 0},
	"login_cancel":    {"login_cancel", "login", false, false, 0},
}

func Operations() []Operation {
	ops := make([]Operation, 0, len(operations))
	for _, op := range operations {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
	return ops
}

func knownCapability(cap string) bool {
	for _, op := range operations {
		if op.Capability == cap {
			return true
		}
	}
	return false
}

var refusalCodes = []string{
	"incompatible_major", "binding", "wrong_target", "invalid_message",
	"unsupported_operation", "unsupported_capability", "not_ready", "stale",
	"key_conflict", "unavailable", "missing", "unreadable", "replaced",
	"truncated", "history_lost", "limit", "proof_refused", "unknown_outcome",
	"not_found", "unsupported_holder", "denied",
}

func RefusalCodes() []string { return append([]string(nil), refusalCodes...) }

type Empty struct{}

type AgentConfig struct {
	Agent  string `json:"agent"`
	Config string `json:"config"`
}

type AgentHealth struct {
	AgentConfig
	ConfigDir    string `json:"config_dir,omitempty"` // 1.2, metadata only
	Installed    bool   `json:"installed"`
	Login        string `json:"login"`                  // logged_in, logged_out, unknown
	Installation string `json:"installation,omitempty"` // 1.1: installed, missing, unknown
}

// Telemetry is an observation, never an admission grant. A consumer checks age,
// boot and configured limits; invalid/unknown samples cannot advertise capacity.
type Telemetry struct {
	ObservedMS int64   `json:"observed_ms"`
	ValidMS    int64   `json:"valid_ms"`
	Known      bool    `json:"known"`
	MemoryFree uint64  `json:"memory_free"`
	DiskFree   uint64  `json:"disk_free,omitempty"` // 1.1; absence is unknown to older peers
	Load       float64 `json:"load"`
}

type Health struct {
	Boot      string        `json:"boot"`
	Ready     bool          `json:"ready"`
	Agents    []AgentHealth `json:"agents"`
	Telemetry Telemetry     `json:"telemetry"`
}

type HostRecord struct {
	Target Target   `json:"target"`
	State  string   `json:"state"` // reserved, running, ended, gone, unknown
	Turn   string   `json:"turn,omitempty"`
	Hooks  []string `json:"hooks,omitempty"`
}

type Inventory struct {
	Boot     string       `json:"boot"`
	Complete bool         `json:"complete"`
	Hosts    []HostRecord `json:"hosts"`
}

type Prepare struct {
	Instructions string   `json:"instructions"`
	Policy       []string `json:"policy"` // session-bound uploaded handles
}

type Start struct {
	AgentConfig
	Holder            string `json:"holder"`
	Workspace         string `json:"workspace"`
	Model             string `json:"model,omitempty"`
	Resume            string `json:"resume,omitempty"`              // opaque adapter conversation ID
	Effort            string `json:"effort,omitempty"`              // 1.2
	NoProjectSettings bool   `json:"no_project_settings,omitempty"` // 1.2
}

// RunMetadata carries codec context, never an address on the main. The worker
// resolves all paths using its registered adapter and owned workspace.
type RunMetadata struct {
	AgentConfig
	Dir               string        `json:"dir"`
	StateDir          string        `json:"state_dir"`
	ConfigDir         string        `json:"config_dir"`
	Model             string        `json:"model,omitempty"`
	Effort            string        `json:"effort,omitempty"`
	Resume            string        `json:"resume,omitempty"`
	Instructions      string        `json:"instructions,omitempty"`
	NoProjectSettings bool          `json:"no_project_settings"`
	Terminal          bool          `json:"terminal"`
	State             string        `json:"state"`
	Turn              string        `json:"turn,omitempty"`
	Status            *RunStatus    `json:"status,omitempty"`
	Waiting           []WaitingHook `json:"waiting"`
	Attached          int           `json:"attached"`
	DetachedMS        int64         `json:"detached_ms"`
}

type RunStatus struct {
	Exited   bool   `json:"exited"`
	Code     int    `json:"code"`
	Signal   string `json:"signal,omitempty"`
	Stopped  bool   `json:"stopped,omitempty"`
	StopNote string `json:"stop_note,omitempty"`
	Error    string `json:"error,omitempty"`
	Shutdown bool   `json:"shutdown,omitempty"`
}

type WaitingHook struct {
	ID      string `json:"id"`
	AtMS    int64  `json:"at_ms"`
	Payload []byte `json:"payload"`
}

type Stop struct {
	Turn string `json:"turn"`
}

type Input struct {
	Turn string `json:"turn"`
	Data []byte `json:"data"`
}

type Answer struct {
	Turn    string `json:"turn"`
	Request string `json:"request"`
	Data    []byte `json:"data"`
}

type ReceiptQuery struct {
	Operation string `json:"operation"`
	Key       string `json:"key"`
}

type Effect struct {
	Host      string `json:"host,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Trash     string `json:"trash,omitempty"`
	Handle    string `json:"handle,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Size      uint64 `json:"size,omitempty"`
}

type Receipt struct {
	Operation   string   `json:"operation"`
	Key         string   `json:"key"`
	RequestHash string   `json:"request_hash"`
	State       string   `json:"state"` // pending, complete, unknown, refused
	Result      *Effect  `json:"result,omitempty"`
	Refusal     *Refusal `json:"refusal,omitempty"`
}

type Attachment struct {
	Handle string `json:"handle"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

type Handle struct {
	Handle string `json:"handle"`
}

type Keys struct {
	Handle string `json:"handle"`
	Data   []byte `json:"data"`
}

type Follow struct {
	Source string `json:"source"` // adapter-reported source ID, never a path
}

type Source struct {
	Handle     string `json:"handle"`
	Source     string `json:"source"`
	Generation string `json:"generation"`
}

type Read struct {
	Source
	Order  uint64 `json:"order"`
	Offset uint64 `json:"offset"`
	Limit  uint32 `json:"limit"`
}

// Bytes gives an exact, ordered range. empty is a successful zero-byte read;
// every other no-data state prevents the consumer claiming catch-up.
type Bytes struct {
	Source
	Order  uint64 `json:"order"`
	Offset uint64 `json:"offset"`
	Next   uint64 `json:"next"`
	State  string `json:"state"`
	Data   []byte `json:"data"`
	Cols   uint16 `json:"cols,omitempty"` // 1.2 attachment output only
	Rows   uint16 `json:"rows,omitempty"`
	Reset  bool   `json:"reset,omitempty"`
	Ended  bool   `json:"ended,omitempty"` // explicit host end; EOF alone is link loss
}

type HistoryRelease struct {
	Retired bool `json:"retired"` // Requires the main's durable run retirement.
}

type Clone struct {
	Repository string `json:"repository"` // configured repository ID, not a URL
}

type Workspace struct {
	Workspace string `json:"workspace"`
}

type BindWorkspace struct {
	Previous  Target `json:"previous"`
	Workspace string `json:"workspace"`
}

type Inspection struct {
	Workspace string   `json:"workspace"`
	Identity  string   `json:"identity"`
	Clean     bool     `json:"clean"` // diagnostic only; removal always reproves
	Reasons   []string `json:"reasons"`
}

type Trash struct {
	Trash string `json:"trash"`
}

type Upload struct {
	Handle string `json:"handle"`
	Digest string `json:"digest"` // SHA-256 of the complete accepted file
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data"`
	Final  bool   `json:"final"`
}

type Download struct {
	Handle string `json:"handle"`
	Offset uint64 `json:"offset"`
	Limit  uint32 `json:"limit"`
}

type FileBytes struct {
	Handle string `json:"handle"`
	Digest string `json:"digest"`
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data"`
	Final  bool   `json:"final"`
}

type Login struct {
	AgentConfig
	Attempt string `json:"attempt"`
}

type LoginInput struct {
	Attempt string `json:"attempt"`
	Data    []byte `json:"data"`
}

type LoginAttempt struct {
	Attempt string `json:"attempt"`
}

type LoginStream struct {
	Attempt string `json:"attempt"`
	State   string `json:"state"` // data, complete, cancelled, expired
	Data    []byte `json:"data"`
}

func body[T any](m Message, valid func(T) bool) error {
	var b T
	if err := decode(m.Body, &b); err != nil {
		return err
	}
	if !valid(b) {
		return refuse("invalid_message", "invalid operation body")
	}
	return nil
}

func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

func digest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func agent(c AgentConfig) bool { return identifier(c.Agent) && identifier(c.Config) }
func source(s Source) bool {
	return identifier(s.Handle) && identifier(s.Source) && identifier(s.Generation)
}
func handles(ss ...string) bool {
	for _, s := range ss {
		if !identifier(s) {
			return false
		}
	}
	return true
}
func goodRefusal(r Refusal) bool {
	return oneOf(r.Code, refusalCodes...) && len(r.Message) > 0 && len(r.Message) <= 512
}

func receipt(r Receipt) bool {
	op, ok := operations[r.Operation]
	if !ok || !op.Durable || !identifier(r.Key) || !digest(r.RequestHash) {
		return false
	}
	switch r.State {
	case "pending", "unknown":
		return r.Result == nil && r.Refusal == nil
	case "refused":
		return r.Result == nil && r.Refusal != nil && goodRefusal(*r.Refusal)
	case "complete":
		if r.Result == nil || r.Refusal != nil {
			return false
		}
		e := r.Result
		for _, h := range []string{e.Host, e.Workspace, e.Trash, e.Handle} {
			if h != "" && !identifier(h) {
				return false
			}
		}
		return e.Digest == "" || digest(e.Digest)
	}
	return false
}

func byteRange(b Bytes) bool {
	if !source(b.Source) || b.Order == 0 || len(b.Data) > MaxChunk || b.Next < b.Offset {
		return false
	}
	if b.State == "data" {
		return len(b.Data) > 0 && b.Next-b.Offset == uint64(len(b.Data))
	}
	return oneOf(b.State, "empty", "unavailable", "missing", "unreadable", "replaced", "truncated", "history_lost") && len(b.Data) == 0 && b.Next == b.Offset
}

func validateBody(m Message) error {
	if m.Kind == "refusal" {
		return body(m, goodRefusal)
	}
	if m.Kind == "stream" {
		switch m.Op {
		case "read", "attach":
			return body(m, byteRange)
		case "login_start":
			return body(m, func(b LoginStream) bool {
				return identifier(b.Attempt) && len(b.Data) <= MaxChunk &&
					(oneOf(b.State, "complete", "cancelled", "expired") && len(b.Data) == 0 || b.State == "data" && len(b.Data) > 0)
			})
		}
		return refuse("invalid_message", "operation has no stream form")
	}
	if m.Kind == "reply" {
		if operations[m.Op].Durable || m.Op == "receipt" {
			return body(m, func(r Receipt) bool {
				return receipt(r) && (m.Op == "receipt" || r.Operation == m.Op && r.Key == m.Key)
			})
		}
		switch m.Op {
		case "health":
			return body(m, func(b Health) bool {
				if !identifier(b.Boot) || !telemetry(b.Telemetry) || len(b.Agents) > 128 {
					return false
				}
				for _, a := range b.Agents {
					if !agentHealth(a) {
						return false
					}
				}
				return true
			})
		case "probe":
			return body(m, agentHealth)
		case "inventory":
			return body(m, func(b Inventory) bool {
				if !identifier(b.Boot) || len(b.Hosts) > 4096 {
					return false
				}
				seen := make(map[Target]bool)
				for _, h := range b.Hosts {
					if !validTarget(h.Target) || seen[h.Target] || !oneOf(h.State, "reserved", "running", "ended", "gone", "unknown") || h.Turn != "" && !identifier(h.Turn) || !handles(h.Hooks...) {
						return false
					}
					seen[h.Target] = true
				}
				return true
			})
		case "attach":
			return body(m, func(b Attachment) bool { return identifier(b.Handle) && b.Cols > 0 && b.Rows > 0 })
		case "describe":
			return body(m, func(b RunMetadata) bool {
				if !agent(b.AgentConfig) || !oneOf(b.State, "reserved", "running", "ended", "gone", "unknown") || b.Turn != "" && !identifier(b.Turn) || len(b.Waiting) > 64 || b.Attached < 0 || len(b.Dir) > 4096 || len(b.StateDir) > 4096 || len(b.ConfigDir) > 4096 || len(b.Instructions) > MaxChunk {
					return false
				}
				for _, h := range b.Waiting {
					if !identifier(h.ID) || h.AtMS < 0 || len(h.Payload) > MaxChunk {
						return false
					}
				}
				return true
			})
		case "follow":
			return body(m, source)
		case "read":
			return body(m, byteRange)
		case "inspect":
			return body(m, func(b Inspection) bool { return handles(b.Workspace, b.Identity) && len(b.Reasons) <= 128 })
		case "download":
			return body(m, func(b FileBytes) bool {
				return identifier(b.Handle) && digest(b.Digest) && len(b.Data) <= MaxChunk && b.Offset <= b.Size && uint64(len(b.Data)) <= b.Size-b.Offset && (!b.Final || uint64(len(b.Data)) == b.Size-b.Offset)
			})
		case "login_start", "login_input", "login_cancel":
			return body(m, func(b LoginAttempt) bool { return identifier(b.Attempt) })
		case "keys", "resize", "detach":
			return body(m, func(Empty) bool { return true })
		}
	}
	if m.Kind != "request" {
		return refuse("invalid_message", "invalid message kind")
	}
	switch m.Op {
	case "health", "inventory", "end_host", "describe":
		return body(m, func(Empty) bool { return true })
	case "probe":
		return body(m, agent)
	case "prepare":
		return body(m, func(b Prepare) bool { return len(b.Instructions) <= MaxChunk && handles(b.Policy...) })
	case "start":
		return body(m, func(b Start) bool {
			return agent(b.AgentConfig) && b.Holder == "own" && identifier(b.Workspace) && len(b.Model) <= 128 && len(b.Resume) <= 256 && len(b.Effort) <= 128
		})
	case "stop":
		return body(m, func(b Stop) bool { return identifier(b.Turn) })
	case "input":
		return body(m, func(b Input) bool { return identifier(b.Turn) && len(b.Data) > 0 && len(b.Data) <= MaxChunk })
	case "answer":
		return body(m, func(b Answer) bool { return handles(b.Turn, b.Request) && len(b.Data) <= MaxChunk })
	case "receipt":
		return body(m, func(b ReceiptQuery) bool { return operations[b.Operation].Durable && identifier(b.Key) })
	case "attach", "resize":
		return body(m, func(b Attachment) bool { return identifier(b.Handle) && b.Cols > 0 && b.Rows > 0 })
	case "keys":
		return body(m, func(b Keys) bool { return identifier(b.Handle) && len(b.Data) > 0 && len(b.Data) <= MaxChunk })
	case "detach":
		return body(m, func(b Handle) bool { return identifier(b.Handle) })
	case "follow":
		return body(m, func(b Follow) bool { return identifier(b.Source) })
	case "read":
		return body(m, func(b Read) bool {
			return source(b.Source) && b.Order > 0 && b.Limit > 0 && b.Limit <= MaxChunk && b.Offset <= math.MaxUint64-uint64(b.Limit)
		})
	case "history_release":
		return body(m, func(b HistoryRelease) bool { return b.Retired })
	case "clone":
		return body(m, func(b Clone) bool { return identifier(b.Repository) })
	case "fetch", "inspect", "remove":
		return body(m, func(b Workspace) bool { return identifier(b.Workspace) })
	case "bind_workspace":
		return body(m, func(b BindWorkspace) bool { return validTarget(b.Previous) && identifier(b.Workspace) })
	case "restore", "acknowledge", "purge":
		return body(m, func(b Trash) bool { return identifier(b.Trash) })
	case "upload":
		return body(m, func(b Upload) bool {
			return identifier(b.Handle) && digest(b.Digest) && len(b.Data) <= MaxChunk && b.Offset <= b.Size && uint64(len(b.Data)) <= b.Size-b.Offset && (!b.Final || uint64(len(b.Data)) == b.Size-b.Offset)
		})
	case "download":
		return body(m, func(b Download) bool {
			return identifier(b.Handle) && b.Limit > 0 && b.Limit <= MaxChunk && b.Offset <= math.MaxUint64-uint64(b.Limit)
		})
	case "login_start":
		return body(m, func(b Login) bool { return agent(b.AgentConfig) && identifier(b.Attempt) })
	case "login_input":
		return body(m, func(b LoginInput) bool { return identifier(b.Attempt) && len(b.Data) > 0 && len(b.Data) <= MaxChunk })
	case "login_cancel":
		return body(m, func(b LoginAttempt) bool { return identifier(b.Attempt) })
	}
	return refuse("invalid_message", "invalid operation form")
}

func agentHealth(a AgentHealth) bool {
	if !agent(a.AgentConfig) || !oneOf(a.Login, "logged_in", "logged_out", "unknown") || !a.Installed && a.Login != "unknown" {
		return false
	}
	return a.Installation == "" || a.Installation == "installed" && a.Installed || oneOf(a.Installation, "missing", "unknown") && !a.Installed && a.Login == "unknown"
}

func telemetry(t Telemetry) bool {
	return t.ObservedMS >= 0 && t.ValidMS >= 0 && t.ValidMS <= 60000 && !math.IsNaN(t.Load) && !math.IsInf(t.Load, 0) && t.Load >= 0 &&
		(t.Known && t.ObservedMS > 0 && t.ValidMS > 0 || !t.Known && t.MemoryFree == 0 && t.DiskFree == 0 && t.Load == 0)
}

// FreshTelemetry is a necessary freshness check, never an admission grant by
// itself. Unknown, future-dated, expired or invalid observations cannot prove
// capacity. Disk constraints additionally require protocol 1.1 or later.
func FreshTelemetry(t Telemetry, nowMS int64) bool {
	return telemetry(t) && t.Known && nowMS >= t.ObservedMS && nowMS-t.ObservedMS < t.ValidMS
}

// RequestHash binds a durable key to the exact operation and JSON body, not a
// transport request ID. JSON object keys are sorted, insignificant whitespace
// removed and numbers retained exactly; optional extensions participate too.
func RequestHash(m Message) (string, error) {
	if m.Kind != "request" || !operations[m.Op].Durable {
		return "", refuse("invalid_message", "not a durable request")
	}
	if err := validateBody(m); err != nil {
		return "", err
	}
	var b any
	d := json.NewDecoder(bytes.NewReader(m.Body))
	d.UseNumber()
	if err := d.Decode(&b); err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Binding   Binding `json:"binding"`
		Target    *Target `json:"target"`
		Operation string  `json:"operation"`
		Body      any     `json:"body"`
	}{m.Binding, m.Target, m.Op, b})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// CheckReceipt correlates a decoded receipt with the receiver's original
// durable request. It does not interpret unknown as permission to retry.
func CheckReceipt(request Message, received Receipt) error {
	hash, err := RequestHash(request)
	if err != nil {
		return err
	}
	if !receipt(received) || received.Key != request.Key || received.Operation != request.Op || received.RequestHash != hash {
		return refuse("key_conflict", "receipt does not match the recorded request")
	}
	return nil
}

// CheckRange correlates a decoded read with the durable ordered read plan.
func CheckRange(request Read, received Bytes) error {
	if !byteRange(received) || request.Source != received.Source || request.Order != received.Order || request.Offset != received.Offset || uint64(len(received.Data)) > uint64(request.Limit) {
		return refuse("invalid_message", "stream range does not match the recorded read")
	}
	return nil
}
