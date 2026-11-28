package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/worker"
	"github.com/yousan/hataori/internal/workerproto"
)

// This fixture has a real main process, fake chat bridge, system SSH forwarding,
// resident and detached fake-agent hosts. It starts no tmux or user service.
type firstWorker struct {
	t       *testing.T
	main    *procRoot
	root    string
	repo    string
	binding workerproto.Binding
	options worker.Options
	worker  *worker.Resident
	proxy   *firstWorkerProxy
	cancel  context.CancelFunc
	done    chan error
	boot    atomic.Uint64
}

// firstWorkerSSH owns every key/config and a foreground server PID. Missing
// prerequisites fail this gate rather than silently skipping a platform.
func firstWorkerSSH(t *testing.T) string {
	t.Helper()
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("first-worker gate requires system OpenSSH")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Fatal("first-worker gate requires ssh-keygen")
	}
	const server = "/usr/sbin/sshd"
	if info, err := os.Stat(server); err != nil || info.Mode()&0111 == 0 {
		t.Fatal("first-worker gate requires its own executable sshd")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDir(t)
	write := func(name, text string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"host", "client"} {
		if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "first-worker-fixture", "-f", filepath.Join(dir, key)).CombinedOutput(); err != nil {
			t.Fatalf("fixture keygen: %v %s", err, out)
		}
	}
	public, err := os.ReadFile(filepath.Join(dir, "client.pub"))
	if err != nil {
		t.Fatal(err)
	}
	write("authorized_keys", string(public), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	write("server.conf", strings.Join([]string{
		fmt.Sprintf("ListenAddress 127.0.0.1:%d", port),
		"HostKey " + filepath.Join(dir, "host"),
		"AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"),
		"PidFile " + filepath.Join(dir, "server.pid"),
		"UsePAM no", "PasswordAuthentication no", "KbdInteractiveAuthentication no",
		"PubkeyAuthentication yes", "StrictModes no", "LogLevel ERROR",
		"PermitUserEnvironment no", "AllowAgentForwarding no", "X11Forwarding no",
	}, "\n")+"\n", 0600)
	log, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(server, "-D", "-e", "-f", filepath.Join(dir, "server.conf"))
	command.Stderr = log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			command.Process.Kill()
			<-done
		}
		log.Close()
	})
	eventually(t, "first-worker fixture SSH listener", 10*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	public, err = os.ReadFile(filepath.Join(dir, "host.pub"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(public))
	write("known_hosts", fmt.Sprintf("[127.0.0.1]:%d %s %s\n", port, fields[0], fields[1]), 0600)
	write("client.conf", strings.Join([]string{
		"Host first-worker", "  HostName 127.0.0.1", fmt.Sprintf("  Port %d", port),
		"  User " + me.Username, "  IdentityFile " + filepath.Join(dir, "client"),
		"  IdentitiesOnly yes", "  IdentityAgent none", "  GlobalKnownHostsFile /dev/null",
		"  UserKnownHostsFile " + filepath.Join(dir, "known_hosts"),
		"  StrictHostKeyChecking yes", "  BatchMode yes",
	}, "\n")+"\n", 0600)
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	write("bin/ssh", "#!/bin/sh\nexec "+quoteSh(ssh)+" -F "+quoteSh(filepath.Join(dir, "client.conf"))+" \"$@\"\n", 0700)
	return filepath.Join(dir, "bin")
}

func newFirstWorker(t *testing.T, configure ...func(*fake.Config)) *firstWorker {
	t.Helper()
	bin := firstWorkerSSH(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	f := &firstWorker{t: t, root: filepath.Join(shortDir(t), "worker"), repo: bareRepo(t)}
	f.boot.Store(1)
	f.main = newProcRoot(t, func(cfg map[string]any) {
		cfg["machine_away_grace"] = "25ms"
		cfg["owner"] = "owner"
		cfg["people"] = []map[string]any{{"name": "owner", "ids": []string{remoteSurface + ":" + e2eOwner}}, {"name": "dispatch", "ids": []string{remoteSurface + ":" + e2eHook}}}
		delete(cfg["scopes"].([]map[string]any)[0], "posters")
		cfg["scopes"].([]map[string]any)[0]["people"] = []string{"dispatch"}
		cfg["adapters"] = map[string]any{"fake": fake.Config{Command: []string{os.Args[0], "fake"}, Hataori: os.Args[0], Terminal: true, Env: []string{"PATH=" + os.Getenv("PATH")}}}
		cfg["scopes"].([]map[string]any)[0]["machines"] = []string{"build"}
		cfg["scopes"] = append(cfg["scopes"].([]map[string]any), map[string]any{"key": "excluded", "client": remoteSurface, "enabled": true, "machines": []string{}})
		cfg["machines"] = []map[string]any{
			{"key": "local", "agents": []string{}},
			{"key": "build", "ssh": "first-worker", "root": f.root, "agents": []string{"fake"}, "max_resident": 2},
		}
	})
	limits := fakesurface.DefaultLimits
	limits.Commands = true
	f.main.driver.SetLimits(limits)
	if err := os.Mkdir(filepath.Join(f.main.root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), f.main.root)
	if err != nil {
		t.Fatal(err)
	}
	mainID, err := st.MainIdentity(context.Background())
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.binding, err = worker.Initialize(f.root, mainID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.main.root, instance.ConfigFile)
	var cfg map[string]any
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatal("read isolated main configuration", err)
	}
	cfg["machines"].([]any)[1].(map[string]any)["installation"] = f.binding.Worker
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"records", "conversations"} {
		if err := os.Mkdir(filepath.Join(f.root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	workerAgent := fake.Config{Command: []string{os.Args[0], "fake"}, Hataori: os.Args[0], Terminal: true, Record: filepath.Join(f.root, "records"), Conversations: filepath.Join(f.root, "conversations"), Env: []string{"PATH=" + os.Getenv("PATH")}}
	for _, change := range configure {
		change(&workerAgent)
	}
	settings, _ := json.Marshal(workerAgent)
	f.options = worker.Options{
		Config:      &worker.Config{Agents: []worker.Configuration{{Agent: "fake", Name: "default", Settings: settings}}, Workspaces: worker.WorkspaceConfig{Machine: "fixture", MainMachine: "fixture", Repositories: map[string]string{"project": f.repo}, RootsComplete: true, Roots: []worker.RootLocation{{Machine: "fixture", Path: f.root}, {Machine: "fixture", Path: f.main.root}}, ProofDestinations: []worker.ProofDestination{{Address: f.repo, Machine: "fixture", Path: strings.TrimPrefix(f.repo, "file://"), Independent: true}}, RetentionSeconds: 1}},
		HostCommand: host.Command{Path: os.Args[0], Args: []string{"host"}, Env: []string{}},
		NewAdapter:  func(_ string, settings []byte) (agent.Adapter, error) { return fake.New(settings) },
		BootID:      func() (string, error) { return fmt.Sprint(f.boot.Load()), nil },
		Telemetry: func() workerproto.Telemetry {
			return workerproto.Telemetry{Known: true, ObservedMS: time.Now().UnixMilli(), ValidMS: 10000, MemoryFree: 1 << 30, DiskFree: 1 << 30}
		},
	}
	f.startResident()
	t.Cleanup(func() {
		f.main.stopDaemon()
		killSessions(f.root) // Only PID identity records in this fixture's worker root.
		f.stopResident()
		if t.Failed() {
			f.main.logMu.Lock()
			log := f.main.log.String()
			if len(log) > 6000 {
				log = log[:3000] + "\n[repeated fixture log truncated]\n" + log[len(log)-3000:]
			}
			t.Log(log)
			f.main.logMu.Unlock()
		}
	})
	f.main.start(t)
	db := f.main.db(t)
	eventually(t, "first-worker ready and reconciled", 20*time.Second, func() bool {
		var ready int
		err := db.QueryRow(`SELECT count(*) FROM machine_health h JOIN machine_agent_health a ON a.machine=h.machine WHERE h.machine='build' AND h.reconciled=1 AND a.agent='fake' AND a.state='logged_in'`).Scan(&ready)
		return err == nil && ready == 1
	})
	return f
}

func (f *firstWorker) startResident() {
	f.t.Helper()
	r, err := worker.Open(f.root, f.binding, f.options)
	if err != nil {
		f.t.Fatal(err)
	}
	f.worker = r
	path, err := r.Listen()
	if err != nil {
		r.Close()
		f.t.Fatal(err)
	}
	f.proxy = newFirstWorkerProxy(f.t, path)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan error, 1)
	done := f.done
	go func() { done <- r.Serve(ctx) }()
}

func (f *firstWorker) stopResident() {
	if f.worker == nil {
		return
	}
	f.cancel()
	f.proxy.close()
	f.worker.Close()
	if err := <-f.done; err != nil {
		f.t.Error(err)
	}
	f.worker = nil
	f.proxy.removeActual()
}

func TestFirstWorkerPlacementChatAndDirectoryOwnership(t *testing.T) {
	f := newFirstWorker(t)
	p := f.main
	owner := fakesurface.Person(e2eOwner)
	// Automatic chat placement and an explicit /new use the same worker path.
	p.driver.Inject(c1, owner, "!go repo:"+f.repo+" reply automatic worker")
	automatic := p.thread(t, 0)
	p.posted(t, automatic, "automatic worker")
	p.driver.InjectCommand(c1, owner, "new", surface.Arg{Name: "repository", Value: f.repo}, surface.Arg{Name: "machine", Value: "build"}, surface.Arg{Name: "prompt", Value: "reply explicit worker"})
	explicit := p.thread(t, 1)
	p.posted(t, explicit, "explicit worker")
	for _, conv := range []surface.Conversation{automatic, explicit} {
		p.driver.Inject(conv, owner, "reply worker followup")
		p.posted(t, conv, "worker followup")
		p.driver.Inject(conv, owner, "ask colour\nreply answered worker")
		p.postedWith(t, conv, "colour")
		p.driver.Inject(conv, owner, "blue")
		p.posted(t, conv, "answered worker")
	}
	db := p.db(t)
	var threads, directories int
	if err := db.QueryRow(`SELECT count(*),count(DISTINCT workspace) FROM threads WHERE machine='build' AND workspace!=''`).Scan(&threads, &directories); err != nil || threads != 2 || directories != 2 {
		t.Fatalf("one worker directory per thread: %d %d %v", threads, directories, err)
	}
	before := f.workspaceIdentities()
	if len(before) != 2 {
		t.Fatalf("actual worker directories: %d", len(before))
	}
	p.restart(t)
	f.sameWorkspaces(before)
	for _, root := range []string{f.root, p.root} {
		var databases []string
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".db") {
				databases = append(databases, path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if root == f.root && len(databases) != 0 || root == p.root && len(databases) != 1 {
			t.Fatalf("main must own the only database: %v", databases)
		}
	}
	if hosts, err := filepath.Glob(filepath.Join(p.root, "state/sessions/*", host.HostPIDFile)); err != nil || len(hosts) != 0 {
		t.Fatalf("worker work launched a main host: %v %v", hosts, err)
	}
	if entries, err := os.ReadDir(filepath.Join(f.root, "config")); err != nil || len(entries) != 0 {
		t.Fatalf("worker received a main chat configuration: %v %v", entries, err)
	}
	t.Log("ran: fake chat -> real main -> fixture system SSH -> resident -> fake terminal agent; automatic/explicit start, follow-up, question/answer, two stable worker directories, main-only database/chat configuration")
}

func (f *firstWorker) workspaceIdentities() map[string]os.FileInfo {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, "workspaces"))
	if err != nil {
		f.t.Fatal(err)
	}
	result := map[string]os.FileInfo{}
	for _, entry := range entries {
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				f.t.Fatal(err)
			}
			result[entry.Name()] = info
		}
	}
	if len(result) == 0 {
		f.t.Fatal("no actual worker workspace directory")
	}
	return result
}
func (f *firstWorker) sameWorkspaces(before map[string]os.FileInfo) {
	f.t.Helper()
	after := f.workspaceIdentities()
	if len(before) != len(after) {
		f.t.Fatalf("worker directories changed: %d %d", len(before), len(after))
	}
	for name, info := range before {
		if after[name] == nil || !os.SameFile(info, after[name]) {
			f.t.Fatalf("worker directory moved or replaced: %s", name)
		}
	}
}
