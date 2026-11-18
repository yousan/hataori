package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	fakeagent "github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	fake "github.com/yousan/hataori/internal/surface/fake"
)

// reports records what the agent of each session reported when it was ready.
type reports struct {
	agent.Adapter
	mu    sync.Mutex
	ready map[string][2]string // by workspace: model, effort
}

func (r *reports) NewCodec(spec agent.Spec) agent.Codec {
	return &reportingCodec{Codec: r.Adapter.NewCodec(spec), r: r, dir: spec.Dir}
}

func (r *reports) of(dir string) ([2]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.ready[dir]
	return v, ok
}

type reportingCodec struct {
	agent.Codec
	r   *reports
	dir string
}

func (c *reportingCodec) Decode(src agent.Source, line []byte) agent.Actions {
	a := c.Codec.Decode(src, line)
	for _, e := range a.Events {
		if e.Kind == agent.SessionReady {
			c.r.mu.Lock()
			c.r.ready[c.dir] = [2]string{e.Model, e.Effort}
			c.r.mu.Unlock()
		}
	}
	return a
}

// workspaceDirs makes one directory per thread under root.
type workspaceDirs string

func (workspaceDirs) Adopt(context.Context, control.ThreadID, core.Placement) (string, bool, error) {
	return "", false, nil
}

func (workspaceDirs) State(context.Context, string) (control.WorkspaceState, error) {
	return control.WorkspaceState{}, nil
}

func (w workspaceDirs) Make(_ context.Context, id control.ThreadID, _ string, _ func(core.Placement) error) (string, error) {
	d := filepath.Join(string(w), id.Scope, id.Key)
	return d, os.MkdirAll(d, 0o700)
}

// killSessions kills the hosts and agents started under root, by the PIDs
// they wrote, and removes the socket directories they left. Each runs in a
// process group of its own (with the tools it started), and the group is
// killed only when the PID leads it.
func killSessions(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	for _, d := range dirs {
		for _, f := range []string{host.PIDFile, host.HostPIDFile} {
			if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
					if pg, err := syscall.Getpgid(pid); err == nil && pg == pid {
						syscall.Kill(-pid, syscall.SIGKILL)
					} else {
						syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			}
		}
		if sock := filepath.Dir(host.SocketPath(d)); strings.HasPrefix(filepath.Base(sock), "hataori-host-") {
			os.RemoveAll(sock)
		}
	}
}

func TestModelAndEffortFromTheTriggerLineOrTheDefaults(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	st := openStore(t, root)
	cfg, _ := json.Marshal(fakeagent.Config{Command: []string{os.Args[0], "role", "fake"}, Hataori: os.Args[0],
		Env: []string{"PATH=" + os.Getenv("PATH")}})
	ad, err := fakeagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep := &reports{Adapter: ad, ready: map[string][2]string{}}
	lb := &logBuf{}
	lg := slog.New(slog.NewTextHandler(lb, nil))
	m := &session.Machine{Store: st}
	r := &runner.Runner{Store: st, Machine: m, Adapters: map[string]agent.Adapter{"fake": rep},
		Host: host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, Root: root, Log: lg,
		InterruptGrace: 5 * time.Second, Poll: 20 * time.Millisecond}
	t.Cleanup(func() { r.Close(); st.Close() })
	d := fake.New()
	fe := &FrontEnd{Name: "chat", Driver: d, Trigger: "!go", Owner: owner, Log: lg,
		Outbox: &Outbox{Store: st, Client: "chat", Driver: d, Log: lg, Retry: 5 * time.Millisecond},
		Scopes: map[string]Scope{"withdefaults": {Enabled: true}, "without": {Enabled: true}}}
	work := workspaceDirs(filepath.Join(root, "work"))
	c := &core.Core{Store: st, Machine: m, Runner: r, Agent: "fake", Log: lg, Workspaces: work,
		Model: "instance-model", Effort: "low",
		Scopes: map[string]core.Scope{"withdefaults": {Enabled: true, Owner: "chat", Model: "scope-model", Effort: "medium"},
			"without": {Enabled: true, Owner: "chat"}},
		Owners: map[string]control.ScopeOwner{"chat": fe}}
	fe.API = c.As

	for _, tc := range []struct {
		scope, text   string
		model, effort string
	}{
		// The options in any order before the prompt, and on either side of repo:.
		{"withdefaults", "!go effort:high repo:file:///unused model:given-model reply done", "given-model", "high"},
		{"withdefaults", "!go repo:file:///unused reply done", "scope-model", "medium"},
		{"without", "!go repo:file:///unused reply done", "instance-model", "low"},
		{"without", "!go model:only-model repo:file:///unused reply done", "only-model", "low"},
	} {
		in := d.Inject(surfaceConv(tc.scope), fake.Person(owner), tc.text)
		fe.Message(in)
		var th string
		for _, call := range d.Calls(surfaceConv(tc.scope)) {
			if call.Op == fake.OpOpenThread {
				th = call.Opened.Thread
			}
		}
		dir := filepath.Join(string(work), tc.scope, th)
		waitFor(t, "the agent of "+tc.text+" to be ready", func() bool { _, ok := rep.of(dir); return ok })
		if got, _ := rep.of(dir); got != [2]string{tc.model, tc.effort} {
			t.Errorf("%q: the agent reports %v, want %s/%s", tc.text, got, tc.model, tc.effort)
		}
		// The agent receives the prompt without the options.
		id := control.ThreadID{Scope: tc.scope, Key: th}
		waitFor(t, "the input of "+tc.text, func() bool { return inputOf(t, st, id) != "" })
		if got := inputOf(t, st, id); got != "reply done" {
			t.Errorf("%q: the agent got %q", tc.text, got)
		}
	}
}

func surfaceConv(channel string) surface.Conversation { return surface.Conversation{Channel: channel} }

// inputOf returns the text of the thread's first input event.
func inputOf(t *testing.T, st *store.Store, id control.ThreadID) string {
	t.Helper()
	var data []byte
	st.DB.QueryRow(`SELECT data FROM events WHERE scope = ? AND thread = ? AND kind = 'input' ORDER BY pos LIMIT 1`,
		id.Scope, id.Key).Scan(&data)
	var d struct{ Text string }
	json.Unmarshal(data, &d)
	return d.Text
}

func TestUnknownEffortStartsNothingAndSaysWhatIsAllowed(t *testing.T) {
	f := newFront(t, workspaces{})
	in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git effort:extreme do it")
	f.fe.Message(in) // delivered twice: still one message
	if n := len(f.runner.all()); n != 0 {
		t.Fatalf("%d sessions started", n)
	}
	if n := len(f.threads()); n != 0 {
		t.Fatalf("%d threads opened", n)
	}
	// One more row in the channel, posted after the refusal, shows that
	// the refusal is posted once.
	if err := f.fe.Outbox.Write(context.Background(), row(1<<40, 0, chanConv, "marker")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the marker", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), 1<<40, 0); return ok })
	var posts []fake.Call
	for _, c := range f.driver.Calls(chanConv) {
		if c.Op == fake.OpPost {
			posts = append(posts, c)
		}
	}
	if len(posts) != 2 {
		t.Fatalf("posts %+v, want the refusal and the marker", posts)
	}
	msg := posts[0].Message
	if msg.ReplyTo == nil || *msg.ReplyTo != in.Ref || !strings.Contains(msg.Text, "low, medium, high, xhigh, max") {
		t.Fatalf("refusal %+v, want a reply to the trigger naming the allowed values", msg)
	}
}
