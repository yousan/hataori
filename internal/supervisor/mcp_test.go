package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// captured is the exchange of Claude Code with `hataori mcp`, taken by
// testdata/capture.sh.
const captured = "testdata/claudecode/2.1.328"

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, m)
	}
	return out
}

func toolNames(t *testing.T, result any) []string {
	t.Helper()
	var r struct {
		Tools []struct{ Name string }
	}
	b, _ := json.Marshal(result)
	json.Unmarshal(b, &r)
	var names []string
	for _, x := range r.Tools {
		names = append(names, x.Name)
	}
	return names
}

var four = []string{ToolList, ToolRead, ToolSend, ToolStart}

// What Claude Code sent is answered as it was then: every request gets one
// answer with its id and initialize in the version Claude Code asked for.
// The four captured tools are what Claude Code was given then; the current
// server also offers interrupt_thread.
func TestClaudeCodeToolsList(t *testing.T) {
	client, err := os.ReadFile(filepath.Join(captured, "client.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	m := &MCP{Dial: func() (control.API, error) { return nil, errors.New("no daemon in this test") }, Scope: "C0123", Version: "test"}
	var out bytes.Buffer
	if err := m.Serve(context.Background(), bytes.NewReader(client), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		id, _ := json.Marshal(r["id"])
		got[string(id)] = r
	}
	want := map[string]map[string]any{}
	for _, r := range lines(t, filepath.Join(captured, "server.jsonl")) {
		id, _ := json.Marshal(r["id"])
		want[string(id)] = r
	}
	var requests int
	for _, c := range lines(t, filepath.Join(captured, "client.jsonl")) {
		if c["id"] == nil {
			continue
		}
		requests++
		id, _ := json.Marshal(c["id"])
		r, ok := got[string(id)]
		if !ok {
			t.Fatalf("no answer to %v", c)
		}
		switch c["method"] {
		case "initialize":
			asked := c["params"].(map[string]any)["protocolVersion"]
			if v := r["result"].(map[string]any)["protocolVersion"]; v != asked {
				t.Fatalf("initialize answered %v, Claude Code asked for %v", v, asked)
			}
		case "tools/list":
			if names := toolNames(t, r["result"]); !slices.Equal(names, []string{ToolList, ToolRead, ToolSend, ToolInterrupt, ToolStart}) {
				t.Fatalf("tools/list gives %v, want the five current tools", names)
			}
			// Compare the tools Claude Code was given in the capture.
			result := r["result"].(map[string]any)
			var capturedTools []any
			for _, item := range result["tools"].([]any) {
				if item.(map[string]any)["name"] != ToolInterrupt {
					capturedTools = append(capturedTools, item)
				}
			}
			a, _ := json.Marshal(map[string]any{"tools": capturedTools})
			b, _ := json.Marshal(want[string(id)]["result"])
			if !bytes.Equal(a, b) {
				t.Fatalf("tools/list changed since the capture:\n%s\n%s", a, b)
			}
		default:
			if r["error"] == nil {
				t.Fatalf("%v answered %v, want an error", c["method"], r)
			}
		}
	}
	if len(got) != requests {
		t.Fatalf("%d answers to %d requests", len(got), requests)
	}
	var init struct {
		MCPServers []struct{ Name, Status string } `json:"mcp_servers"`
		Tools      []string
	}
	b, _ := os.ReadFile(filepath.Join(captured, "init.json"))
	if err := json.Unmarshal(b, &init); err != nil {
		t.Fatal(err)
	}
	var prefixed []string
	for _, n := range four {
		prefixed = append(prefixed, "mcp__hataori__"+n)
	}
	if len(init.MCPServers) != 1 || init.MCPServers[0].Status != "connected" || !slices.Equal(init.Tools, prefixed) {
		t.Fatalf("Claude Code's init message: %+v", init)
	}
}

// fakeAPI answers Threads and Thread with one thread whose output is out.
type fakeAPI struct {
	control.API
	out    string
	recent int
}

var fakeID = control.ThreadID(7)

func (f *fakeAPI) Threads(_ context.Context, flt control.Filter) ([]control.ThreadInfo, error) {
	if flt.Thread != 0 && flt.Thread != fakeID {
		return nil, nil
	}
	return []control.ThreadInfo{{ID: fakeID, Scope: "C0123", Title: "Fix it", State: control.Idle}}, nil
}

func (f *fakeAPI) Thread(_ context.Context, id control.ThreadID, recent, _ int) (control.ThreadDetail, error) {
	f.recent = recent
	out := f.out
	if len(out) > recent {
		out = out[len(out)-recent:]
	}
	return control.ThreadDetail{ThreadInfo: control.ThreadInfo{ID: id, Scope: "C0123", Title: "Fix it", State: control.Idle}, Recent: out}, nil
}

func call(t *testing.T, m *MCP, name string, args string) (string, bool) {
	t.Helper()
	text, err := m.call(context.Background(), name, json.RawMessage(args))
	if err != nil {
		return err.Error(), true
	}
	return text, false
}

// read_thread returns at most the configured bytes of output, and says
// when it cut it; it is not cut when it fits.
func TestReadThreadIsCapped(t *testing.T) {
	api := &fakeAPI{out: strings.Repeat("x", 90) + "THE END"}
	m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123", OutputMax: 20}
	text, isErr := call(t, m, ToolRead, `{"number":7}`)
	if isErr {
		t.Fatal(text)
	}
	if !strings.Contains(text, "cut: only its last 20 bytes are shown") || !strings.HasSuffix(text, strings.Repeat("x", 13)+"THE END") {
		t.Fatalf("read_thread:\n%s", text)
	}
	if strings.Contains(text, strings.Repeat("x", 14)) {
		t.Fatalf("read_thread gives more than 20 bytes of output:\n%s", text)
	}
	m.OutputMax = 200
	text, _ = call(t, m, ToolRead, `{"number":7}`)
	if strings.Contains(text, "cut") || !strings.HasSuffix(text, api.out) {
		t.Fatalf("read_thread cut what fits:\n%s", text)
	}
	if text, isErr := call(t, m, ToolRead, `{"number":8}`); !isErr || !strings.HasPrefix(text, "not in this scope") {
		t.Fatalf("read_thread of a number not in the scope: %q", text)
	}
}

// A cut never splits a character.
func TestReadThreadCutsAtACharacter(t *testing.T) {
	api := &fakeAPI{out: strings.Repeat("あ", 10)} // 3 bytes each
	m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123", OutputMax: 10}
	text, _ := call(t, m, ToolRead, `{"number":7}`)
	if !strings.HasSuffix(text, "\n"+strings.Repeat("あ", 3)) {
		t.Fatalf("read_thread:\n%q", text)
	}
}
