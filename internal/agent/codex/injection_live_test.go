package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// Opt-in, no inference: verify the pinned real CLI accepts a repeated trusted
// injection even when both copies are in flight. Only empty acknowledgements
// and counts may be exported; never raw startup metadata or credentials.
func TestRealCodexInstructionInjectionRetry(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CODEX_INJECTION_RETRY") != "1" {
		t.Skip("set HATAORI_REAL_CODEX_INJECTION_RETRY=1 for the isolated CLI check")
	}
	root := t.TempDir()
	spec := agent.Spec{Dir: filepath.Join(root, "work"), StateDir: filepath.Join(root, "state"), Model: confinedModel,
		Instructions: "Disposable instruction-retry fixture. No turn or tool use is authorized.", Confine: &agent.Confinement{}}
	for _, dir := range []string{spec.Dir, spec.StateDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ad, _ := New(nil)
	launch, err := ad.Launch(spec)
	if err != nil {
		t.Fatal("verified confined launch unavailable; no fallback attempted")
	}
	for path, content := range launch.Files {
		p := filepath.Join(spec.StateDir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launch.Argv[0], launch.Argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stderr = launch.Env, spec.Dir, io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cancel(); cmd.Wait() }()
	lines := make(chan []byte, 1024)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			select {
			case lines <- append([]byte(nil), scanner.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
	}()
	write := func(b []byte) {
		t.Helper()
		if _, err := in.Write(b); err != nil {
			t.Fatal("fixture pipe write failed")
		}
	}
	c := ad.NewCodec(spec)
	for _, b := range c.Open().Write {
		write(b)
	}
	injectionID, ready, turns := 0, 0, 0
	var acks [][]byte
	var idle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			t.Fatal("bounded real injection check timed out; raw output omitted")
		case <-idle:
			if len(acks) != 2 || ready != 1 || turns != 0 {
				t.Fatal("duplicate injection changed readiness or started a turn")
			}
			if path := os.Getenv("HATAORI_CODEX_INJECTION_CAPTURE"); path != "" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "acks.jsonl"), append(bytes.Join(acks, []byte("\n")), '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				notes, _ := json.MarshalIndent(map[string]any{"version": confinedVersion, "model": confinedModel, "duplicate_acks": len(acks),
					"ready_events": ready, "turns_observed": turns, "idle_seconds": 2, "inference_requested": false,
					"launch": "actual confined adapter; disposable state; existing CLI auth mounted read-only"}, "", "  ")
				if err := os.WriteFile(filepath.Join(path, "notes.json"), append(notes, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return
		case line, ok := <-lines:
			if !ok {
				t.Fatal("fixture CLI exited before completing the check")
			}
			var m struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			if m.Method == "turn/started" {
				turns++
			}
			if injectionID != 0 && m.ID == injectionID && m.Method == "" {
				if len(m.Error) != 0 || !bytes.Equal(bytes.TrimSpace(m.Result), []byte("{}")) {
					t.Fatal("duplicate injection refused or returned a malformed acknowledgement")
				}
				// Re-encode only the proven-empty result, excluding other fields.
				ack, _ := json.Marshal(map[string]any{"id": m.ID, "result": map[string]any{}})
				acks = append(acks, ack)
				if len(acks) == 2 {
					idle = time.After(2 * time.Second)
				}
			}
			a := c.Decode(agent.Output, line)
			for _, e := range a.Events {
				if e.Kind == agent.SessionReady {
					ready++
				}
				if e.Kind == agent.SessionEnded {
					t.Fatal("confined startup refused; no fallback attempted")
				}
			}
			for _, b := range a.Write {
				var request struct {
					ID     int
					Method string
				}
				json.Unmarshal(b, &request)
				write(b)
				if request.Method == "thread/inject_items" {
					injectionID = request.ID
					write(b) // exact same id/bytes before reading either ack
				}
			}
		}
	}
}
