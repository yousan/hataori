package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/placement"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/worker"
	"github.com/yousan/hataori/internal/workerproto"
)

func remoteFixture(t *testing.T, terminal bool) (*fixture, *worker.Resident) {
	t.Helper()
	f := newFixture(t, options{terminal: terminal, poll: 30 * time.Millisecond})
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(f.log.String())
		}
	})
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	root := filepath.Join(t.TempDir(), "worker")
	main, err := f.st.MainIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := worker.Initialize(root, main)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "--allow-empty", "-m", "fixture"}} {
		c := exec.Command("git", args...)
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("isolated git fixture: %s %v", b, err)
		}
	}
	for _, name := range []string{"conversations", "records"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	settings, _ := json.Marshal(fake.Config{Conversations: filepath.Join(root, "conversations"), Record: filepath.Join(root, "records"), Command: []string{os.Args[0], "role", "fake"}, Hataori: os.Args[0], Terminal: terminal})
	resident, err := worker.Open(root, binding, worker.Options{Config: &worker.Config{Agents: []worker.Configuration{{Agent: "fake", Name: "default", Settings: settings, NoProjectSettings: false}}, Workspaces: worker.WorkspaceConfig{Repositories: map[string]string{"fixture": (&url.URL{Scheme: "file", Path: repo}).String()}}}, HostCommand: host.Command{Path: os.Args[0], Args: []string{"role", "host"}, Env: []string{}}, Telemetry: func() workerproto.Telemetry {
		return workerproto.Telemetry{Known: true, ObservedMS: time.Now().UnixMilli(), ValidMS: 10000, MemoryFree: 1 << 30, DiskFree: 1 << 30}
	}, NewAdapter: func(_ string, b []byte) (agent.Adapter, error) { return fake.New(b) }})
	if err != nil {
		t.Fatal(err)
	}
	socket, err := resident.Listen()
	if err != nil {
		resident.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go resident.Serve(ctx)
	t.Cleanup(func() {
		f.run.Close()
		rows, err := f.st.DB.Query(`SELECT session_id FROM session_machine_runs`)
		if err == nil {
			var ids []int64
			for rows.Next() {
				var id int64
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				if remote, err := f.run.remoteSession(context.Background(), id); err == nil {
					remote.End(context.Background(), "fixture-end", time.Millisecond, time.Millisecond)
				}
			}
		}
		cancel()
		resident.Close()
	})
	if err := f.st.SyncMachines(context.Background(), []store.MachineBinding{{Key: "build", SSH: "fixture", Root: root}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BindMachineInstallation(context.Background(), "build", binding.Worker); err != nil {
		t.Fatal(err)
	}
	f.run.Remotes = map[string]RemoteMachine{"build": {Binding: binding, Destination: "fixture", Root: root, Open: func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, nil, err
		}
		peer := resident.Hello()
		peer.Required = []string{"runner", "replay", "receipts"}
		if err := json.NewEncoder(c).Encode(peer); err != nil {
			c.Close()
			return nil, nil, err
		}
		b, err := bufio.NewReader(c).ReadBytes('\n')
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		hello, err := workerproto.DecodeHello(b)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		p, err := workerproto.Negotiate(binding, peer, hello)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, p, nil
	}}}
	return f, resident
}

func seedRemote(t *testing.T, f *fixture, id int64, script string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := f.st.DB.Exec(`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine,repo) VALUES(?,'s',?,'owner','creating',?,?,'build','fixture')`, id, fmt.Sprint(id), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at,launch) VALUES(?,?,'fake','creating',?,?,1)`, id, id, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`UPDATE threads SET no_project_settings=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	workspace, err := f.run.MakeRemoteWorkspace(context.Background(), id, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`UPDATE threads SET workspace=? WHERE id=?`, workspace, id); err != nil {
		t.Fatal(err)
	}
	if err := f.run.Start(context.Background(), core.Launch{Session: id, Thread: control.ThreadID(id), Agent: "fake", Dir: workspace, Principal: "owner", Key: fmt.Sprint(id), Input: control.Input{Text: script}}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteRunnerRestartMidTurn(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "tool wait 800ms\nreply worker result")
	waitFor(t, "worker turn", 5*time.Second, f.has("turn_started", 1))
	f.run.Close()
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one worker result after restart", 8*time.Second, f.has("turn_ended", 1))
	if got := counts(t, f.st); got["turn_started"] != 1 || got["turn_ended"] != 1 {
		t.Fatalf("duplicate lifecycle: %v", got)
	}
	var proof int
	if err := f.st.DB.QueryRow(`SELECT count(*) FROM session_read_proofs`).Scan(&proof); err != nil || proof == 0 {
		t.Fatalf("no committed replay proofs: %d %v", proof, err)
	}
	if _, err := os.Stat(StateDir(f.root, 1)); !os.IsNotExist(err) {
		t.Fatalf("worker I/O created main session state: %v", err)
	}
}

func TestRemoteRunnerQuestionAnswerAndFollowup(t *testing.T) {
	f, _ := remoteFixture(t, true)
	seedRemote(t, f, 1, "ask colour\nreply original")
	waitFor(t, "worker question", 8*time.Second, f.has("request", 1))
	var request string
	if err := f.st.DB.QueryRow(`SELECT handle FROM requests WHERE state='open'`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	f.run.Close()
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := owner(f.core).Answer(context.Background(), control.RequestID(request), control.Answer{Text: "blue"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "answered worker turn", 8*time.Second, func() bool { return withText(t, f.st, "text", "original") == 1 })
	if err := owner(f.core).Send(context.Background(), thread, "followup", control.Input{Text: "reply followup"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "followup on same worker", 8*time.Second, func() bool { return withText(t, f.st, "text", "followup") == 1 && f.has("turn_ended", 3)() })
	if got := counts(t, f.st); got["turn_ended"] != 3 || got["request"] != 1 {
		t.Fatalf("worker lifecycle: %v", got)
	}
}

func TestRemoteRunnerSleepResumeKeepsWorkspace(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply before sleep")
	waitFor(t, "first worker turn", 8*time.Second, f.has("turn_ended", 1))
	var before string
	if err := f.st.DB.QueryRow(`SELECT workspace FROM session_machine_runs WHERE session_id=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if slept, err := f.run.Sleep(context.Background(), thread); err != nil || !slept {
		t.Fatalf("sleep: %v %v", slept, err)
	}
	if err := owner(f.core).Send(context.Background(), thread, "after-sleep", control.Input{Text: "reply resumed"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resumed worker turn", 10*time.Second, f.has("turn_ended", 2))
	var after string
	if err := f.st.DB.QueryRow(`SELECT workspace FROM session_machine_runs WHERE session_id=1`).Scan(&after); err != nil || after != before {
		t.Fatalf("workspace changed: %s %s %v", before, after, err)
	}
	var runs int
	if err := f.st.DB.QueryRow(`SELECT count(*) FROM machine_run_history`).Scan(&runs); err != nil || runs != 2 {
		t.Fatalf("run history: %d %v", runs, err)
	}
}

type lostWorkerReply struct {
	net.Conn
	lost    *atomic.Bool
	drop    bool
	refusal bool
}

func (c *lostWorkerReply) Write(b []byte) (int, error) {
	var m workerproto.Message
	if json.Unmarshal(b, &m) == nil && m.Op == "input" {
		var in workerproto.Input
		json.Unmarshal(m.Body, &in)
		if bytes.Contains(in.Data, []byte("acknowledgment lost")) && !c.lost.Load() {
			c.drop = true
		}
	}
	return c.Conn.Write(b)
}
func (c *lostWorkerReply) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.drop && n > 0 {
		c.drop = false
		c.lost.Store(true)
		if c.refusal {
			var m workerproto.Message
			if err := json.Unmarshal(b[:n], &m); err != nil {
				return 0, err
			}
			m.Kind = "refusal"
			m.Body = json.RawMessage(`{"code":"unavailable","message":"reply lost after effect"}`)
			data, err := workerproto.Encode(m)
			if err != nil {
				return 0, err
			}
			if len(data) > len(b) {
				return 0, io.ErrShortBuffer
			}
			copy(b, data)
			return len(data), nil
		}
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestRemoteRunnerLostInputReplyNeverRepeatsWrite(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprint(refusal), func(t *testing.T) {
			f, _ := remoteFixture(t, false)
			cfg := f.run.Remotes["build"]
			open := cfg.Open
			var lost atomic.Bool
			cfg.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
				c, p, err := open(ctx)
				if err != nil {
					return nil, nil, err
				}
				return &lostWorkerReply{Conn: c, lost: &lost, refusal: refusal}, p, nil
			}
			f.run.Remotes["build"] = cfg
			seedRemote(t, f, 1, "tool wait 600ms\nreply acknowledgment lost")
			waitFor(t, "lost acknowledgment after worker wrote", 8*time.Second, lost.Load)
			var delivery string
			waitFor(t, "recorded uncertain outcome", 5*time.Second, func() bool {
				err := f.st.DB.QueryRow(`SELECT state FROM session_machine_actions WHERE operation='input' ORDER BY id DESC LIMIT 1`).Scan(&delivery)
				return err == nil && delivery != "pending"
			})
			if delivery != "unknown" {
				t.Fatalf("post-effect lost reply became no-write proof: %s", delivery)
			}
			f.run.Close()
			next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
			f.run = next
			f.core.Runner = next
			if err := next.Reconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "original written input to finish", 8*time.Second, f.has("turn_ended", 1))
			if got := counts(t, f.st); got["turn_started"] != 1 || got["turn_ended"] != 1 {
				t.Fatalf("input was repeated: %v", got)
			}
			var unknown int
			if err := f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE state IN ('pending','unknown')`).Scan(&unknown); err != nil || unknown != 0 {
				t.Fatalf("receipt was not reconciled: %d %v", unknown, err)
			}
		})
	}
}

func TestRemoteAttachmentSurvivesMainCloseAndReportsNetworkLoss(t *testing.T) {
	f, _ := remoteFixture(t, true)
	seedRemote(t, f, 1, "reply ready")
	waitFor(t, "initial worker turn", 8*time.Second, f.has("turn_ended", 1))
	remote, err := f.run.remoteSession(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	open := remote.Open
	var connection net.Conn
	remote.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		c, p, err := open(ctx)
		connection = c
		return c, p, err
	}
	a, err := remote.Attach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remote.Open = open
	defer a.Close()
	if _, err := a.Next(); err != nil {
		t.Fatal(err)
	}
	f.run.Close()
	if err := a.Input([]byte("reply attached\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attachment still owned after main close", 5*time.Second, func() bool { m, err := remote.Info(context.Background()); return err == nil && m.Attached == 1 })
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := a.Next()
		if err != nil {
			if !errors.Is(err, machineio.ErrAttachmentLost) {
				t.Fatalf("network loss was not visible: %v", err)
			}
			break
		}
	}
}

func TestRemoteRunnerFreshConversationAndSafeStart(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply original")
	waitFor(t, "original turn", 8*time.Second, f.has("turn_ended", 1))
	var original string
	f.st.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE id=1`).Scan(&original)
	if slept, err := f.run.Sleep(context.Background(), thread); err != nil || !slept {
		t.Fatal(err)
	}
	fresh := f.core.AsFresh(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all})
	if err := fresh.Send(context.Background(), thread, "fresh", control.Input{Text: "reply isolated"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "isolated turn", 8*time.Second, f.has("turn_ended", 2))
	var current string
	f.st.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE id=1`).Scan(&current)
	// The original is restored by the core after the isolated turn, while
	// metadata proves that this actual worker launch did not resume it.
	remote, err := f.run.remoteSession(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := remote.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Resume != "" || !info.NoProjectSettings {
		t.Fatalf("unsafe or shared launch: %+v", info)
	}
	if original == "" {
		t.Fatal("original conversation was not recorded")
	}
	if current != original {
		t.Fatalf("isolated conversation replaced original: %q != %q", current, original)
	}
}
func TestRemoteRunnerCannotResumeTransfersWorkspace(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply original")
	waitFor(t, "original turn", 8*time.Second, f.has("turn_ended", 1))
	remote, err := f.run.remoteSession(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := remote.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slept, err := f.run.Sleep(context.Background(), thread); err != nil || !slept {
		t.Fatal(err)
	}
	var conversation string
	f.st.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE id=1`).Scan(&conversation)
	// Only the isolated fake agent's own conversation fixture is removed.
	if err := os.Remove(filepath.Join(f.run.Remotes["build"].Root, "conversations", conversation)); err != nil {
		t.Fatal(err)
	}
	send(t, owner(f.core), thread, "replacement", "reply replacement")
	waitFor(t, "replacement turn", 10*time.Second, f.has("turn_ended", 2))
	var next int64
	var path string
	f.st.DB.QueryRow(`SELECT id,json_extract(spec,'$.spec.Dir') FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&next, &path)
	if next != 2 || path != info.Dir {
		t.Fatalf("replacement %d path %q != %q", next, path, info.Dir)
	}
}

// transformReply changes an actual isolated worker frame, retaining its
// binding/target/correlation fields. It never invents agent output.
type transformReply struct {
	net.Conn
	reader *bufio.Reader
	buffer []byte
	change func(workerproto.Message) workerproto.Message
}

func (c *transformReply) Read(b []byte) (int, error) {
	if len(c.buffer) == 0 {
		frame, err := c.reader.ReadBytes('\n')
		if err != nil {
			return 0, err
		}
		var m workerproto.Message
		if err := json.Unmarshal(frame, &m); err != nil {
			return 0, err
		}
		c.buffer, err = workerproto.Encode(c.change(m))
		if err != nil {
			return 0, err
		}
	}
	n := copy(b, c.buffer)
	c.buffer = c.buffer[n:]
	return n, nil
}

func transformRemote(f *fixture, change func(workerproto.Message) workerproto.Message) {
	cfg := f.run.Remotes["build"]
	open := cfg.Open
	cfg.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		c, p, err := open(ctx)
		if err != nil {
			return nil, nil, err
		}
		return &transformReply{Conn: c, reader: bufio.NewReader(c), change: change}, p, nil
	}
	f.run.Remotes["build"] = cfg
}

func TestRemoteRunnerEqualMetadataPathsStayIsolated(t *testing.T) {
	a, _ := remoteFixture(t, false)
	b, _ := remoteFixture(t, false)
	for _, f := range []*fixture{a, b} {
		transformRemote(f, func(m workerproto.Message) workerproto.Message {
			if m.Op == "describe" && m.Kind == "reply" {
				var info workerproto.RunMetadata
				json.Unmarshal(m.Body, &info)
				info.Dir, info.StateDir, info.ConfigDir = "/worker/equal/workspace", "/worker/equal/session", "/worker/equal/config"
				m.Body, _ = json.Marshal(info)
			}
			return m
		})
	}
	seedRemote(t, a, 1, "reply machine A")
	seedRemote(t, b, 1, "reply machine B")
	for _, f := range []*fixture{a, b} {
		waitFor(t, "isolated result", 8*time.Second, f.has("turn_ended", 1))
	}
	for i, f := range []*fixture{a, b} {
		want := []string{"machine A", "machine B"}[i]
		texts := f.eventData("text")
		if len(texts) != 1 || texts[0]["text"] != want {
			t.Fatalf("mixed streams: %v", texts)
		}
		if err := owner(f.core).Send(context.Background(), thread, "next", control.Input{Text: "reply " + want + " again"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "isolated followup", 8*time.Second, f.has("turn_ended", 2))
	}
}

func TestRemoteRunnerMissingReplayPausesWithoutCommitting(t *testing.T) {
	f, _ := remoteFixture(t, false)
	var missing atomic.Bool
	transformRemote(f, func(m workerproto.Message) workerproto.Message {
		if missing.Load() && m.Op == "read" && m.Kind == "reply" {
			var b workerproto.Bytes
			json.Unmarshal(m.Body, &b)
			b.Data = nil
			b.State = "history_lost"
			b.Next = b.Offset
			m.Body, _ = json.Marshal(b)
		}
		return m
	})
	seedRemote(t, f, 1, "tool wait 800ms\nreply retained")
	waitFor(t, "recorded worker range", 5*time.Second, f.has("turn_started", 1))
	f.run.Close()
	var before int64
	f.st.DB.QueryRow(`SELECT log_offset FROM sessions WHERE id=1`).Scan(&before)
	missing.Store(true)
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "visible replay pause", 5*time.Second, func() bool {
		var reason string
		f.st.DB.QueryRow(`SELECT io_error FROM sessions WHERE id=1`).Scan(&reason)
		return reason != ""
	})
	var after int64
	f.st.DB.QueryRow(`SELECT log_offset FROM sessions WHERE id=1`).Scan(&after)
	if before != after || f.has("turn_ended", 1)() {
		t.Fatalf("false progress: %d -> %d, %v", before, after, counts(t, f.st))
	}
	missing.Store(false)
	waitFor(t, "recovered retained range", 8*time.Second, f.has("turn_ended", 1))
	if got := counts(t, f.st); got["turn_ended"] != 1 || got["turn_started"] != 1 {
		t.Fatalf("duplicate replay: %v", got)
	}
}

func TestRemoteRunnerSleeperCleanupRetriesAfterReconnect(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply ready")
	waitFor(t, "worker ready", 5*time.Second, f.has("turn_ended", 1))
	f.run.Close()
	// A crash after the durable state transition, before end_host.
	if _, err := f.st.DB.Exec(`UPDATE sessions SET state='asleep' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	cfg := f.run.Remotes["build"]
	open := cfg.Open
	var away atomic.Bool
	away.Store(true)
	cfg.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		if away.Load() {
			return nil, nil, errors.New("isolated link is away")
		}
		return open(ctx)
	}
	f.run.Remotes["build"] = cfg
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	away.Store(false)
	remote, err := next.remoteSession(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "named sleeper cleanup after reconnect", 8*time.Second, func() bool { l, err := remote.Alive(context.Background()); return err == nil && l.Ended() })
}

func TestRemoteRunnerConfirmedNotSentCanRetry(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply ready")
	waitFor(t, "initial turn", 5*time.Second, f.has("turn_ended", 1))
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	f.run.Close()
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs(id,session_id,data) VALUES(42,1,'{"text":"reply retried"}')`); err != nil {
		t.Fatal(err)
	}
	remote := lv.io.(*machineio.Remote)
	open := remote.Open
	calls := 0
	remote.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		calls++
		if calls == 2 {
			return nil, nil, errors.New("connection refused before any request")
		}
		return open(ctx)
	}
	a, err := lv.codec.Input(agent.Input{ID: "42", Text: "reply retried"})
	if err != nil {
		t.Fatal(err)
	}
	var nothing *holder.NotSent
	if err := f.run.act(lv, a); !errors.As(err, &nothing) {
		t.Fatalf("missing no-write proof: %v", err)
	}
	var written int
	f.st.DB.QueryRow(`SELECT written FROM pending_inputs WHERE id=42`).Scan(&written)
	if written != 0 {
		t.Fatalf("confirmed unwritten input cannot retry: %d", written)
	}
	remote.Open = open
	if err := f.run.act(lv, a); err != nil {
		t.Fatal(err)
	}
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one confirmed retry", 8*time.Second, f.has("turn_ended", 2))
	if got := counts(t, f.st); got["turn_started"] != 2 || got["turn_ended"] != 2 {
		t.Fatalf("repeated retry: %v", got)
	}
}

type unwrittenUnknown struct {
	net.Conn
	dropped bool
	writes  *atomic.Int64
}

func (c *unwrittenUnknown) Write(b []byte) (int, error) {
	var m workerproto.Message
	if json.Unmarshal(b, &m) == nil && m.Op == "input" {
		var in workerproto.Input
		json.Unmarshal(m.Body, &in)
		if bytes.Contains(in.Data, []byte("unknown delivery")) {
			c.dropped = true
			c.writes.Add(1)
			return len(b), nil
		}
	}
	return c.Conn.Write(b)
}
func (c *unwrittenUnknown) Read(b []byte) (int, error) {
	if c.dropped {
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Read(b)
}

func TestRemoteRunnerUnknownUnreportedInputFailsVisibly(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply ready")
	waitFor(t, "initial turn", 5*time.Second, f.has("turn_ended", 1))
	cfg := f.run.Remotes["build"]
	open := cfg.Open
	var writes atomic.Int64
	cfg.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		c, p, err := open(ctx)
		if err != nil {
			return nil, nil, err
		}
		return &unwrittenUnknown{Conn: c, writes: &writes}, p, nil
	}
	f.run.Remotes["build"] = cfg
	// Existing followers retain their verified transport object.
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	lv.mu.Lock()
	lv.io.(*machineio.Remote).Open = cfg.Open
	lv.mu.Unlock()
	if err := owner(f.core).Send(context.Background(), thread, "unknown", control.Input{Text: "reply unknown delivery"}, control.Deliver); !errors.Is(err, machineio.ErrUnknown) {
		t.Fatalf("unknown delivery: %v", err)
	}
	waitFor(t, "ambiguous attempted write", 5*time.Second, func() bool { return writes.Load() == 1 })
	f.run.Close()
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "visible not-delivered outcome", 8*time.Second, func() bool { return lastTurnReason(t, f.st) == "not delivered" })
	if writes.Load() != 1 {
		t.Fatalf("unknown paste repeated %d times", writes.Load())
	}
}

func TestRemoteRunnerOversizedInputFailsOnceWithoutRetry(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "reply ready")
	waitFor(t, "initial turn", 5*time.Second, f.has("turn_ended", 1))
	err := owner(f.core).Send(context.Background(), thread, "oversized", control.Input{Text: strings.Repeat("x", workerproto.MaxChunk+1)}, control.Deliver)
	if err == nil || !strings.Contains(err.Error(), "encoded bytes") {
		t.Fatalf("missing size refusal: %v", err)
	}
	f.run.Close()
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one visible size failure", 8*time.Second, func() bool { return lastTurnReason(t, f.st) == "not delivered" })
	var inputs, failures int
	f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='input'`).Scan(&inputs)
	f.st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind='turn_ended' AND json_extract(data,'$.reason')='not delivered'`).Scan(&failures)
	if inputs != 1 || failures != 1 {
		t.Fatalf("oversized input retried: writes=%d failures=%d", inputs, failures)
	}
}

func TestRemoteRunnerRefusedHookGetsNewAttemptAfterRestart(t *testing.T) {
	f, _ := remoteFixture(t, true)
	seedRemote(t, f, 1, "ask colour\nreply original")
	waitFor(t, "held question", 8*time.Second, f.has("request", 1))
	var request string
	if err := f.st.DB.QueryRow(`SELECT handle FROM requests WHERE state='open'`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	remote := lv.io.(*machineio.Remote)
	info, err := remote.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.run.Close()
	// Freeze only the main's follower; the owned worker and hook keep running.
	f.run.live = map[int64]*live{1: lv}
	defer func() {
		f.run.mu.Lock()
		if f.run.live[1] == lv {
			f.run.live = nil
		}
		f.run.mu.Unlock()
	}()
	socket := host.SocketPath(info.StateDir)
	parked := socket + ".fixture"
	parkedNow := false
	t.Cleanup(func() {
		if parkedNow {
			os.Rename(parked, socket)
		}
	})
	open := remote.Open
	remote.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		c, p, err := open(ctx)
		if err != nil {
			return nil, nil, err
		}
		return &transformReply{Conn: c, reader: bufio.NewReader(c), change: func(m workerproto.Message) workerproto.Message {
			if m.Op == "describe" && m.Kind == "reply" && !parkedNow {
				if err := os.Rename(socket, parked); err != nil {
					t.Error(err)
				} else {
					parkedNow = true
				}
			}
			return m
		}}, p, nil
	}
	var nothing *holder.NotSent
	if err := owner(f.core).Answer(context.Background(), control.RequestID(request), control.Answer{Text: "blue"}); !errors.As(err, &nothing) {
		t.Fatalf("expected durable pre-write refusal: %v", err)
	}
	if err := os.Rename(parked, socket); err != nil {
		t.Fatal(err)
	}
	parkedNow = false
	remote.Open = open
	f.run.live = nil
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 30 * time.Millisecond, Remotes: f.run.Remotes}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "held answer after confirmed refusal", 8*time.Second, func() bool { return withText(t, f.st, "text", "original") == 1 })
	var refused, complete int
	f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='answer' AND state='refused'`).Scan(&refused)
	f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='answer' AND state='complete'`).Scan(&complete)
	if refused != 1 || complete != 1 {
		var states string
		f.st.DB.QueryRow(`SELECT group_concat(operation||':'||state||':'||request) FROM session_machine_actions`).Scan(&states)
		t.Fatalf("answer attempts: refused=%d complete=%d, actions=%s, events=%v", refused, complete, states, f.eventData("turn_ended"))
	}
}

func TestPlacementCoreSelectsWorkerAndWaitsWithoutLocalFallback(t *testing.T) {
	f, resident := remoteFixture(t, false)
	ctx := context.Background()
	f.core.Placement = &core.MachinePlacement{Machines: map[string]core.PlacementMachine{"build": {Limits: placement.Limits{MaxResident: 1}}}, Observe: func(context.Context, string, string) (placement.Observation, error) {
		now := time.Now()
		return placement.Observation{Ready: true, Reconciled: true, Known: true, Installed: true, LoggedIn: true, At: now, HealthAt: now, ValidFor: 10 * time.Second, Memory: 1 << 30, Disk: 1 << 30}, nil
	}}
	f.run.Admit = f.core.AdmitSession
	remote := f.run.Remotes["build"]
	remote.Admission = &workerproto.Admission{MaxResident: 1}
	f.run.Remotes["build"] = remote
	id, err := owner(f.core).Start(ctx, control.StartRequest{Machine: "build", Scope: "s", Thread: "placement-explicit", Key: "placement-explicit", Repo: "fixture", Input: control.Input{Text: "reply placed"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "placed worker result", 8*time.Second, func() bool { return withText(t, f.st, "text", "placed") == 1 })
	info, err := owner(f.core).Thread(ctx, id, 0, 0)
	if err != nil || info.Machine != "build" || info.PlacementPending {
		t.Fatal(info, err)
	}
	request := control.StartRequest{Scope: "s", Thread: "placement-auto", Key: "placement-auto", Repo: "fixture", Input: control.Input{Text: "reply queued"}}
	pending, err := owner(f.core).Start(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := owner(f.core).Start(ctx, request)
	if err != nil || again != pending {
		t.Fatal(again, pending, err)
	}
	info, err = owner(f.core).Thread(ctx, pending, 0, 0)
	if err != nil || !info.PlacementPending || info.Machine != "" {
		t.Fatal(info, err)
	}
	var runs int
	if err := f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatal(runs, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "work")); !os.IsNotExist(err) {
		t.Fatal("worker placement touched main workspace", err)
	}
	remoteSession, err := f.run.remoteSession(ctx, int64(id))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "placed turn completed", 8*time.Second, f.has("turn_ended", 1))
	closer := f.core.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all | control.OpClose | control.OpReopen})
	if err := closer.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "confirmed worker end", 8*time.Second, func() bool { l, err := remoteSession.Alive(ctx); return err == nil && l.Ended() })
	reconcilePlacementFixture(t, f, resident)
	if err := f.core.DispatchPlacements(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "oldest queued start on worker", 8*time.Second, func() bool { return withText(t, f.st, "text", "queued") == 1 })
	info, err = owner(f.core).Thread(ctx, pending, 0, 0)
	if err != nil || info.PlacementPending || info.Machine != "build" {
		t.Fatal(info, err)
	}
	// Reopen keeps the old machine. With the queued thread occupying its
	// only slot, the next wake waits there without another clone or host.
	if _, err := closer.ReopenThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := owner(f.core).Send(ctx, id, "wake-full", control.Input{Text: "reply wake-after-room"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	info, err = owner(f.core).Thread(ctx, id, 0, 0)
	if err != nil || !info.PlacementPending || info.Machine != "build" {
		t.Fatal(info, err)
	}

	if err := closer.CloseThread(ctx, pending); err != nil {
		t.Fatal(err)
	}
	pendingRemote, err := f.run.remoteSession(ctx, int64(pending))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queued worker ended", 8*time.Second, func() bool { l, e := pendingRemote.Alive(ctx); return e == nil && l.Ended() })
	reconcilePlacementFixture(t, f, resident)
	if err := f.core.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reopened worker after room", 8*time.Second, func() bool { return withText(t, f.st, "text", "wake-after-room") == 1 })
	if slept, err := f.run.Sleep(ctx, id); err != nil || !slept {
		t.Fatal("placed sleep", slept, err)
	}
	waitFor(t, "sleep worker ended", 8*time.Second, func() bool {
		r, e := f.run.remoteSession(ctx, int64(id))
		if e != nil {
			return false
		}
		l, e := r.Alive(ctx)
		return e == nil && l.Ended()
	})
	reconcilePlacementFixture(t, f, resident)
	if _, err := closer.ReopenThread(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := owner(f.core).Send(ctx, pending, "refill", control.Input{Text: "reply refill"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refilled worker", 8*time.Second, func() bool { return withText(t, f.st, "text", "refill") == 1 })
	if err := owner(f.core).Send(ctx, id, "sleep-wake-full", control.Input{Text: "reply sleep-wake-capacity"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	info, err = owner(f.core).Thread(ctx, id, 0, 0)
	if err != nil || !info.PlacementPending || info.Machine != "build" {
		t.Fatal("sleep wake moved or ignored capacity", info, err)
	}
	// An unavailable original conversation causes a replacement only after
	// room returns; every session still shares the original worker handle.
	var conversation string
	f.st.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE thread_id=? ORDER BY id DESC LIMIT 1`, int64(id)).Scan(&conversation)
	if err := os.Remove(filepath.Join(f.run.Remotes["build"].Root, "conversations", conversation)); err != nil {
		t.Fatal(err)
	}
	if err := closer.CloseThread(ctx, pending); err != nil {
		t.Fatal(err)
	}
	pendingRemote, err = f.run.remoteSession(ctx, int64(pending))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refill ended", 8*time.Second, func() bool { l, e := pendingRemote.Alive(ctx); return e == nil && l.Ended() })
	reconcilePlacementFixture(t, f, resident)
	if err := f.core.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "placed replacement after capacity", 10*time.Second, func() bool { return withText(t, f.st, "text", "sleep-wake-capacity") == 1 })
	var handles, machines int
	if err := f.st.DB.QueryRow(`SELECT count(DISTINCT x.workspace),count(DISTINCT x.machine) FROM session_machine_runs x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, int64(id)).Scan(&handles, &machines); err != nil || handles != 1 || machines != 1 {
		t.Fatal("replacement relocated", handles, machines, err)
	}

}

func reconcilePlacementFixture(t *testing.T, f *fixture, resident *worker.Resident) {
	t.Helper()
	ctx := context.Background()
	remote := f.run.Remotes["build"]
	conn, protocol, err := remote.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := workerproto.Message{Kind: "request", ID: "fixture-inventory", Binding: remote.Binding, Op: "inventory", Body: json.RawMessage(`{}`)}
	data, err := workerproto.Encode(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	data, err = bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.Decode(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	var inv workerproto.Inventory
	if err := json.Unmarshal(reply.Body, &inv); err != nil {
		t.Fatal(err)
	}
	if !inv.Complete {
		t.Fatal("fixture inventory incomplete")
	}
	var runs []store.MachineRunObservation
	for _, h := range inv.Hosts {
		runs = append(runs, store.MachineRunObservation{Thread: int64(h.Target.Thread), Session: h.Target.Session, Run: h.Target.Run, State: h.State})
	}
	if err := f.st.Update(ctx, func(tx *store.Tx) error { return tx.ObserveMachineRuns(ctx, "build", runs) }); err != nil {
		t.Fatal(err)
	}
}

func TestPlacementLostWorkerStartReplyKeepsReservation(t *testing.T) {
	for _, cancelLabel := range []bool{false, true} {
		t.Run(fmt.Sprint("canceled-label-", cancelLabel), func(t *testing.T) {
			f, _ := remoteFixture(t, false)
			ctx := context.Background()
			f.core.Placement = &core.MachinePlacement{Machines: map[string]core.PlacementMachine{"build": {Limits: placement.Limits{MaxResident: 1}}}, Observe: func(context.Context, string, string) (placement.Observation, error) {
				now := time.Now()
				return placement.Observation{Ready: true, Reconciled: true, Known: true, Installed: true, LoggedIn: true, At: now, HealthAt: now, ValidFor: 10 * time.Second, Memory: 1 << 30, Disk: 1 << 30}, nil
			}}
			f.run.Admit = f.core.AdmitSession
			remote := f.run.Remotes["build"]
			remote.Admission = &workerproto.Admission{MaxResident: 1}
			open := remote.Open
			var lost atomic.Bool
			remote.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
				c, p, err := open(ctx)
				if err != nil {
					return nil, nil, err
				}
				return &transformReply{Conn: c, reader: bufio.NewReaderSize(c, workerproto.MaxFrame), change: func(m workerproto.Message) workerproto.Message {
					if m.Op == "start" && m.Kind == "reply" && lost.CompareAndSwap(false, true) {
						m.Kind = "refusal"
						m.Body = json.RawMessage(`{"code":"unavailable","message":"reply lost after start"}`)
					}
					return m
				}}, p, nil
			}
			f.run.Remotes["build"] = remote
			id, err := owner(f.core).Start(ctx, control.StartRequest{Machine: "build", Scope: "s", Thread: "lost-start", Key: "lost-start", Repo: "fixture", Input: control.Input{Text: "reply original-start"}})
			if err != nil {
				t.Fatal(err)
			}
			info, err := owner(f.core).Thread(ctx, id, 0, 0)
			if err != nil || !info.PlacementPending || !lost.Load() {
				t.Fatal(info, err, lost.Load())
			}
			reservations, err := f.st.MachineReservations(ctx)
			if err != nil || len(reservations) != 1 {
				t.Fatal(reservations, err)
			}
			if cancelLabel {
				if _, err := f.st.DB.Exec(`UPDATE sessions SET io_error=? WHERE thread_id=?`, core.PlacementCanceledReason, int64(id)); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.core.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "original start receipt recovery", 8*time.Second, func() bool { return withText(t, f.st, "text", "original-start") == 1 })
			got := counts(t, f.st)
			if got["turn_started"] != 1 || got["turn_ended"] != 1 {
				t.Fatal("lost reply duplicated worker", got)
			}
			info, err = owner(f.core).Thread(ctx, id, 0, 0)
			if err != nil || info.PlacementPending || info.IOPaused != "" || info.Machine != "build" {
				t.Fatal(info, err)
			}
			var starts int
			if err := f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='start'`).Scan(&starts); err != nil || starts != 1 {
				t.Fatal("start intent was replaced after unknown reply", starts, err)
			}
		})
	}
}
