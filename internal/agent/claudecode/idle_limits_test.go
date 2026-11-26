package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
)

// Replay a real status-line payload through the handoff, codec, machine,
// Limits API and wake formatter. The host runs only this test's idle fake;
// no CLI, tmux, user configuration or real account is involved.
func TestIdleStatusLineCannotReplaceNewerLimits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 'idle', 'p', 'idle', 0, 0), (2, 's', 'active', 'p', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'claude-code', 'idle', 0, 0), (2, 2, 'claude-code', 'idle', 0, 0)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	machine := &session.Machine{Store: st}
	api := (&core.Core{Store: st}).As(control.Caller{May: control.OpRead})
	// Exercise the host's short socket path even on systems whose temporary
	// directory is short; macOS temporary directories can already need it.
	dir := filepath.Join(t.TempDir(), strings.Repeat("deep/", 20))
	t.Cleanup(func() {
		host.Stop(dir, time.Second, time.Second)
		killGroups(dir)
	})
	if _, err := host.Start(host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, dir,
		host.Spec{Argv: []string{os.Args[0], "role", "idle"}, Env: []string{}}); err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile("testdata/2.1.327/limits/statusline.json")
	if err != nil {
		t.Fatal(err)
	}
	// Vary values of the captured fields to model another session and reset.
	variant := func(used int, reset int64) []byte {
		t.Helper()
		var in map[string]json.RawMessage
		if err := json.Unmarshal(input, &in); err != nil {
			t.Fatal(err)
		}
		in["rate_limits"] = json.RawMessage(fmt.Sprintf(`{"seven_day":{"resets_at":%d,"used_percentage":%d}}`, reset, used))
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	codecs := map[int64]*codec{1: newTestCodec(), 2: newTestCodec()}
	seen := 0
	history := map[int64][][]byte{}
	at := time.Unix(1794980000, 0)
	// handOn's time and the host receipt time are controlled independently:
	// the latter is part of hataori's own envelope, not agent output.
	push := func(sid int64, in []byte, when time.Time, wantCalls int) {
		t.Helper()
		handOn(host.SocketPath(dir), filepath.Join(dir, fmt.Sprintf("last-%d", sid)), in, when)
		log, err := os.ReadFile(filepath.Join(dir, host.EventsFile))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(log), []byte("\n"))
		var calls []call
		for _, line := range lines {
			var k call
			if json.Unmarshal(line, &k) == nil && bytes.Contains(k.Payload, []byte(statusLineEvent)) {
				calls = append(calls, k)
			}
		}
		if len(calls)-seen != wantCalls {
			t.Errorf("session %d at %s: %d new calls, want %d", sid, when, len(calls)-seen, wantCalls)
		}
		for _, k := range calls[seen:] {
			k.At = when.UnixMilli()
			line, _ := json.Marshal(k)
			history[sid] = append(history[sid], line)
			for _, e := range codecs[sid].Decode(agent.Hook, line).Events {
				if err := machine.Apply(ctx, sid, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
					t.Fatal(err)
				}
			}
		}
		seen = len(calls)
	}
	check := func(used int, observed, now time.Time, reset int64, stale bool) {
		t.Helper()
		ls, err := api.Limits(ctx)
		if err != nil || len(ls) != 1 || len(ls[0].Windows) != 1 {
			t.Fatalf("Limits: %+v, %v", ls, err)
		}
		l := ls[0]
		if !l.At.Equal(observed) || l.Windows[0].Used != used || l.Windows[0].Resets == nil || l.Windows[0].Resets.Unix() != reset {
			t.Errorf("Limits: %+v, want %d at %s, reset %d", l, used, observed, reset)
		}
		line := supervisor.UsageWindows(ls, []string{"claude-code"}, now)
		if !strings.Contains(line, fmt.Sprintf("seven_day %d%% used", used)) || strings.Contains(line, "Reported ") != stale {
			t.Errorf("wake: %q (stale %v)", line, stale)
		}
	}
	const reset = int64(1795161600)
	push(1, input, at, 1)
	push(2, variant(72, reset), at.Add(time.Minute), 1)
	push(1, input, at.Add(time.Hour), 0)
	check(72, at.Add(time.Minute), at.Add(time.Hour), reset, true)
	// Recreate the codec, like a daemon reconnect: the persistent --last
	// state still keeps an old status-line observation from becoming fresh.
	codecs[1] = newTestCodec()
	for _, line := range history[1] {
		codecs[1].Decode(agent.Hook, line) // rebuild sequence numbers by replay
	}
	push(1, variant(21, reset), at.Add(2*time.Hour), 0) // reordered keys
	check(72, at.Add(time.Minute), at.Add(2*time.Hour), reset, true)
	push(1, bytes.Replace(variant(21, reset), []byte(`"used_percentage":21`), []byte(`"used_percentage":21.0`), 1), at.Add(2*time.Hour+time.Minute), 0)
	// A later reset replaces the snapshot even with lower usage.
	push(1, variant(3, reset+604800), at.Add(3*time.Hour), 1)
	check(3, at.Add(3*time.Hour), at.Add(3*time.Hour), reset+604800, false)
	// A genuine changed decrease is allowed within that same reset too.
	push(1, variant(2, reset+604800), at.Add(4*time.Hour), 1)
	check(2, at.Add(4*time.Hour), at.Add(4*time.Hour), reset+604800, false)
	push(1, variant(2, reset+604800), at.Add(5*time.Hour), 0)
	check(2, at.Add(4*time.Hour), at.Add(5*time.Hour), reset+604800, true)
}
