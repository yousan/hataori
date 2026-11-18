package wire

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
	"time"

	"github.com/yousan/hataori/internal/control"
)

var update = flag.Bool("update", false, "write the golden files of a new protocol version")

// GoldenDir is where the golden files of the current version are.
var GoldenDir = filepath.Join("testdata", "protocol", Version)

const sumsFile = "testdata/protocol/SUMS"

var (
	exampleTime = time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	exampleID   = control.ThreadID{Scope: "C0123", Key: "T0456"}
	exampleInfo = control.ThreadInfo{ID: exampleID, Number: 7, Title: "Fix the build", Repo: "https://example.com/repo.git",
		State: control.Running, StartedBy: "webhook:W1", Created: exampleTime, Updated: exampleTime,
		Agent: "claude-code", Model: "opus", Effort: "high", Turns: 12, Last: "Running the tests...", Background: 1,
		Context: &control.Context{Used: 45000, Size: 200000}, Tokens: &control.TokenCount{Turn: 2450, Today: 98000}}
)

// eventData is an example of every event kind's data.
var eventData = map[control.EventKind]string{
	"thread_created":    `{"effort":"high","from":"discord:W1","model":"opus","principal":"dispatch","repo":"https://example.com/repo.git","session":3,"thread":7}`,
	"state":             `{"detail":"last lines of the terminal","from":"running","reason":"interrupted","session":3,"to":"asleep"}`,
	"turn_started":      `{"by":"caller","from":"discord:W1","key":"M0789","principal":"dispatch","session":3,"turn":12}`,
	"turn_ended":        `{"background":0,"by":"caller","from":"discord:W1","outcome":"failure","principal":"dispatch","reason":"limit","session":3,"turn":12}`,
	"input":             `{"session":3,"text":"Run the tests.","turn":12}`,
	"answer":            `{"session":3,"text":"blue","turn":13}`,
	"text":              `{"final":true,"session":3,"text":"All tests pass."}`,
	"tool_started":      `{"id":"toolu_1","parent":"agent_1","session":3,"tool":"Bash"}`,
	"tool_ended":        `{"id":"toolu_1","session":3,"tool":"Bash"}`,
	"subagent_started":  `{"id":"agent_1","name":"general-purpose","session":3}`,
	"subagent_ended":    `{"id":"agent_1","session":3}`,
	"request":           `{"approval":false,"choices":["red","blue"],"request":"3:toolu_2","session":3,"text":"Which colour?"}`,
	"request_withdrawn": `{"request":"3:toolu_2","session":3}`,
	"file_out":          `{"caption":"The report and its data.","file":"/srv/hataori/state/sessions/3/out/report.pdf","more":["/srv/hataori/state/sessions/3/out/data.csv"],"session":3}`,
	"usage":             `{"cache_read":1200,"cache_write":300,"context_size":200000,"context_used":45000,"input":800,"model":"opus","output":150,"session":3,"turn":12}`,
	"login_lost":        `{"agent":"claude-code","outage":1,"thread":7}`,
	"login_restored":    `{"agent":"claude-code","outage":1,"scopes":["C0123"],"threads":[{"scope":"C0123","key":"T0456"}]}`,
	"session_replaced":  `{"new_session":4,"reason":"cannot resume","session":3}`,
	control.CaughtUp:    ``,
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// examples is one encoded example of every message, of every operation's
// request and response, and of every event kind, by golden file name.
func examples(t *testing.T) map[string]any {
	t.Helper()
	ex := map[string]any{
		"hello.json":         hello{Version: Version},
		"hello-refusal.json": hello{Version: Version, Error: versionError("2.0", Version).Error()},
		"response-error.json": response{ID: 5, Error: &wireError{Code: "not_supported",
			Message: `wire: operation "Unheard": not supported`}},
	}
	args := map[string]any{
		"Threads": threadsArgs{Filter: control.Filter{Scope: "C0123"}},
		"Thread":  threadArgs{ID: exampleID, Recent: 2000, Events: 5},
		"Start": control.StartRequest{Key: "M0789", Scope: "C0123", Repo: "https://example.com/repo.git",
			Input: control.Input{Text: "Fix the build."}, Model: "opus", Effort: "high"},
		"Send":       sendArgs{ID: exampleID, Key: "M0790", Input: control.Input{Text: "And the docs."}, Busy: control.Deliver},
		"Answer":     answerArgs{Request: "3:toolu_2", Answer: control.Answer{Text: "blue", Choices: []string{"blue"}}},
		"Stop":       stopArgs{ID: exampleID},
		"Events":     eventsArgs{Filter: control.Filter{}, After: 41},
		"Workspace":  workspaceArgs{ID: exampleID},
		"SignInLink": signInLinkArgs{},
		"Terminal":   terminalArgs{ID: exampleID, Run: "5f0c9a1e7b3d4c2a8e6f1b0d9c7a5e3f", After: 1024},
	}
	results := map[string]any{
		"Threads": []control.ThreadInfo{exampleInfo},
		"Thread": control.ThreadDetail{ThreadInfo: exampleInfo, Recent: "Running the tests...",
			Requests: []control.Request{{ID: "3:toolu_2", Kind: control.Question, Text: "Which colour?",
				Choices: []control.Choice{{Label: "red", Token: "red"}, {Label: "blue", Token: "blue"}}}},
			Events: []control.Event{{Pos: 42, Thread: exampleID, Time: exampleTime, Kind: "text",
				Data: raw(`{"final":false,"session":3,"text":"Running the tests..."}`)}}},
		"Start": exampleID,
		"Workspace": control.WorkspaceState{Branch: "fix-build", Base: "9fceb02d0ae598e95dc970b74767f19372d61af8",
			Added: 12, Removed: 3, Uncommitted: 2, Unpushed: 1, Fetched: &exampleTime},
		"SignInLink": "https://dashboard.example.com/signin?t=Q2hhbmdlIG1lIGV2ZXJ5IHRpbWU",
	}
	ex["terminal.json"] = response{ID: 1, Terminal: &control.TerminalChunk{Run: "5f0c9a1e7b3d4c2a8e6f1b0d9c7a5e3f",
		Pos: 1046, Reset: false, Cols: 120, Rows: 40, Data: []byte("\x1b[32mok\x1b[0m tests pass\r\n")}}
	for op := range operations {
		a, ok := args[op]
		if !ok {
			t.Fatalf("no example of %s's arguments", op)
		}
		ex["request-"+op+".json"] = request{ID: 1, Op: op, Args: mustJSON(a)}
		resp := response{ID: 1}
		if r, ok := results[op]; ok {
			resp.Result = mustJSON(r)
		}
		ex["response-"+op+".json"] = resp
	}
	for kind := range control.EventKinds {
		d, ok := eventData[kind]
		if !ok {
			t.Fatalf("no example of the event %s", kind)
		}
		e := control.Event{Pos: 42, Thread: exampleID, Time: exampleTime, Kind: kind}
		if d != "" {
			e.Data = raw(d)
		}
		if kind == control.CaughtUp {
			e.Thread = control.ThreadID{}
		} else if err := control.CheckEvent(e); err != nil {
			t.Fatalf("the example of %s: %v", kind, err)
		}
		// Every field of the kind is in its example, optional ones too, so
		// that a change to any field changes a golden file.
		var m map[string]any
		json.Unmarshal(e.Data, &m)
		for _, f := range control.EventKinds[kind] {
			if _, ok := m[f.Name]; !ok {
				t.Fatalf("the example of %s has no field %s", kind, f.Name)
			}
		}
		ex["event-"+string(kind)+".json"] = response{ID: 1, Event: &e}
	}
	return ex
}

// encode is a message as it goes over the socket.
func encode(v any) []byte {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(v)
	return b.Bytes()
}

// dirSum is the checksum of a version's golden files.
func dirSum(t *testing.T, dir string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(f), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readSums(t *testing.T) map[string]string {
	t.Helper()
	sums := map[string]string{}
	b, err := os.ReadFile(sumsFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if v, s, ok := strings.Cut(l, " "); ok {
			sums[v] = s
		}
	}
	return sums
}

// The encodings are those of the golden files of this version, and no
// version's golden files changed since its checksum was recorded.
func TestGolden(t *testing.T) {
	ex := examples(t)
	sums := readSums(t)
	if *update {
		if _, recorded := sums[Version]; recorded {
			t.Fatalf("version %s is recorded in %s: change the version before writing new golden files", Version, sumsFile)
		}
		os.MkdirAll(GoldenDir, 0o755)
		for name, v := range ex {
			if err := os.WriteFile(filepath.Join(GoldenDir, name), encode(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		f, err := os.OpenFile(sumsFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		sums[Version] = dirSum(t, GoldenDir)
		fmt.Fprintf(f, "%s %s\n", Version, sums[Version])
		f.Close()
	}
	files, _ := filepath.Glob(filepath.Join(GoldenDir, "*.json"))
	if len(files) != len(ex) {
		t.Errorf("%d golden files in %s, %d examples", len(files), GoldenDir, len(ex))
	}
	for name, v := range ex {
		want, err := os.ReadFile(filepath.Join(GoldenDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := encode(v); !bytes.Equal(got, want) {
			t.Errorf("%s: the encoding changed in version %s:\n got %s\nwant %s", name, Version, got, want)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join("testdata", "protocol", "*.*"))
	for _, d := range dirs {
		v := filepath.Base(d)
		if got := dirSum(t, d); sums[v] != got {
			t.Errorf("the golden files of version %s differ from their checksum in %s: a version's encodings never change; change the version", v, sumsFile)
		}
	}
	if _, ok := sums[Version]; !ok {
		t.Errorf("version %s has no checksum in %s", Version, sumsFile)
	}
}

// Every golden message decodes and encodes again to the same bytes: the
// file is what the code reads as well as what it writes.
func TestGoldenRoundTrip(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(GoldenDir, "*.json"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var v any = &response{}
		switch name := filepath.Base(f); {
		case strings.HasPrefix(name, "hello"):
			v = &hello{}
		case strings.HasPrefix(name, "request-"):
			v = &request{}
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if got := encode(v); !bytes.Equal(got, b) {
			t.Errorf("%s: decoded and encoded again:\n got %s\nwant %s", f, got, b)
		}
	}
}
