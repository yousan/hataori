package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

type codexWire struct {
	Dir string         `json:"dir"`
	Msg map[string]any `json:"msg"`
}

const deviceLink = "https://auth.example.invalid/codex/device"
const deviceCode = "TE57-7F3AQ"
const codexLoggedFile = "hataori-test-logged-in"
const codexSignedInFile = "hataori-test-signed-in"

func codexLoggedIn() bool {
	_, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), codexLoggedFile))
	return err == nil
}

func codexLogin(args []string) int {
	capture := os.Getenv("HATAORI_TEST_CODEX_CAPTURE")
	if capture == "" {
		fmt.Fprintln(os.Stderr, "codex replay: capture environment variable is required")
		return 2
	}
	if args[0] == "--version" {
		b, err := os.ReadFile(filepath.Join(capture, "VERSION"))
		if err != nil {
			return 2
		}
		fmt.Fprintln(os.Stdout, strings.Split(string(b), "\n")[0])
		return 0
	}
	if len(args) > 1 && args[1] == "status" {
		if codexLoggedIn() {
			fmt.Fprintln(os.Stderr, "Logged in using ChatGPT")
			return 0
		}
		fmt.Fprintln(os.Stderr, "Not logged in")
		return 1
	}
	if len(args) > 1 && args[1] == "--device-auth" {
		f, err := os.Open(filepath.Join(capture, "login_device/output.jsonl"))
		if err != nil {
			return 2
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var row struct{ Stream, Line string }
			if json.Unmarshal(scanner.Bytes(), &row) != nil {
				return 2
			}
			line := strings.ReplaceAll(strings.ReplaceAll(row.Line, "https://auth.openai.com/codex/device", deviceLink), "XX00-0X00X", deviceCode)
			if row.Stream == "stderr" {
				fmt.Fprintln(os.Stderr, line)
			} else {
				fmt.Fprintln(os.Stdout, line)
			}
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), codexSignedInFile)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "Error logging in: device auth timed out after 15 minutes")
				return 1
			}
			time.Sleep(10 * time.Millisecond)
		}
		if os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), codexLoggedFile), nil, 0600) != nil {
			return 1
		}
		fmt.Fprintln(os.Stderr, "Successfully logged in")
		return 0
	}
	fmt.Fprintln(os.Stderr, "codex replay: unsupported login command")
	return 2
}

func codexReplay(args []string) int {
	capture, record := os.Getenv("HATAORI_TEST_CODEX_CAPTURE"), os.Getenv("HATAORI_TEST_CODEX_RECORD")
	if capture == "" || record == "" {
		fmt.Fprintln(os.Stderr, "codex replay: capture and record environment variables are required")
		return 2
	}
	var names []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		names = append(names, k)
	}
	os.WriteFile(record, []byte(strings.Join(names, "\n")), 0600)
	read := func(path string) []codexWire {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		var lines []codexWire
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 4096), 4<<20)
		for s.Scan() {
			var l codexWire
			if json.Unmarshal(s.Bytes(), &l) == nil {
				lines = append(lines, l)
			}
		}
		return lines
	}
	// The scenario selects a capture; the optional wire file records client lines.
	scenario := os.Getenv("HATAORI_TEST_CODEX_SCENARIO")
	if scenario == "" {
		scenario = "start"
	}
	lines := read(filepath.Join(capture, scenario, "wire.jsonl"))
	usage := read(filepath.Join(capture, "usage/wire.jsonl"))
	var rate map[string]any
	for i, l := range usage {
		if l.Dir == "send" && l.Msg["method"] == "account/rateLimits/read" {
			for _, r := range usage[i+1:] {
				if r.Dir == "recv" && r.Msg["id"] == l.Msg["id"] {
					rate = r.Msg
					break
				}
			}
			break
		}
	}
	pos := 0
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		if wire := os.Getenv("HATAORI_TEST_CODEX_WIRE"); wire != "" {
			f, err := os.OpenFile(wire, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return 2
			}
			fmt.Fprintln(f, scanner.Text())
			f.Close()
		}
		var m map[string]any
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			continue
		}
		method, _ := m["method"].(string)
		if method == "thread/start" {
			p, _ := m["params"].(map[string]any)
			cwd, _ := p["cwd"].(string)
			b, _ := json.Marshal(cwd)
			wanted := "projects={" + string(b) + "={trust_level=\"trusted\"}}"
			trusted := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-c" && args[i+1] == wanted {
					trusted = true
				}
			}
			if cwd != "" && !trusted && os.Getenv("CODEX_HOME") != "" {
				f, e := os.OpenFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
				if e == nil {
					fmt.Fprintf(f, "\n[projects.%s]\ntrust_level = \"trusted\"\n", b)
					f.Close()
				}
			}
		}
		if method == "account/rateLimits/read" && rate != nil {
			copy := map[string]any{}
			for k, v := range rate {
				copy[k] = v
			}
			copy["id"] = m["id"]
			out.Encode(copy)
			continue
		}
		if method != "" {
			next := pos
			for next < len(lines) && (lines[next].Dir != "send" || lines[next].Msg["method"] != method) {
				next++
			}
			if next < len(lines) {
				pos = next
			}
		} else if pos < len(lines) && lines[pos].Dir == "send" && lines[pos].Msg["method"] == nil {
			// A server request must receive the captured id and result.
			expected := lines[pos].Msg
			want, _ := json.Marshal([]any{expected["id"], expected["result"]})
			got, _ := json.Marshal([]any{m["id"], m["result"]})
			if !bytes.Equal(want, got) {
				fmt.Fprintf(os.Stderr, "codex replay: response %s, want %s\n", got, want)
				return 3
			}
		}
		if method == "" && (pos >= len(lines) || lines[pos].Dir != "send" || lines[pos].Msg["method"] != nil) {
			fmt.Fprintln(os.Stderr, "codex replay: unexpected response to a server request")
			return 3
		}
		if pos < len(lines) && lines[pos].Dir == "send" && (lines[pos].Msg["method"] == method || (method == "" && lines[pos].Msg["method"] == nil)) {
			requestPos := pos
			pos++
			for pos < len(lines) && lines[pos].Dir == "recv" {
				v := lines[pos].Msg
				if _, ok := v["id"]; ok && v["method"] == nil {
					v["id"] = m["id"]
				}
				out.Encode(v)
				if method == "turn/start" && v["method"] == "turn/started" && !codexLoggedIn() {
					// The failed turn shape comes from the 0.163.0 schema, not a capture.
					params := v["params"].(map[string]any)
					turn := params["turn"].(map[string]any)
					out.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": params["threadId"], "turn": map[string]any{"id": turn["id"], "status": "failed", "error": map[string]any{"message": "unauthorized", "codexErrorInfo": "unauthorized"}}}})
					pos = requestPos
					break
				}
				pos++
			}
			continue
		}
		if id, ok := m["id"]; ok {
			out.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
	return 0
}

func TestCodexStartsAndAnswers(t *testing.T) {
	temp := t.TempDir()
	home := filepath.Join(temp, "home")
	os.Mkdir(home, 0700)
	config := filepath.Join(home, "config.toml")
	original := []byte("# user configuration\n")
	os.WriteFile(config, original, 0600)
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, codexLoggedFile), nil, 0600)
	t.Setenv("HATAORI_TEST_NOT_PASSED", "1")
	capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(temp, "env")
	t.Setenv("HATAORI_TEST_CODEX_CAPTURE", capture)
	t.Setenv("HATAORI_TEST_CODEX_RECORD", record)
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": os.Args[0], "pass_env": []string{"HATAORI_TEST_CODEX_CAPTURE", "HATAORI_TEST_CODEX_RECORD"}}}
	})
	r.deadline = time.Now().Add(30 * time.Second)
	startDaemon(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" hello")
	th := r.thread(t, 0)
	r.posted(t, th, "I’ll run the requested shell command, then answer the arithmetic question.")
	eventually(t, "turn end", r.within(15*time.Second), func() bool { return len(r.events(t, "turn_ended", th)) > 0 })
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, s := range strings.Fields("PATH HOME USER LOGNAME SHELL LANG LC_ALL LC_CTYPE TZ TMPDIR XDG_RUNTIME_DIR XDG_CONFIG_HOME CODEX_HOME HATAORI_TEST_CODEX_CAPTURE HATAORI_TEST_CODEX_RECORD") {
		allowed[s] = true
	}
	seen := map[string]bool{}
	for _, s := range strings.Fields(string(b)) {
		if !allowed[s] {
			t.Errorf("unexpected environment %s", s)
		}
		seen[s] = true
	}
	for _, name := range []string{"HATAORI_TEST_CODEX_CAPTURE", "HATAORI_TEST_CODEX_RECORD"} {
		if !seen[name] {
			t.Errorf("missing passed variable %s", name)
		}
	}
	b, err = os.ReadFile(config)
	if err != nil || string(b) != string(original) {
		t.Fatalf("config changed: %q %v", b, err)
	}
	dir := filepath.Join(r.root, "workspaces", numberOf(t, r.root, th))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != ".git,README" {
		t.Fatal(names)
	}
	cmd := exec.Command("git", "status", "--porcelain", "--ignored")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("git status: %s %v", out, err)
	}
}

func TestCodexConfigInTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		settings map[string]any
		valid    bool
	}{{map[string]any{}, true}, {map[string]any{"nope": 1}, false}} {
		root := newRoot(t)
		path := filepath.Join(root, instance.ConfigFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		json.Unmarshal(b, &cfg)
		cfg["adapters"] = map[string]any{"codex": tc.settings}
		b, _ = json.Marshal(cfg)
		os.WriteFile(path, b, 0600)
		if tc.valid {
			startDaemon(t, root)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			code := runCtx(ctx, []string{"run", "--root", root}, &out, &stderr)
			if ctx.Err() == context.DeadlineExceeded {
				t.Fatal("the daemon started with an unknown key in adapters.codex")
			}
			if code == 0 || out.Len() != 0 || !strings.Contains(stderr.String(), "nope") || strings.Count(strings.TrimSpace(stderr.String()), "\n") != 0 {
				t.Fatalf("%d %q %q", code, out.String(), stderr.String())
			}
		}
	}
}

func TestCodexLoginRelay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HATAORI_TEST_CODEX_CAPTURE", capture)
	t.Setenv("HATAORI_TEST_CODEX_RECORD", filepath.Join(t.TempDir(), "env"))
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{"codex": map[string]any{"command": os.Args[0], "pass_env": []string{"HATAORI_TEST_CODEX_CAPTURE", "HATAORI_TEST_CODEX_RECORD"}}}
	})
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	a := r.order(t, 0, "!go repo:"+repo+" reply one")
	r.posted(t, a, "The turn failed: the agent is not logged in.")
	b := r.order(t, 1, "!go repo:"+repo+" reply two")
	r.posted(t, b, "The turn failed: the agent is not logged in.")
	dm, _ := r.driver.OpenDirect(nil, e2eOwner)
	eventually(t, "device prompt", 15*time.Second, func() bool {
		for _, c := range r.driver.Calls(dm) {
			if strings.Contains(c.Message.Text, "Stopped threads (2)") && strings.Contains(c.Message.Text, deviceLink) && strings.Contains(c.Message.Text, deviceCode) && strings.Contains(c.Message.Text, "Nothing to send back here:") {
				return true
			}
		}
		return false
	})
	r.driver.InjectDirect(e2eOwner, "test")
	r.posted(t, dm, "Not waiting for a code.")
	postsBeforeSignIn := 0
	for _, c := range r.driver.Calls(dm) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "is logged out") {
			postsBeforeSignIn++
		}
	}
	if postsBeforeSignIn != 1 {
		t.Fatalf("%d outage messages before sign-in", postsBeforeSignIn)
	}
	if err := os.WriteFile(filepath.Join(home, codexSignedInFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, th := range []surface.Conversation{a, b} {
		r.posted(t, th, "The agent is logged in again. Send your last message again to go on.")
	}
	r.posted(t, dm, "Logged in again.")
	for _, th := range []surface.Conversation{a, b} {
		if n := r.count(th, "The agent is logged in again. Send your last message again to go on."); n != 1 {
			t.Fatalf("%d restoration messages", n)
		}
	}
	posts := 0
	for _, c := range r.driver.Calls(dm) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "is logged out") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("%d outage messages", posts)
	}
	r.driver.Inject(a, fakesurface.Webhook(e2eHook), "try again")
	r.posted(t, a, "I’ll run the requested shell command, then answer the arithmetic question.")
	checkLoginSecrets(t, r, logged, deviceLink, deviceCode)
}

// TestCodexQuestionsAndApprovalsInTheThread checks captured approvals are automatically accepted and hidden, and questions shown with one choice per option and answered by the next message on the fake surface.
func TestCodexQuestionsAndApprovalsInTheThread(t *testing.T) {
	for _, tc := range []struct {
		scenario, first, answer, last string
		choices                       int // of the first post
	}{
		{"approval_accept", "Ran `touch approved.txt` successfully.", "continue", "Created `patch.txt` containing `hi`.", 0},
		{"question_async", "Which colour do you like?", "blue", "You like blue.", 4},
		{"question_after_turn", "Which colour do you like?", "blue", "Blue.", 4},
		{"question_plan", "What would you like help with?\n- Plan a change (Recommended): Explore the project and prepare a concrete implementation plan.\n- Explain something: Answer a question or clarify a topic.\n- Review work: Inspect existing code, writing, or a design.", "Plan a change (Recommended)", "", 3},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			temp := t.TempDir()
			home := filepath.Join(temp, "home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", home)
			if err := os.WriteFile(filepath.Join(home, codexLoggedFile), nil, 0600); err != nil {
				t.Fatal(err)
			}
			capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
			if err != nil {
				t.Fatal(err)
			}
			wire := filepath.Join(temp, "wire")
			t.Setenv("HATAORI_TEST_CODEX_CAPTURE", capture)
			t.Setenv("HATAORI_TEST_CODEX_RECORD", filepath.Join(temp, "env"))
			t.Setenv("HATAORI_TEST_CODEX_SCENARIO", tc.scenario)
			t.Setenv("HATAORI_TEST_CODEX_WIRE", wire)
			r := newChatRoot(t, func(cfg map[string]any) {
				cfg["adapters"] = map[string]any{"codex": map[string]any{"command": os.Args[0], "pass_env": []string{"HATAORI_TEST_CODEX_CAPTURE", "HATAORI_TEST_CODEX_RECORD", "HATAORI_TEST_CODEX_SCENARIO", "HATAORI_TEST_CODEX_WIRE"}}}
			})
			r.deadline = time.Now().Add(30 * time.Second)
			startDaemon(t, r.root)
			r.connected(t)
			r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" hello")
			th := r.thread(t, 0)
			eventually(t, "a post of "+tc.first, r.within(15*time.Second), func() bool {
				for _, c := range r.driver.Calls(th) {
					if c.Op == fakesurface.OpPost && c.Message.Text == tc.first && len(c.Message.Choices) == tc.choices {
						return true
					}
				}
				return false
			})
			if tc.scenario == "question_after_turn" {
				r.posted(t, th, "I haven’t received your answer yet. Which colour do you like?")
			}
			endsBefore := len(r.events(t, "turn_ended", th))
			r.driver.Inject(th, fakesurface.Webhook(e2eHook), tc.answer)
			if tc.last != "" {
				r.posted(t, th, tc.last)
			} else {
				eventually(t, "turn end", r.within(15*time.Second), func() bool { return len(r.events(t, "turn_ended", th)) > endsBefore })
			}
			b, err := os.ReadFile(wire)
			if err != nil {
				t.Fatal(err)
			}
			var sent []map[string]any
			for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
				var m map[string]any
				if err := json.Unmarshal(line, &m); err != nil {
					t.Fatal(err)
				}
				sent = append(sent, m)
			}
			f, err := os.Open(filepath.Join(capture, tc.scenario, "wire.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var captured []codexWire
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 4096), 4<<20)
			for scanner.Scan() {
				var line codexWire
				if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
					t.Fatal(err)
				}
				captured = append(captured, line)
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			selectMethod := func(messages []map[string]any, method string) []map[string]any {
				var found []map[string]any
				for _, m := range messages {
					if method == "" {
						if _, hasMethod := m["method"]; !hasMethod {
							found = append(found, m)
						}
					} else if m["method"] == method {
						found = append(found, m)
					}
				}
				return found
			}
			var capturedSent []map[string]any
			for _, l := range captured {
				if l.Dir == "send" {
					capturedSent = append(capturedSent, l.Msg)
				}
			}
			switch tc.scenario {
			case "approval_accept", "question_plan":
				got, want := selectMethod(sent, ""), selectMethod(capturedSent, "")
				if !reflect.DeepEqual(got, want) || (tc.scenario == "approval_accept" && len(got) != 2) || (tc.scenario == "question_plan" && len(got) != 1) {
					t.Fatalf("responses got %v want %v", got, want)
				}
				if tc.scenario == "approval_accept" && len(r.events(t, "request", th)) != 0 {
					t.Fatal("approval request appeared in thread")
				}
			case "question_async":
				got, want := selectMethod(sent, "turn/steer"), selectMethod(capturedSent, "turn/steer")
				if len(got) != 1 || len(want) != 1 {
					t.Fatalf("steers got %v want %v", got, want)
				}
				gp, wp := got[0]["params"].(map[string]any), want[0]["params"].(map[string]any)
				input := []any{map[string]any{"type": "text", "text": "blue"}}
				if !reflect.DeepEqual(gp["input"], input) || gp["expectedTurnId"] != wp["expectedTurnId"] {
					t.Fatalf("steer params %v", gp)
				}
			case "question_after_turn":
				starts := selectMethod(sent, "turn/start")
				if len(starts) != 2 || !reflect.DeepEqual(starts[1]["params"].(map[string]any)["input"], []any{map[string]any{"type": "text", "text": "blue"}}) {
					t.Fatalf("turn starts %v", starts)
				}
			}
		})
	}
}

// TestCodexReplayKeepsPosition checks an unknown client request leaves the next capture available.
func TestCodexReplayKeepsPosition(t *testing.T) {
	capture, err := filepath.Abs("../../internal/agent/codex/testdata/0.163.0")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(capture, "start/wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var initialize map[string]any
	for scanner.Scan() {
		var line codexWire
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Dir == "send" && line.Msg["method"] == "initialize" {
			initialize = line.Msg
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if initialize == nil {
		t.Fatal("missing captured initialize")
	}
	// The unknown request is derived from the captured initialize with another method and id.
	unknown := make(map[string]any, len(initialize))
	for k, v := range initialize {
		unknown[k] = v
	}
	unknown["method"] = "unknown/clientMethod"
	unknown["id"] = float64(999)
	first, _ := json.Marshal(unknown)
	second, _ := json.Marshal(initialize)
	cmd := exec.Command(os.Args[0], "app-server")
	cmd.Env = append(os.Environ(), "HATAORI_TEST_CODEX_CAPTURE="+capture, "HATAORI_TEST_CODEX_RECORD="+filepath.Join(t.TempDir(), "env"), "HATAORI_TEST_CODEX_SCENARIO=start")
	cmd.Stdin = bytes.NewReader(append(append(append(first, '\n'), second...), '\n'))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("replay: %v: %s", err, stderr.String())
	}
	var responses []map[string]any
	scan := bufio.NewScanner(&stdout)
	for scan.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["id"]; ok && m["method"] == nil {
			responses = append(responses, m)
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if len(responses) < 2 || responses[0]["id"] != float64(999) || responses[0]["error"] == nil || responses[1]["id"] != initialize["id"] || responses[1]["result"] == nil || responses[1]["error"] != nil {
		t.Fatalf("responses: %v", responses)
	}
}
