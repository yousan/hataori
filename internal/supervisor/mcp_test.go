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
	"time"

	"github.com/yousan/hataori/internal/control"
)

// captured is the exchange of Claude Code with `hataori mcp`, taken by
// testdata/capture.sh.
const captured = "testdata/claudecode/2.1.332"

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

// Listed is called once, after the first answer to tools/list was written,
// and not for one that asks for anything else.
func TestListedAfterTheFirstToolsList(t *testing.T) {
	client, err := os.ReadFile(filepath.Join(captured, "client.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var at []string // what was answered when it was called
	m := &MCP{Dial: func() (control.API, error) { return nil, errors.New("no daemon in this test") }, Scope: "C0123", Version: "test",
		Listed: func() { at = append(at, out.String()) }}
	if err := m.Serve(context.Background(), bytes.NewReader(bytes.Repeat(client, 2)), &out); err != nil {
		t.Fatal(err)
	}
	if len(at) != 1 || !strings.HasSuffix(at[0], "\n") || !strings.Contains(at[0][strings.LastIndex(at[0][:len(at[0])-1], "\n")+1:], `"tools":[`) {
		t.Fatalf("called %d times, after %.400q", len(at), at)
	}
}

// What Claude Code sent is answered as it was then: every request gets one
// answer with its id, initialize in the version Claude Code asked for and
// with the server's instructions (#653), and tools/list with the tools
// Claude Code was given then.
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
			if got, was := r["result"].(map[string]any)["instructions"], want[string(id)]["result"].(map[string]any)["instructions"]; got != was || got != Instructions {
				t.Fatalf("the instructions changed since the capture:\n%v\n%v", got, was)
			}
		case "tools/list":
			current := []string{ToolList, ToolRead, ToolSend, ToolDefer, ToolDeferred, ToolCancelDeferred, ToolInterrupt, ToolStart, ToolIssues, ToolIssue, ToolPulls, ToolPull, ToolChecks, ToolDeployed, ToolRemind, ToolCancelRemind, ToolFiles, ToolFile, ToolWrite, ToolEdit}
			if names := toolNames(t, r["result"]); !slices.Equal(names, current) {
				t.Fatalf("tools/list gives %v, want the current tools %v", names, current)
			}
			// Keep the external capture immutable. Placement adds one optional
			// property; research adds an explicit capability qualification. All
			// other captured fields still match, including the original wording.
			for _, tool := range r["result"].(map[string]any)["tools"].([]any) {
				x := tool.(map[string]any)
				if x["name"] == ToolStart {
					props := x["inputSchema"].(map[string]any)["properties"].(map[string]any)
					machine, ok := props["machine"].(map[string]any)
					if !ok || machine["type"] != "string" {
						t.Fatal("start lacks typed machine option")
					}
					delete(props, "machine")
					research := props["research"].(map[string]any)
					description := research["description"].(string)
					if !strings.HasSuffix(description, researchCapabilityNote) {
						t.Fatal("research lacks capability qualification")
					}
					research["description"] = strings.TrimSuffix(description, researchCapabilityNote)
				}
			}

			// New opt-in tools have their own schema/dispatch regression. The
			// actual external exchange remains immutable for existing tools.
			ts := r["result"].(map[string]any)["tools"].([]any)
			ts = slices.DeleteFunc(ts, func(v any) bool {
				n := v.(map[string]any)["name"]
				return n == ToolDefer || n == ToolDeferred || n == ToolCancelDeferred
			})
			r["result"].(map[string]any)["tools"] = ts
			a, _ := json.Marshal(r["result"])
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
	for _, x := range tools {
		if x.Name == ToolDefer || x.Name == ToolDeferred || x.Name == ToolCancelDeferred {
			continue
		}
		prefixed = append(prefixed, "mcp__hataori__"+x.Name)
	}
	slices.Sort(prefixed)
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

// list_threads lists the open threads, counts the others by state, and lists
// every one with state all (#796).
func TestListThreadsListsTheOpenOnes(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	api := &listAPI{}
	for i, th := range []struct {
		state control.State
		ago   time.Duration
	}{
		{control.Running, time.Minute},
		{control.Idle, 72 * time.Hour},
		{control.Waiting, 72 * time.Hour},
		{control.Failed, 72 * time.Hour},
		{control.Closed, time.Hour},
		{control.Removed, time.Hour},
		{control.Asleep, time.Hour},
		{control.Asleep, 48 * time.Hour},
	} {
		api.threads = append(api.threads, control.ThreadInfo{ID: control.ThreadID(i + 1), Scope: "C0123", Title: "Work", State: th.state, Updated: now.Add(-th.ago)})
	}
	m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123", OutputMax: 100, Now: func() time.Time { return now }}
	numbers := func(text string) (ns []string) {
		for _, l := range strings.Split(text, "\n") {
			if n, _, ok := strings.Cut(l, " "); ok && strings.HasPrefix(n, "#") {
				ns = append(ns, n)
			}
		}
		return ns
	}
	for _, args := range []string{`{}`, `{"state":"open"}`} {
		text, isErr := call(t, m, ToolList, args)
		if isErr || !strings.HasPrefix(text, "5 open threads in this channel:\n") {
			t.Fatalf("list_threads %s: %s", args, text)
		}
		if got, want := numbers(text), []string{"#1", "#2", "#3", "#4", "#7"}; !slices.Equal(got, want) {
			t.Errorf("list_threads %s lists %v, want %v", args, got, want)
		}
		ls := strings.Split(strings.TrimSpace(text), "\n")
		if got, want := ls[len(ls)-1], "Others, not listed: 1 asleep, 1 closed, 1 removed; state all lists them."; got != want {
			t.Errorf("list_threads %s ends with %q, want %q", args, got, want)
		}
	}
	text, isErr := call(t, m, ToolList, `{"state":"all"}`)
	if want := []string{"#1", "#2", "#3", "#4", "#5", "#6", "#7", "#8"}; isErr || !slices.Equal(numbers(text), want) {
		t.Errorf("list_threads of all:\n%s", text)
	}
	if text, isErr := call(t, m, ToolRead, `{"number":8}`); isErr || !strings.HasPrefix(text, "Thread #8: Work\n") {
		t.Errorf("read_thread of a thread list_threads leaves out: %s", text)
	}
	for _, s := range []string{"closed", "OPEN", " "} {
		if text, isErr := call(t, m, ToolList, `{"state":"`+s+`"}`); !isErr || text != "bad arguments: state is open or all" {
			t.Errorf("list_threads of state %q: %q, want it refused", s, text)
		}
	}
	// Asleep for exactly a day is not open; a moment less is.
	for ago, want := range map[time.Duration]string{
		24 * time.Hour:                 "0 open threads in this channel:\nOthers, not listed: 1 asleep; state all lists them.\n",
		24*time.Hour - time.Nanosecond: "1 open threads in this channel:\n#1 ",
	} {
		api.threads = []control.ThreadInfo{{ID: 1, Scope: "C0123", Title: "Work", State: control.Asleep, Updated: now.Add(-ago)}}
		if text, _ := call(t, m, ToolList, `{}`); !strings.HasPrefix(text, want) {
			t.Errorf("list_threads with a thread asleep for %s: %q", ago, text)
		}
	}
}

// listAPI answers Threads and Thread with threads.
type listAPI struct {
	control.API
	threads []control.ThreadInfo
}

func (l *listAPI) Threads(_ context.Context, flt control.Filter) ([]control.ThreadInfo, error) {
	if flt.Thread == 0 {
		return l.threads, nil
	}
	for _, t := range l.threads {
		if t.ID == flt.Thread {
			return []control.ThreadInfo{t}, nil
		}
	}
	return nil, nil
}

func (l *listAPI) Thread(_ context.Context, id control.ThreadID, _, _ int) (control.ThreadDetail, error) {
	for _, t := range l.threads {
		if t.ID == id {
			return control.ThreadDetail{ThreadInfo: t}, nil
		}
	}
	return control.ThreadDetail{}, errors.New("no such thread")
}

// A number below 1 names no thread and is not looked up, and what a lookup
// returned is used only when it is the number asked for (#570).
func TestOnlyTheThreadAskedForIsUsed(t *testing.T) {
	api := &otherAPI{}
	m := &MCP{Dial: func() (control.API, error) { return api, nil }, Scope: "C0123"}
	for _, name := range []string{ToolRead, ToolSend, ToolInterrupt} {
		for _, n := range []string{"0", "-1", "-9223372036854775808", "8"} {
			text, isErr := call(t, m, name, `{"number":`+n+`,"text":"hello","reason":"urgent"}`)
			if want := "not in this scope: no thread #" + n + " in this channel"; !isErr || text != want {
				t.Errorf("%s of number %s: error %v, %q; want %q", name, n, isErr, text, want)
			}
		}
	}
	if len(api.asked) != 3 { // 8, once by each tool
		t.Errorf("threads were looked up for %v", api.asked)
	}
}

// otherAPI answers Threads with thread 7, whatever the filter; a call on a
// thread panics, as the API it embeds is nil.
type otherAPI struct {
	control.API
	asked []control.ThreadID
}

func (o *otherAPI) Threads(_ context.Context, flt control.Filter) ([]control.ThreadInfo, error) {
	o.asked = append(o.asked, flt.Thread)
	return []control.ThreadInfo{{ID: fakeID, Scope: "C0123", Title: "Fix it", State: control.Idle}}, nil
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
