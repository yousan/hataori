package workerproto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write goldens for a new worker protocol version only")

var registration = Binding{Main: "main-A", Worker: "worker-A"}
var target = Target{Thread: 7, Session: "session-A", Run: "run-A"}
var src = Source{Handle: "source-handle", Source: "events", Generation: "generation-A"}
var agentConfig = AgentConfig{Agent: "fake", Config: "default"}

func allCaps() []string {
	set := make(map[string]bool)
	for _, op := range Operations() {
		set[op.Capability] = true
	}
	var caps []string
	for c := range set {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	return caps
}

func hello() Hello {
	return Hello{Version: Version, Binding: registration, Capabilities: allCaps()}
}

func connection(t *testing.T) *Connection {
	t.Helper()
	c, err := Negotiate(registration, hello(), hello())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func examples(t *testing.T) map[string]any {
	t.Helper()
	fileSum := sha256.Sum256([]byte("abc"))
	fileDigest := hex.EncodeToString(fileSum[:])
	requests := map[string]any{
		"health": Empty{}, "inventory": Empty{}, "probe": agentConfig,
		"prepare":  Prepare{Instructions: "Answer in English.", Policy: []string{"policy-A"}},
		"end_host": Empty{},
		"start":    Start{AgentConfig: agentConfig, Holder: "own", Workspace: "workspace-A", Model: "default"},
		"stop":     Stop{Turn: "turn-A"}, "input": Input{Turn: "turn-A", Data: []byte("Run tests.\n")},
		"answer":  Answer{Turn: "turn-A", Request: "request-A", Data: []byte("yes")},
		"receipt": ReceiptQuery{Operation: "input", Key: "key-input"},
		"attach":  Attachment{Handle: "attachment-A", Cols: 80, Rows: 24},
		"keys":    Keys{Handle: "attachment-A", Data: []byte("x")},
		"resize":  Attachment{Handle: "attachment-A", Cols: 120, Rows: 40},
		"detach":  Handle{Handle: "attachment-A"}, "follow": Follow{Source: "events"},
		"read":            Read{Source: src, Order: 1, Offset: 0, Limit: MaxChunk},
		"history_release": HistoryRelease{Retired: true}, "clone": Clone{Repository: "repo-A"},
		"fetch": Workspace{Workspace: "workspace-A"}, "inspect": Workspace{Workspace: "workspace-A"},
		"remove": Workspace{Workspace: "workspace-A"}, "restore": Trash{Trash: "trash-A"},
		"acknowledge": Trash{Trash: "trash-A"}, "purge": Trash{Trash: "trash-A"},
		"upload":       Upload{Handle: "upload-A", Digest: fileDigest, Size: 3, Data: []byte("abc"), Final: true},
		"download":     Download{Handle: "outgoing-A", Offset: 0, Limit: MaxChunk},
		"login_start":  Login{AgentConfig: agentConfig, Attempt: "attempt-A"},
		"login_input":  LoginInput{Attempt: "attempt-A", Data: []byte("synthetic-login-input")},
		"login_cancel": LoginAttempt{Attempt: "attempt-A"},
	}
	replies := map[string]any{
		"health":    Health{Boot: "boot-A", Ready: true, Agents: []AgentHealth{{AgentConfig: agentConfig, Installed: true, Login: "logged_in", Installation: "installed"}}, Telemetry: Telemetry{ObservedMS: 1000, ValidMS: 5000, Known: true, MemoryFree: 1024, DiskFree: 2048, Load: 0.5}},
		"inventory": Inventory{Boot: "boot-A", Complete: true, Hosts: []HostRecord{{Target: target, State: "running", Turn: "turn-A", Hooks: []string{"request-A"}}}},
		"probe":     AgentHealth{AgentConfig: agentConfig, Installed: true, Login: "logged_out", Installation: "installed"},
		"attach":    Attachment{Handle: "attachment-A", Cols: 80, Rows: 24},
		"keys":      Empty{}, "resize": Empty{}, "detach": Empty{}, "follow": src,
		"read":        Bytes{Source: src, Order: 1, Next: 3, State: "data", Data: []byte("abc")},
		"inspect":     Inspection{Workspace: "workspace-A", Identity: "directory-A", Clean: false, Reasons: []string{"untracked files"}},
		"download":    FileBytes{Handle: "outgoing-A", Digest: fileDigest, Size: 3, Data: []byte("abc"), Final: true},
		"login_start": LoginAttempt{Attempt: "attempt-A"}, "login_input": LoginAttempt{Attempt: "attempt-A"}, "login_cancel": LoginAttempt{Attempt: "attempt-A"},
	}
	ex := map[string]any{
		"hello.json":              hello(),
		"hello-negotiated.json":   connection(t).Hello(),
		"hello-required.json":     Hello{Version: Version, Binding: registration, Capabilities: allCaps(), Required: []string{"receipts"}},
		"hello-incompatible.json": VersionRefusal{Local: Version, Peer: "2.0", Refusal: Refusal{Code: "incompatible_major", Message: "protocol majors differ"}},
	}
	msgs := make(map[string]Message)
	for _, op := range Operations() {
		b, ok := requests[op.Name]
		if !ok {
			t.Fatalf("missing request example for %s", op.Name)
		}
		m := Message{Kind: "request", ID: "request-" + op.Name, Binding: registration, Op: op.Name, Body: raw(t, b)}
		if op.Session {
			copy := target
			m.Target = &copy
		}
		if op.Durable {
			m.Key = "key-" + op.Name
		}
		msgs[op.Name] = m
		ex["request-"+op.Name+".json"] = m
	}
	for _, op := range Operations() {
		m := msgs[op.Name]
		m.Kind = "reply"
		if op.Durable || op.Name == "receipt" {
			original := msgs[op.Name]
			if op.Name == "receipt" {
				original = msgs["input"]
			}
			hash, err := RequestHash(original)
			if err != nil {
				t.Fatal(err)
			}
			effect := Effect{}
			switch op.Name {
			case "start":
				effect.Host = "host-A"
			case "clone", "restore":
				effect.Workspace = "workspace-A"
			case "remove":
				effect.Trash = "trash-A"
			case "upload":
				effect.Handle, effect.Digest, effect.Size = "upload-A", fileDigest, 3
			}
			r := Receipt{Operation: original.Op, Key: original.Key, RequestHash: hash, State: "complete", Result: &effect}
			m.Body = raw(t, r)
			ex["reply-"+op.Name+".json"] = m
			for _, state := range []string{"pending", "unknown", "refused"} {
				r.State, r.Result = state, nil
				if state == "refused" {
					r.Refusal = &Refusal{Code: "denied", Message: "operation refused before effects"}
				}
				m.Body = raw(t, r)
				ex["reply-"+op.Name+"-"+state+".json"] = m
			}
		} else {
			b, ok := replies[op.Name]
			if !ok {
				t.Fatalf("missing reply example for %s", op.Name)
			}
			m.Body = raw(t, b)
			ex["reply-"+op.Name+".json"] = m
		}
		m.Kind = "refusal"
		m.Body = raw(t, Refusal{Code: "not_ready", Message: "reconciliation is not complete"})
		ex["refusal-"+op.Name+".json"] = m
	}
	for _, code := range RefusalCodes() {
		m := msgs["input"]
		m.Kind, m.Body = "refusal", raw(t, Refusal{Code: code, Message: "safe refusal explanation"})
		ex["refusal-code-"+code+".json"] = m
	}
	unknown := msgs["health"]
	unknown.Kind, unknown.Op = "refusal", "future"
	unknown.Body = raw(t, Refusal{Code: "unsupported_operation", Message: "operation is not supported"})
	ex["refusal-unknown-operation.json"] = unknown
	unnegotiated := msgs["input"]
	unnegotiated.Kind = "refusal"
	unnegotiated.Body = raw(t, Refusal{Code: "unsupported_capability", Message: "operation capability was not negotiated"})
	ex["refusal-unnegotiated-capability.json"] = unnegotiated
	for _, op := range []string{"read", "attach"} {
		for _, state := range []string{"data", "empty", "unavailable", "missing", "unreadable", "replaced", "truncated", "history_lost"} {
			b := Bytes{Source: src, Order: 1, State: state}
			if state == "data" {
				b.Next, b.Data = 3, []byte("abc")
			}
			m := msgs[op]
			m.Kind, m.Body = "stream", raw(t, b)
			ex["stream-"+op+"-"+state+".json"] = m
		}
	}
	for _, state := range []string{"data", "complete", "cancelled", "expired"} {
		b := LoginStream{Attempt: "attempt-A", State: state}
		if state == "data" {
			b.Data = []byte("synthetic-login-link")
		}
		m := msgs["login_start"]
		m.Kind, m.Body = "stream", raw(t, b)
		ex["stream-login-"+state+".json"] = m
	}
	return ex
}

func TestGolden(t *testing.T) {
	ex := examples(t)
	dir := filepath.Join("testdata", "protocol", Version)
	if *update {
		manifest, err := os.ReadFile("testdata/protocol/SUMS")
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(manifest), "\n") {
			if strings.HasSuffix(line, "  "+Version) {
				t.Fatal("a frozen worker version cannot be updated; choose a new version")
			}
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, value := range ex {
			data, err := Encode(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		sum := dirSum(t, dir)
		manifest = append(manifest, []byte(fmt.Sprintf("%s  %s\n", sum, Version))...)
		if err := os.WriteFile("testdata/protocol/SUMS", manifest, 0644); err != nil {
			t.Fatal(err)
		}
	}
	c := connection(t)
	for name, value := range ex {
		t.Run(name, func(t *testing.T) {
			got, err := Encode(value)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("encoding changed: choose a new worker version and keep the old goldens")
			}
			if m, ok := value.(Message); ok {
				if _, err := c.Decode(want, m.Target); err != nil {
					t.Fatalf("golden does not decode: %v", err)
				}
			}
		})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(ex) {
		t.Fatal("golden set differs from the defined examples")
	}
}

func dirSum(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, entry := range entries { // ReadDir sorts by name.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			t.Fatal("unexpected golden entry")
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", entry.Name(), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestFrozenVersions(t *testing.T) {
	manifest, err := os.ReadFile("testdata/protocol/SUMS")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !digest(fields[0]) || seen[fields[1]] {
			t.Fatal("invalid or duplicate worker SUMS entry")
		}
		v := fields[1]
		if _, _, ok := version(v); !ok {
			t.Fatal("invalid frozen version")
		}
		seen[v] = true
		if got := dirSum(t, filepath.Join("testdata", "protocol", v)); got != fields[0] {
			t.Fatalf("worker protocol %s goldens changed without a version change", v)
		}
	}
	if !seen[Version] {
		t.Fatal("current worker version is not frozen")
	}
	entries, err := os.ReadDir("testdata/protocol")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && !seen[entry.Name()] {
			t.Fatal("unfrozen worker version directory")
		}
	}
}
