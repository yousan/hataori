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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/instance"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

type codexWire struct {
	Dir string         `json:"dir"`
	Msg map[string]any `json:"msg"`
}

func codexReplay(args []string) int {
	capture, record := os.Getenv("HATAORI_TEST_CODEX_CAPTURE"), os.Getenv("HATAORI_TEST_CODEX_RECORD")
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
	lines := read(filepath.Join(capture, "start/wire.jsonl"))
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
		if pos < len(lines) && lines[pos].Dir == "send" && lines[pos].Msg["method"] == method {
			pos++
			for pos < len(lines) && lines[pos].Dir == "recv" {
				v := lines[pos].Msg
				if _, ok := v["id"]; ok {
					v["id"] = m["id"]
				}
				out.Encode(v)
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
