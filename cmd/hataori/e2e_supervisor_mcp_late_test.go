package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/host"
)

// claudeStandin is a stand-in for the Claude Code CLI on its terminal, as
// far as the wait for its MCP servers goes (#704): it reports its start,
// takes pastes at once, and connects to its MCP servers only
// HATAORI_TEST_MCP_DELAY later. A turn has the tools that were there when
// its prompt was submitted, as the CLI's has, and answers with their number.
func claudeStandin(args []string) int {
	if len(args) > 0 && args[0] == "auth" {
		fmt.Println(`{"loggedIn": true}`)
		return 0
	}
	arg := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	call := func(event string, more map[string]any) {
		p := map[string]any{"hook_event_name": event, "session_id": arg("--session-id") + arg("--resume")}
		for k, v := range more {
			p[k] = v
		}
		b, _ := json.Marshal(p)
		host.Call(os.Getenv(host.HookEnvVar), b, false, 10*time.Second)
	}
	var tools atomic.Int64
	delay, err := time.ParseDuration(os.Getenv("HATAORI_TEST_MCP_DELAY"))
	if err != nil {
		return 2
	}
	go func() {
		if arg("--mcp-config") == "" {
			return // not a supervisor's
		}
		time.Sleep(delay)
		n, err := standinTools(arg("--mcp-config"))
		if err != nil {
			fmt.Println("mcp:", err)
		}
		tools.Store(int64(n))
	}()
	source := "startup"
	if arg("--resume") != "" {
		source = "resume"
	}
	call("SessionStart", map[string]any{"source": source})
	fmt.Print("\x1b[?2004h")
	var typed []byte
	in := bufio.NewReader(os.Stdin)
	for prompt := 1; ; {
		c, err := in.ReadByte()
		if err != nil {
			return 0
		}
		if c != '\r' || (bytes.Contains(typed, []byte("\x1b[200~")) && !bytes.Contains(typed, []byte("\x1b[201~"))) {
			typed = append(typed, c)
			continue
		}
		text := strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "", "\r", "\n").Replace(string(typed))
		typed = nil
		if text == "" {
			continue // Enter in an empty box
		}
		n, id := tools.Load(), fmt.Sprintf("p%d", prompt)
		prompt++
		call("UserPromptSubmit", map[string]any{"prompt_id": id, "prompt": text})
		call("Stop", map[string]any{"prompt_id": id, "last_assistant_message": fmt.Sprintf("tools: %d", n)})
	}
}

// standinTools connects to the only server of the MCP config in file, and
// returns how many tools it has.
func standinTools(file string) (int, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		}
	}
	if err := json.Unmarshal(b, &cfg); err != nil || len(cfg.MCPServers) != 1 {
		return 0, fmt.Errorf("%s: %v", b, err)
	}
	for _, s := range cfg.MCPServers {
		cmd := exec.Command(s.Command, s.Args...)
		cmd.Env = os.Environ()
		for k, v := range s.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			return 0, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		answers := bufio.NewReader(out)
		for i, m := range []string{
			`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		} {
			if _, err := fmt.Fprintln(in, m); err != nil {
				return 0, err
			}
			if i == 1 {
				continue
			}
			l, err := answers.ReadBytes('\n')
			if err != nil {
				return 0, err
			}
			var resp struct {
				Result struct{ Tools []json.RawMessage }
			}
			if json.Unmarshal(l, &resp) == nil && len(resp.Result.Tools) > 0 {
				return len(resp.Result.Tools), nil // the server goes on until the CLI ends
			}
		}
	}
	return 0, nil
}

// A supervisor whose agent connects late to hataori's MCP server: the first
// turn, the turn of a wake, which is a launch of its own (#676), and a
// person's turn each have hataori's tools (#704).
func TestSupervisorTurnsWaitForTheirTools(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nHATAORI_TEST_MCP_DELAY=3s exec '"+os.Args[0]+"' claude-standin \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wt := newWakeTest(t, func(cfg map[string]any) {
		cfg["agent"] = claudecode.Name
		cfg["adapters"] = map[string]any{claudecode.Name: map[string]any{"command": wrapper}}
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	_, pos := wt.worker(t, owner, "c1", "x", "reply done")
	wt.read(t, pos)
	eventually(t, "the quiet period", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
	wt.clock().advance(30 * time.Second)
	wt.woken(t, owner, sv.ID, 1)
	var answers []string
	eventually(t, "the wake's turn", 15*time.Second, func() bool {
		answers = nil
		for _, e := range threadEvents(t, owner, sv.ID, "text") {
			answers = append(answers, e["text"].(string))
		}
		return len(answers) >= 2
	})
	supervisorReady(t, owner, "c1")
	answers = append(answers, sendAndRead(t, owner, sv.ID, "hello", "hello"))
	for i, a := range answers {
		if !strings.HasPrefix(a, "tools: ") || a == "tools: 0" {
			t.Errorf("turn %d of %d was given %q, want hataori's tools", i+1, len(answers), a)
		}
	}
}
