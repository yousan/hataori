package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "worker-host":
			if err := host.Run(os.Args[2]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "worker-fake":
			os.Exit(fake.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		case "hook":
			os.Exit(host.HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
		}
	}
	os.Exit(m.Run())
}

type fixture struct {
	t             *testing.T
	root          string
	binding       workerproto.Binding
	options       Options
	r             *Resident
	connection    *workerproto.Connection
	target        workerproto.Target
	configuration workerproto.AgentConfig
	workspace     string
	sequence      int
}

func testJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func testFrame(t *testing.T, v any) []byte {
	t.Helper()
	b, err := workerproto.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newFixture(t *testing.T, terminal bool) *fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "worker")
	binding, err := Initialize(root, "main-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "state/worker/observed"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := testJSON(t, fake.Config{Command: []string{os.Args[0], "worker-fake"}, Hataori: os.Args[0], Terminal: terminal,
		Env:    []string{"PATH=/usr/bin:/bin", "WORKER_MARKER=worker-owned", "HATAORI_TOKEN=must-not-leak", "HATAORI_ROOT=must-not-leak", "SSH_AUTH_SOCK=must-not-leak"},
		Record: filepath.Join(root, "state/worker/observed")})
	options := Options{Config: &Config{Agents: []Configuration{{Agent: "fake", Name: "default", Settings: settings, NoProjectSettings: true}}},
		HostCommand: host.Command{Path: os.Args[0], Args: []string{"worker-host"}, Env: []string{}},
		BootID:      func() (string, error) { return "boot-test", nil },
		Telemetry: func() workerproto.Telemetry {
			return workerproto.Telemetry{ObservedMS: time.Now().UnixMilli(), ValidMS: 10000, Known: true, MemoryFree: 1 << 30, DiskFree: 1 << 30}
		},
		NewAdapter: func(name string, settings []byte) (agent.Adapter, error) {
			if name != "fake" {
				return nil, errors.New("unexpected adapter")
			}
			return fake.New(settings)
		}}
	f := &fixture{t: t, root: root, binding: binding, options: options, target: workerproto.Target{Thread: 7, Session: "session-test", Run: "run-test"}, configuration: workerproto.AgentConfig{Agent: "fake", Config: "default"}, workspace: "workspace-test"}
	f.open()
	t.Cleanup(func() {
		// The only process we stop is a host created by this fixture, reached
		// through its original private address and live identity, never by name.
		if f.r != nil {
			for _, run := range f.r.state.Runs {
				c, err := host.DialIdentity(context.Background(), f.r.hostDir(run.Target), run.Identity)
				if err == nil {
					c.Deadline(time.Now().Add(5 * time.Second))
					c.End(50*time.Millisecond, 50*time.Millisecond)
					c.Close()
					deadline := time.Now().Add(8 * time.Second)
					for !f.r.ended(run) && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
					}
					if !f.r.ended(run) {
						t.Error("own host did not finish cleanup")
					}
				}
			}
			f.r.Close()
		}
	})
	return f
}

func (f *fixture) open() {
	f.t.Helper()
	r, err := Open(f.root, f.binding, f.options)
	if err != nil {
		f.t.Fatal(err)
	}
	f.r = r
	peer := r.Hello()
	peer.Required = nil
	f.connection, err = workerproto.Negotiate(f.binding, peer, r.Hello())
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) request(op, key string, target *workerproto.Target, body any) workerproto.Message {
	f.t.Helper()
	f.sequence++
	return workerproto.Message{Kind: "request", ID: fmt.Sprintf("request-%d", f.sequence), Binding: f.binding, Target: target, Op: op, Key: key, Body: testJSON(f.t, body)}
}

func (f *fixture) call(m workerproto.Message) workerproto.Message {
	f.t.Helper()
	reply := f.r.Dispatch(context.Background(), f.connection, testFrame(f.t, m))
	if _, err := f.connection.Decode(testFrame(f.t, reply), m.Target); err != nil {
		f.t.Fatalf("resident produced invalid response: %v; %+v", err, reply)
	}
	return reply
}

func (f *fixture) receipt(m workerproto.Message) workerproto.Receipt {
	f.t.Helper()
	reply := f.call(m)
	if reply.Kind != "reply" {
		f.t.Fatalf("wanted receipt, got %s", reply.Body)
	}
	var rec workerproto.Receipt
	if err := json.Unmarshal(reply.Body, &rec); err != nil {
		f.t.Fatal(err)
	}
	if err := workerproto.CheckReceipt(m, rec); err != nil {
		f.t.Fatal(err)
	}
	return rec
}

func (f *fixture) prepare() {
	f.t.Helper()
	if rec := f.receipt(f.request("prepare", "prepare-test", &f.target, workerproto.Prepare{Instructions: "Worker-owned instructions.", Policy: []string{}})); rec.State != "complete" {
		f.t.Fatalf("prepare: %+v", rec)
	}
	// #961 supplies the worker-local workspace implementation. This isolated
	// fixture seeds its owned directory/identity record, never an external path.
	name := filepath.Join(f.root, "workspaces", f.workspace)
	if err := os.Mkdir(name, 0o700); err != nil {
		f.t.Fatal(err)
	}
	info, err := os.Lstat(name)
	if err != nil {
		f.t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	f.r.state.Workspaces[targetKey(f.target)+"-"+f.workspace] = workspaceRecord{Target: f.target, Handle: f.workspace, Device: uint64(st.Dev), Inode: uint64(st.Ino)}
	if err := f.r.save(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) startRequest() workerproto.Message {
	return f.request("start", "start-test", &f.target, workerproto.Start{AgentConfig: f.configuration, Holder: "own", Workspace: f.workspace})
}

func (f *fixture) waitReady() {
	f.t.Helper()
	f.t.Cleanup(func() {
		if f.t.Failed() {
			for _, name := range []string{host.StatusFile, host.HostPIDFile, "host.log", host.StderrLog, host.EventsFile} {
				b, _ := os.ReadFile(filepath.Join(f.r.hostDir(f.target), name))
				f.t.Logf("%s: %s", name, b)
			}
		}
	})
	eventually(f.t, "the actual fake agent's first report", func() bool {
		info, err := os.Stat(filepath.Join(f.r.hostDir(f.target), host.EventsFile))
		return err == nil && info.Size() > 0
	})
}

func (f *fixture) input(text, key string) workerproto.Message {
	f.t.Helper()
	a := f.r.adapters[f.configuration]
	actions, err := a.NewCodec(agent.Spec{}).Input(agent.Input{ID: "turn-test", Text: text})
	if err != nil {
		f.t.Fatal(err)
	}
	var data []byte
	for _, b := range actions.Write {
		data = append(data, b...)
	}
	for _, part := range actions.Keys {
		b, err := holder.Encode(part)
		if err != nil {
			f.t.Fatal(err)
		}
		data = append(data, b...)
	}
	return f.request("input", key, &f.target, workerproto.Input{Turn: "turn-test", Data: data})
}

func refused(t *testing.T, m workerproto.Message, want string) {
	t.Helper()
	var r workerproto.Refusal
	if m.Kind != "refusal" || json.Unmarshal(m.Body, &r) != nil || r.Code != want {
		t.Fatalf("got %s %s, want %s", m.Kind, m.Body, want)
	}
}

func TestRegistrationPermissionsReplacementAndLock(t *testing.T) {
	f := newFixture(t, false)
	if _, err := Initialize(f.root, "other-main"); !errors.Is(err, ErrBinding) {
		t.Fatalf("second main: %v", err)
	}
	if _, err := Open(f.root, f.binding, f.options); !errors.Is(err, ErrLocked) {
		t.Fatalf("second resident: %v", err)
	}
	other := filepath.Join(t.TempDir(), "replacement")
	b, err := Initialize(other, f.binding.Main)
	if err != nil {
		t.Fatal(err)
	}
	if b.Worker == f.binding.Worker {
		t.Fatal("replacement reused installation identity")
	}
	if _, err := Open(other, f.binding, f.options); !errors.Is(err, ErrBinding) {
		t.Fatalf("replacement: %v", err)
	}
	// A copied registration is still not the original filesystem installation.
	reg, err := os.ReadFile(filepath.Join(f.root, registrationFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, registrationFile), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(other, f.binding, f.options); !errors.Is(err, ErrBinding) {
		t.Fatalf("copied installation: %v", err)
	}
	f.r.Close()
	if err := os.Chmod(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.root, f.binding, f.options); err == nil {
		t.Fatal("accepted an exposed worker root")
	}
	os.Chmod(f.root, 0o700)
	if err := os.Chmod(filepath.Join(f.root, registrationFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.root, f.binding, f.options); err == nil {
		t.Fatal("accepted exposed registration")
	}
}

func TestStartReceiptSurvivesLostReplyAndRestart(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	request := f.startRequest()
	f.r.options.checkpoint = func(point string) error {
		if point == "start_effect" {
			return errors.New("lost reply")
		}
		return nil
	}
	refused(t, f.call(request), "unavailable")
	f.waitReady()
	identity := f.r.state.Runs[targetKey(f.target)].Identity
	f.r.Close()
	f.options.checkpoint = nil
	f.open()
	request.ID = "retry-after-restart"
	rec := f.receipt(request)
	if rec.State != "complete" || rec.Result == nil || rec.Result.Host != identity {
		t.Fatalf("reconciled receipt: %+v", rec)
	}
	rec = f.receipt(request)
	if rec.State != "complete" {
		t.Fatalf("repeated receipt: %+v", rec)
	}
	starts, err := filepath.Glob(filepath.Join(f.root, "state/worker/observed/start-*.json"))
	if err != nil || len(starts) != 1 {
		t.Fatalf("fake agent starts: %v, %v", starts, err)
	}
	end := f.receipt(f.request("end_host", "end-test", &f.target, workerproto.Empty{}))
	if end.State != "complete" {
		t.Fatalf("end: %+v", end)
	}
}

func TestResidentRestartPreservesFakeTurnAndWaitingHook(t *testing.T) {
	f := newFixture(t, true)
	f.prepare()
	if rec := f.receipt(f.startRequest()); rec.State != "complete" {
		t.Fatal(rec)
	}
	f.waitReady()
	if rec := f.receipt(f.input("ask colour\nreply survived", "input-test")); rec.State != "complete" {
		t.Fatal(rec)
	}
	var hook string
	eventually(t, "the fake agent's waiting hook", func() bool {
		c, err := f.r.owned(f.r.state.Runs[targetKey(f.target)])
		if err != nil {
			return false
		}
		defer c.Close()
		calls, err := c.Waiting()
		if err == nil && len(calls) == 1 {
			hook = calls[0].ID
			return true
		}
		return false
	})
	identity := f.r.state.Runs[targetKey(f.target)].Identity
	f.r.Close()
	c, err := host.DialIdentity(context.Background(), f.r.hostDir(f.target), identity)
	if err != nil {
		t.Fatalf("resident exit stopped its host: %v", err)
	}
	calls, err := c.Waiting()
	c.Close()
	if err != nil || len(calls) != 1 || calls[0].ID != hook {
		t.Fatalf("hook did not survive: %+v, %v", calls, err)
	}
	f.open()
	reply := f.call(f.request("inventory", "", nil, workerproto.Empty{}))
	var inventory workerproto.Inventory
	if json.Unmarshal(reply.Body, &inventory) != nil || !inventory.Complete || len(inventory.Hosts) != 1 || len(inventory.Hosts[0].Hooks) != 1 || inventory.Hosts[0].Hooks[0] != hook {
		t.Fatalf("inventory: %s", reply.Body)
	}
	if !f.r.ready {
		t.Fatal("recovered resident is not ready")
	}
	answer := f.receipt(f.request("answer", "answer-test", &f.target, workerproto.Answer{Turn: "turn-test", Request: hook, Data: []byte("blue")}))
	if answer.State != "complete" {
		t.Fatal(answer)
	}
	eventually(t, "the original waiting hook to finish", func() bool {
		c, err := f.r.owned(f.r.state.Runs[targetKey(f.target)])
		if err != nil {
			return false
		}
		defer c.Close()
		calls, err := c.Waiting()
		return err == nil && len(calls) == 0
	})
	eventually(t, "the original fake turn to complete", func() bool {
		b, err := os.ReadFile(filepath.Join(f.r.hostDir(f.target), host.EventsFile))
		if err != nil {
			return false
		}
		codec := f.r.adapters[f.configuration].NewCodec(agent.Spec{})
		completed := false
		for _, line := range bytes.Split(b, []byte("\n")) {
			for _, event := range codec.Decode(agent.Hook, line).Events {
				completed = completed || event.Kind == agent.TurnEnded && event.Status == agent.Completed
			}
		}
		return completed
	})
	starts, _ := filepath.Glob(filepath.Join(f.root, "state/worker/observed/start-*.json"))
	if len(starts) != 1 {
		t.Fatalf("restarted the fake agent: %v", starts)
	}
}

func TestUnknownWritesAreNotReplayed(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	f.receipt(f.startRequest())
	f.waitReady()
	m := f.input("reply once", "input-unknown")
	f.r.options.checkpoint = func(point string) error {
		if point == "input_effect" {
			return errors.New("lost reply")
		}
		return nil
	}
	refused(t, f.call(m), "unavailable")
	f.r.Close()
	f.open()
	rec := f.receipt(m)
	if rec.State != "unknown" {
		t.Fatalf("unknown write became retryable: %+v", rec)
	}
	if rec = f.receipt(m); rec.State != "unknown" {
		t.Fatal(rec)
	}
	eventually(t, "the fake's input observation", func() bool {
		files, _ := filepath.Glob(filepath.Join(f.root, "state/worker/observed/inputs-*.jsonl"))
		if len(files) != 1 {
			return false
		}
		b, _ := os.ReadFile(files[0])
		return bytes.Count(b, []byte("\n")) == 1
	})
}

func TestEndReceiptSurvivesLostReply(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	f.receipt(f.startRequest())
	f.waitReady()
	m := f.request("end_host", "end-unknown", &f.target, workerproto.Empty{})
	f.r.options.checkpoint = func(point string) error {
		if point == "end_host_effect" {
			return errors.New("lost reply")
		}
		return nil
	}
	refused(t, f.call(m), "unavailable")
	f.r.Close()
	f.open()
	rec := f.receipt(m)
	if rec.State != "complete" {
		t.Fatalf("end was not reconciled: %+v", rec)
	}
	if f.r.state.Runs[targetKey(f.target)].State != "ended" {
		t.Fatal("host is not known ended")
	}
}

func TestLaunchUsesWorkerPolicyAndEnvironment(t *testing.T) {
	t.Setenv("BOT_CREDENTIAL_MARKER", "main-secret")
	t.Setenv("FORGE_CREDENTIAL_MARKER", "main-secret")
	t.Setenv("SUPERVISOR_CREDENTIAL_MARKER", "main-secret")
	f := newFixture(t, false)
	f.prepare()
	f.receipt(f.startRequest())
	f.waitReady()
	files, _ := filepath.Glob(filepath.Join(f.root, "state/worker/observed/start-*.json"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Args, Env []string
		Dir       string
	}
	if err := json.Unmarshal(b, &observed); err != nil {
		t.Fatal(err)
	}
	wantDir, err := filepath.EvalSymlinks(filepath.Join(f.root, "workspaces", f.workspace))
	if err != nil {
		t.Fatal(err)
	}
	if observed.Dir != wantDir {
		t.Fatalf("worker cwd: %s", observed.Dir)
	}
	joined := strings.Join(observed.Env, "\n")
	if !strings.Contains(joined, "WORKER_MARKER=worker-owned") {
		t.Fatal("worker environment missing")
	}
	for _, name := range []string{"HATAORI_TOKEN", "HATAORI_ROOT", "SSH_AUTH_SOCK", "BOT_CREDENTIAL_MARKER", "FORGE_CREDENTIAL_MARKER", "SUPERVISOR_CREDENTIAL_MARKER"} {
		if strings.Contains(joined, name+"=") {
			t.Fatalf("launch leaked %s", name)
		}
	}
	if !strings.Contains(strings.Join(observed.Args, " "), "--no-project-settings") {
		t.Fatal("worker policy did not reach the launch")
	}
	instructions, err := os.ReadFile(filepath.Join(f.r.hostDir(f.target), fake.InstructionsFile))
	if err != nil || string(instructions) != "Worker-owned instructions." {
		t.Fatalf("instructions: %q, %v", instructions, err)
	}
}

func TestStaleAndUnknownTelemetryRefusesStart(t *testing.T) {
	for _, state := range []string{"unknown", "stale", "future", "zero-memory", "zero-disk"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t, false)
			f.prepare()
			f.r.options.Telemetry = func() workerproto.Telemetry {
				h := workerproto.Telemetry{ObservedMS: time.Now().UnixMilli(), ValidMS: 1000, Known: true, MemoryFree: 1, DiskFree: 1}
				switch state {
				case "unknown":
					h.Known = false
					h.MemoryFree = 0
					h.DiskFree = 0
				case "stale":
					h.ObservedMS -= 2000
				case "future":
					h.ObservedMS += 10000
				case "zero-memory":
					h.MemoryFree = 0
				case "zero-disk":
					h.DiskFree = 0
				}
				return h
			}
			refused(t, f.call(f.startRequest()), "unavailable")
			if _, ok, _ := host.ReadSpec(f.r.hostDir(f.target)); ok {
				t.Fatal("unavailable telemetry started a host")
			}
		})
	}
}

func TestPrivateSocketAndHandshakeRefusals(t *testing.T) {
	f := newFixture(t, false)
	addr, err := f.r.Listen()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(addr)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions: %v, %v", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.r.Serve(ctx) }()
	for _, kind := range []string{"main", "worker", "major", "owner"} {
		c, err := net.Dial("unix", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		hello := f.r.Hello()
		hello.Required = nil
		switch kind {
		case "main":
			hello.Binding.Main = "other-main"
		case "worker":
			hello.Binding.Worker = "replacement"
		case "major":
			hello.Version = "2.0"
		}
		data := testFrame(t, hello)
		if kind == "owner" {
			data = append(bytes.TrimSpace(data)[:len(bytes.TrimSpace(data))-1], []byte(`,"owner":true}`+"\n")...)
		}
		if _, err := c.Write(data); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(c).ReadBytes('\n')
		c.Close()
		if kind == "owner" {
			if err == nil {
				t.Fatal("owner hello was accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if kind == "major" {
			var refusal workerproto.VersionRefusal
			if json.Unmarshal(line, &refusal) != nil || refusal.Local != workerproto.Version || refusal.Peer != "2.0" {
				t.Fatalf("major refusal: %s", line)
			}
		} else {
			var refusal workerproto.Refusal
			if json.Unmarshal(line, &refusal) != nil || refusal.Code != "binding" {
				t.Fatalf("binding refusal: %s", line)
			}
		}
	}
	if len(f.r.state.Runs) != 0 {
		t.Fatal("refused handshake created a run")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNaturalExitReconcilesWithoutResidentRestart(t *testing.T) {
	for _, operation := range []string{"inventory", "end_host"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t, false)
			f.prepare()
			f.receipt(f.startRequest())
			f.waitReady()
			f.receipt(f.input("exit", "exit-input"))
			eventually(t, "owned final status", func() bool { return f.r.ended(f.r.state.Runs[targetKey(f.target)]) })
			if operation == "inventory" {
				var inv workerproto.Inventory
				json.Unmarshal(f.call(f.request(operation, "", nil, workerproto.Empty{})).Body, &inv)
				if !inv.Complete || len(inv.Hosts) != 1 || inv.Hosts[0].State != "ended" {
					t.Fatalf("inventory: %+v", inv)
				}
			} else if rec := f.receipt(f.request(operation, "end-natural", &f.target, workerproto.Empty{})); rec.State != "complete" {
				t.Fatal(rec)
			}
			fresh := f.target
			fresh.Run = "new-run"
			if rec := f.receipt(f.request("prepare", "prepare-next", &fresh, workerproto.Prepare{})); rec.State != "complete" {
				t.Fatal(rec)
			}
		})
	}
}

func TestFailedCompletionCannotReturnUndurableSuccess(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	f.receipt(f.startRequest())
	f.waitReady()
	m := f.input("reply committed-once", "save-failed")
	f.r.options.checkpoint = func(point string) error {
		if point == "input_effect" {
			return os.Chmod(filepath.Join(f.root, journalFile), 0o644)
		}
		return nil
	}
	refused(t, f.call(m), "unavailable")
	if rec := f.receipt(m); rec.State != "unknown" {
		t.Fatalf("uncommitted success escaped: %+v", rec)
	}
	os.Chmod(filepath.Join(f.root, journalFile), 0o600)
	f.r.Close()
	f.open()
	if rec := f.receipt(m); rec.State != "unknown" {
		t.Fatalf("pending write replayed: %+v", rec)
	}
}

func TestStartIntentWithoutEffectStaysUnknownAfterReboot(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	m := f.startRequest()
	f.r.options.checkpoint = func(point string) error {
		if point == "start_intent" {
			return errors.New("crash before launch")
		}
		return nil
	}
	refused(t, f.call(m), "unavailable")
	f.r.Close()
	f.options.BootID = func() (string, error) { return "new-boot", nil }
	f.open()
	if rec := f.receipt(m); rec.State != "unknown" {
		t.Fatalf("reboot invented launch: %+v", rec)
	}
	files, _ := filepath.Glob(filepath.Join(f.root, "state/worker/observed/start-*.json"))
	if len(files) != 0 {
		t.Fatal(files)
	}
}

func TestRootReplacementRefusedWhileResidentOpen(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	original := f.root + "-original"
	if err := os.Rename(f.root, original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.root); os.Rename(original, f.root) })
	if err := os.Mkdir(f.root, 0o700); err != nil {
		t.Fatal(err)
	}
	refused(t, f.call(f.startRequest()), "unavailable")
	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement touched: %v, %v", entries, err)
	}
}

func TestForeignTargetsKeysAndPolicyFilesRefused(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	for _, part := range []string{"thread", "session", "run"} {
		other := f.target
		switch part {
		case "thread":
			other.Thread++
		case "session":
			other.Session = "foreign"
		case "run":
			other.Run = "foreign"
		}
		m := f.startRequest()
		m.Target = &other
		if got := f.call(m); got.Kind != "refusal" {
			t.Fatalf("foreign %s accepted: %+v", part, got)
		}
	}
	m := f.request("prepare", "prepare-test", &f.target, workerproto.Prepare{Instructions: "changed"})
	refused(t, f.call(m), "key_conflict")
	for _, name := range []string{"../escape", "/absolute", "owner.json", "HOST.JSON", "sub/../../escape"} {
		if err := f.r.writeLaunchFiles(f.target, map[string][]byte{name: []byte("bad")}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestOldPIDAndSocketPointerNeverAuthorizeRun(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	f.receipt(f.startRequest())
	f.waitReady()
	run := f.r.state.Runs[targetKey(f.target)]
	for _, name := range []string{host.PIDFile, host.HostPIDFile} {
		os.WriteFile(filepath.Join(f.r.hostDir(f.target), name), []byte("1\n"), 0o600)
	}
	os.WriteFile(filepath.Join(f.r.hostDir(f.target), "host.sock.path"), []byte("/nonexistent/foreign.sock"), 0o600)
	c, err := f.r.owned(run)
	if err != nil {
		t.Fatalf("followed pointer/PID: %v", err)
	}
	c.Close()
	wrong := *run
	wrong.Identity = "old-run-identity"
	if _, err := f.r.owned(&wrong); err == nil {
		t.Fatal("accepted another run identity")
	}
	if _, err := host.DialIdentity(context.Background(), f.r.hostDir(f.target), wrong.Identity); err == nil {
		t.Fatal("live host accepted another identity")
	}
}

func TestSocketReplacementFilePreserved(t *testing.T) {
	f := newFixture(t, false)
	addr, err := f.r.Listen()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(addr); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addr, []byte("operator data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.r.Close()
	b, err := os.ReadFile(addr)
	if err != nil || string(b) != "operator data" {
		t.Fatalf("replacement lost: %q %v", b, err)
	}
	f.open()
	if _, err := f.r.Listen(); err == nil {
		t.Fatal("overwrote non-socket data")
	}
}

type healthAdapter struct {
	agent.Adapter
	health  agent.Health
	err     error
	onProbe func()
}

func (a healthAdapter) Probe(context.Context, agent.Spec) (agent.Health, error) {
	if a.onProbe != nil {
		a.onProbe()
	}
	return a.health, a.err
}
func (a healthAdapter) ForWorker() agent.Adapter { return a }
func TestPerConfigurationHealthDistinguishesUnknown(t *testing.T) {
	f := newFixture(t, false)
	for _, tc := range []struct {
		name, install, login string
		health               agent.Health
		err                  error
	}{
		{"missing", "missing", "unknown", agent.Health{}, nil},
		{"loggedout", "installed", "logged_out", agent.Health{Installed: true}, nil},
		{"loggedin", "installed", "logged_in", agent.Health{Installed: true, LoggedIn: true}, nil},
		{"unknown", "unknown", "unknown", agent.Health{Unknown: true}, nil},
		{"loginunknown", "installed", "unknown", agent.Health{Installed: true, Unknown: true}, nil},
		{"error", "unknown", "unknown", agent.Health{}, errors.New("uncertain")},
	} {
		key := workerproto.AgentConfig{Agent: "fake", Config: tc.name}
		f.r.configs[key] = Configuration{Agent: "fake", Name: tc.name}
		f.r.adapters[key] = healthAdapter{Adapter: f.r.adapters[f.configuration], health: tc.health, err: tc.err}
		var h workerproto.AgentHealth
		json.Unmarshal(f.call(f.request("probe", "", nil, key)).Body, &h)
		if h.AgentConfig != key || h.Installation != tc.install || h.Login != tc.login {
			t.Fatalf("%s: %+v", tc.name, h)
		}
	}
}

func TestRecoveryRequiresWaitingHookReconciliation(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	run := f.r.state.Runs[targetKey(f.target)]
	run.State = "starting"
	if err := f.r.save(); err != nil {
		t.Fatal(err)
	}
	// An isolated private host-protocol fixture proves that identity success
	// alone cannot substitute for checking the owned host's waiting hooks.
	addr := strings.TrimPrefix(host.HookEnv(f.r.hostDir(f.target)), host.HookEnvVar+"=")
	if err := os.MkdirAll(filepath.Dir(addr), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		d := json.NewDecoder(c)
		e := json.NewEncoder(c)
		var request struct {
			Op string `json:"op"`
		}
		if d.Decode(&request) != nil || request.Op != "identity" {
			return
		}
		e.Encode(map[string]any{"ok": true, "identity": run.Identity})
		if d.Decode(&request) != nil || request.Op != "waiting" {
			return
		}
		e.Encode(map[string]any{"ok": false, "error": "waiting unavailable"})
	}()
	f.r.Close()
	f.open()
	<-done
	if f.r.ready || f.r.state.Runs[targetKey(f.target)].State != "unknown" {
		t.Fatal("identity alone enabled readiness")
	}
	ln.Close()
}

func TestRootMovedDuringProbeRefusesPathBasedLaunch(t *testing.T) {
	f := newFixture(t, false)
	f.prepare()
	original := f.root + "-original"
	t.Cleanup(func() { os.Remove(f.root); os.Rename(original, f.root) })
	f.r.adapters[f.configuration] = healthAdapter{Adapter: f.r.adapters[f.configuration], health: agent.Health{Installed: true, LoggedIn: true}, onProbe: func() {
		if err := os.Rename(f.root, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.root, 0700); err != nil {
			t.Fatal(err)
		}
	}}
	refused(t, f.call(f.startRequest()), "unavailable")
	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement touched: %v %v", entries, err)
	}
}

func TestInventoryAfterLostIntentReplyUsesPublicUnknownState(t *testing.T) {
	for _, op := range []string{"prepare", "start"} {
		t.Run(op, func(t *testing.T) {
			f := newFixture(t, false)
			var m workerproto.Message
			if op == "start" {
				f.prepare()
				m = f.startRequest()
			} else {
				m = f.request("prepare", "lost-prepare", &f.target, workerproto.Prepare{})
			}
			f.r.options.checkpoint = func(point string) error {
				if point == op+"_intent" {
					return errors.New("lost intent reply")
				}
				return nil
			}
			refused(t, f.call(m), "unavailable")
			reply := f.call(f.request("inventory", "", nil, workerproto.Empty{}))
			var inv workerproto.Inventory
			if err := json.Unmarshal(reply.Body, &inv); err != nil || inv.Complete || len(inv.Hosts) != 1 || inv.Hosts[0].State != "unknown" {
				t.Fatalf("inventory: %s %v", reply.Body, err)
			}
		})
	}
}

func TestPrepareCannotReplaceRetainedTarget(t *testing.T) {
	for _, final := range []string{"ended", "gone"} {
		t.Run(final, func(t *testing.T) {
			f := newFixture(t, false)
			original := f.request("prepare", "retained-prepare", &f.target, workerproto.Prepare{})
			f.receipt(original)
			run := f.r.state.Runs[targetKey(f.target)]
			identity := run.Identity
			if final == "ended" {
				// A same-identity final status is real host evidence, not a journal
				// assertion invented by the peer.
				f.workspace = "workspace-retained"
				name := filepath.Join(f.root, "workspaces", f.workspace)
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(name)
				if err != nil {
					t.Fatal(err)
				}
				st := info.Sys().(*syscall.Stat_t)
				f.r.state.Workspaces[targetKey(f.target)+"-"+f.workspace] = workspaceRecord{Target: f.target, Handle: f.workspace, Device: uint64(st.Dev), Inode: uint64(st.Ino)}
				if err := f.r.save(); err != nil {
					t.Fatal(err)
				}
				f.receipt(f.startRequest())
				f.waitReady()
				f.receipt(f.request("end_host", "end-retained", &f.target, workerproto.Empty{}))
			} else {
				// Simulate a previously recorded run absent after reboot. It still
				// retains its target and cannot be relabelled as a different run.
				run.State = "gone"
				run.Boot = "previous-boot"
				if err := f.r.save(); err != nil {
					t.Fatal(err)
				}
			}
			refused(t, f.call(f.request("prepare", "new-prepare-key", &f.target, workerproto.Prepare{})), "wrong_target")
			if f.r.state.Runs[targetKey(f.target)].Identity != identity || !f.r.ready {
				t.Fatal("stale prepare replaced ownership or disabled resident")
			}
			if rec := f.receipt(original); rec.State != "complete" {
				t.Fatalf("original prepare lost idempotency: %+v", rec)
			}
			if err := f.r.checkMarker(f.r.state.Runs[targetKey(f.target)]); err != nil {
				t.Fatal(err)
			}
			f.r.Close()
			f.open()
			if !f.r.ready || f.r.state.Runs[targetKey(f.target)].Identity != identity {
				t.Fatal("retained target did not survive restart")
			}
			fresh := f.target
			fresh.Run = "fresh-retained-run"
			if rec := f.receipt(f.request("prepare", "fresh-key", &fresh, workerproto.Prepare{})); rec.State != "complete" {
				t.Fatal(rec)
			}
		})
	}
}
