package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

type Configuration struct {
	Agent             string          `json:"agent"`
	Name              string          `json:"configuration"`
	Settings          json.RawMessage `json:"settings"`
	ConfigDir         string          `json:"config_dir,omitempty"`
	NoProjectSettings bool            `json:"no_project_settings,omitempty"`
}

type Config struct {
	Agents []Configuration `json:"agents"`
}

type Options struct {
	// Config, when nil, is read only from this worker's config/worker.json.
	Config      *Config
	HostCommand host.Command
	NewAdapter  func(string, []byte) (agent.Adapter, error)
	Now         func() time.Time
	BootID      func() (string, error)
	Telemetry   func() workerproto.Telemetry
	// checkpoint is only a fault-injection point in isolated tests. Returning
	// an error simulates loss at a crash boundary; recovery must reconcile it.
	checkpoint func(string) error
}

type runRecord struct {
	Target       workerproto.Target      `json:"target"`
	Identity     string                  `json:"identity"`
	Boot         string                  `json:"boot"`
	State        string                  `json:"state"`
	Agent        workerproto.AgentConfig `json:"agent"`
	Workspace    string                  `json:"workspace,omitempty"`
	Turn         string                  `json:"turn,omitempty"`
	Instructions string                  `json:"instructions,omitempty"`
	Policy       []string                `json:"policy,omitempty"`
}

type ownerMarker struct {
	Version  int                 `json:"version"`
	Binding  workerproto.Binding `json:"binding"`
	Target   workerproto.Target  `json:"target"`
	Identity string              `json:"identity"`
}

type workspaceRecord struct {
	Target workerproto.Target `json:"target"`
	Handle string             `json:"handle"`
	Device uint64             `json:"device"`
	Inode  uint64             `json:"inode"`
}

type receiptRecord struct {
	Request workerproto.Message `json:"request"`
	Receipt workerproto.Receipt `json:"receipt"`
}

type journal struct {
	Version    int                        `json:"version"`
	Binding    workerproto.Binding        `json:"binding"`
	Runs       map[string]*runRecord      `json:"runs"`
	Receipts   map[string]*receiptRecord  `json:"receipts"`
	Workspaces map[string]workspaceRecord `json:"workspaces"`
}

func emptyJournal(binding workerproto.Binding) journal {
	return journal{Version: 1, Binding: binding, Runs: map[string]*runRecord{}, Receipts: map[string]*receiptRecord{}, Workspaces: map[string]workspaceRecord{}}
}

// Resident owns one root. Closing it releases only the resident lock/socket;
// detached hosts, their logs and waiting hooks are deliberately left alive.
type Resident struct {
	mu          sync.Mutex
	root        *os.Root
	dir         string
	lock        *os.File
	binding     workerproto.Binding
	boot        string
	state       journal
	options     Options
	configs     map[workerproto.AgentConfig]Configuration
	adapters    map[workerproto.AgentConfig]agent.Adapter
	ready       bool
	closed      bool
	listener    *net.UnixListener
	socket      string
	socketInfo  os.FileInfo
	connections map[net.Conn]bool
}

func Open(path string, expected workerproto.Binding, options Options) (_ *Resident, err error) {
	root, physical, err := openRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			root.Close()
		}
	}()
	var reg registration
	if err = readPrivate(root, registrationFile, 4096, &reg); err != nil {
		return nil, err
	}
	if reg.Version != 1 || reg.Binding != expected || !workerproto.ValidID(expected.Main) || !workerproto.ValidID(expected.Worker) || !rootIdentity(root, reg) {
		return nil, ErrBinding
	}
	for _, dir := range []string{"state/worker", "state/sessions", "config", "workspaces"} {
		if err = privateDir(root, dir); err != nil {
			return nil, err
		}
	}
	lk, err := takeLock(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			releaseLock(lk)
		}
	}()
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.BootID == nil {
		options.BootID = bootID
	}
	if options.NewAdapter == nil {
		options.NewAdapter = func(name string, b []byte) (agent.Adapter, error) {
			f, ok := agent.Lookup(name)
			if !ok {
				return nil, errors.New("worker: adapter is not installed in this binary")
			}
			return f(b)
		}
	}
	if options.Telemetry == nil {
		options.Telemetry = func() workerproto.Telemetry { return resourceTelemetry(physical, options.Now()) }
	}
	r := &Resident{root: root, dir: physical, lock: lk, binding: expected, options: options,
		configs: map[workerproto.AgentConfig]Configuration{}, adapters: map[workerproto.AgentConfig]agent.Adapter{}, connections: map[net.Conn]bool{}}
	if r.boot, err = options.BootID(); err != nil || !workerproto.ValidID(r.boot) {
		return nil, errors.New("worker: boot identity unavailable")
	}
	if err = readPrivate(root, journalFile, maxJournal, &r.state); err != nil {
		return nil, err
	}
	if r.state.Version != 1 || r.state.Binding != expected || r.state.Runs == nil || r.state.Receipts == nil || r.state.Workspaces == nil {
		return nil, ErrBinding
	}
	var cfg Config
	if options.Config != nil {
		cfg = *options.Config
	} else if err = readPrivate(root, "config/worker.json", workerproto.MaxFrame, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Agents) > 32 {
		return nil, errors.New("worker: at most 32 agent configurations")
	}
	for _, c := range cfg.Agents {
		key := workerproto.AgentConfig{Agent: c.Agent, Config: c.Name}
		if !workerproto.ValidID(c.Agent) || !workerproto.ValidID(c.Name) || c.ConfigDir != "" && !filepath.IsAbs(c.ConfigDir) {
			return nil, errors.New("worker: invalid local agent configuration")
		}
		if _, ok := r.configs[key]; ok {
			return nil, errors.New("worker: duplicate agent configuration")
		}
		a, e := options.NewAdapter(c.Agent, c.Settings)
		if e != nil {
			return nil, e
		}
		local, ok := a.(agent.WorkerAdapters)
		if !ok {
			return nil, errors.New("worker: adapter cannot isolate its environment")
		}
		r.configs[key], r.adapters[key] = c, local.ForWorker()
	}
	if options.HostCommand.Path == "" {
		return nil, errors.New("worker: no installed host command")
	}
	if err = r.validateJournal(); err != nil {
		return nil, err
	}
	if err = r.recover(); err != nil {
		return nil, err
	}
	return r, nil
}

func targetKey(t workerproto.Target) string {
	b, _ := json.Marshal(t)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func receiptKey(t workerproto.Target, op, key string) string {
	b, _ := json.Marshal([]string{targetKey(t), op, key})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (r *Resident) runDir(t workerproto.Target) string {
	return filepath.Join("state/sessions", targetKey(t))
}
func (r *Resident) hostDir(t workerproto.Target) string { return filepath.Join(r.dir, r.runDir(t)) }

func (r *Resident) validateJournal() error {
	threads := map[uint64]bool{}
	for key, run := range r.state.Runs {
		if run == nil || workerproto.CheckTarget(run.Target, run.Target) != nil || key != targetKey(run.Target) || !workerproto.ValidID(run.Identity) || !workerproto.ValidID(run.Boot) {
			return errors.New("worker: invalid owned-run journal")
		}
		if run.State == "ended" || run.State == "gone" {
			continue
		}
		if threads[run.Target.Thread] {
			return errors.New("worker: thread has conflicting session ownership")
		}
		threads[run.Target.Thread] = true
	}
	for key, rec := range r.state.Receipts {
		if rec == nil || rec.Request.Target == nil || rec.Request.Binding != r.binding || key != receiptKey(*rec.Request.Target, rec.Request.Op, rec.Request.Key) || workerproto.CheckReceipt(rec.Request, rec.Receipt) != nil {
			return errors.New("worker: invalid durable receipt")
		}
	}
	for key, w := range r.state.Workspaces {
		if !workerproto.ValidID(w.Handle) || workerproto.CheckTarget(w.Target, w.Target) != nil || key != targetKey(w.Target)+"-"+w.Handle || w.Inode == 0 {
			return errors.New("worker: invalid owned workspace record")
		}
	}
	return nil
}

func (r *Resident) visibleRoot() error {
	visible, err := os.Stat(r.dir)
	pinned, e := r.root.Stat(".")
	if err != nil || e != nil || !os.SameFile(visible, pinned) || private(visible, true) != nil {
		return errors.New("worker: registered root pathname changed")
	}
	return nil
}

func (r *Resident) checkMarker(run *runRecord) error {
	if err := r.visibleRoot(); err != nil {
		return err
	}
	info, err := r.root.Lstat(r.runDir(run.Target))
	if err != nil {
		return err
	}
	if err := private(info, true); err != nil {
		return err
	}
	var marker ownerMarker
	if err := readPrivate(r.root, filepath.Join(r.runDir(run.Target), "owner.json"), 4096, &marker); err != nil {
		return err
	}
	if marker.Version != 1 || marker.Binding != r.binding || marker.Target != run.Target || marker.Identity != run.Identity {
		return ErrBinding
	}
	return nil
}

func (r *Resident) ownedStatus(run *runRecord) (host.Status, bool, error) {
	if err := r.checkMarker(run); err != nil {
		return host.Status{}, false, err
	}
	var spec host.Spec
	if err := readPrivate(r.root, filepath.Join(r.runDir(run.Target), host.SpecFile), workerproto.MaxFrame, &spec); err != nil {
		return host.Status{}, false, err
	}
	if spec.Identity != run.Identity {
		return host.Status{}, false, ErrBinding
	}
	var status host.Status
	if err := readPrivate(r.root, filepath.Join(r.runDir(run.Target), host.StatusFile), workerproto.MaxFrame, &status); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status, false, nil
		}
		return status, false, err
	}
	if !status.Exited && !status.Stopped && status.Error == "" {
		return status, false, errors.New("worker: incomplete host status")
	}
	return status, true, nil
}

func (r *Resident) save() error { return writeState(r.root, journalFile, r.state, true) }

func (r *Resident) point(name string) error {
	if r.options.checkpoint != nil {
		return r.options.checkpoint(name)
	}
	return nil
}

// owned connects only to our recorded state directory, verifies the identity
// held by the live host, and returns that same connection for any effect.
func (r *Resident) owned(run *runRecord) (*host.Client, error) {
	if err := r.checkMarker(run); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := host.DialIdentity(ctx, r.hostDir(run.Target), run.Identity)
	if err != nil {
		return nil, err
	}
	c.Deadline(time.Now().Add(5 * time.Second))
	return c, nil
}

func (r *Resident) recover() error {
	r.ready = true
	for _, run := range r.state.Runs {
		if err := r.checkMarker(run); err != nil {
			run.State = "unknown"
			r.ready = false
			continue
		}
		if run.State == "prepared" {
			continue
		}
		if run.State == "preparing" {
			run.State = "prepared"
			continue
		}
		c, err := r.owned(run)
		if err == nil {
			_, waitingErr := c.Waiting()
			c.Close()
			if waitingErr != nil {
				run.State = "unknown"
				r.ready = false
			} else {
				run.State = "running"
			}
			continue
		}
		if _, ok, e := r.ownedStatus(run); e == nil && ok {
			run.State = "ended"
		} else if run.Boot != r.boot {
			run.State = "gone"
		} else {
			// Same-boot silence cannot prove absence or authorize another start.
			run.State = "unknown"
			r.ready = false
		}
	}
	for _, rec := range r.state.Receipts {
		if rec.Receipt.State != "pending" {
			continue
		}
		switch rec.Request.Op {
		case "prepare":
			run := r.state.Runs[targetKey(*rec.Request.Target)]
			if run != nil && run.State == "prepared" {
				rec.Receipt.State = "complete"
				rec.Receipt.Result = &workerproto.Effect{}
			} else {
				rec.Receipt.State = "unknown"
				r.ready = false
			}
		case "start":
			run := r.state.Runs[targetKey(*rec.Request.Target)]
			if run != nil && (run.State == "running" || run.State == "ended") {
				rec.Receipt.State = "complete"
				rec.Receipt.Result = &workerproto.Effect{Host: run.Identity}
			} else {
				rec.Receipt.State = "unknown"
				r.ready = false
			}
		case "end_host":
			run := r.state.Runs[targetKey(*rec.Request.Target)]
			if run != nil && (run.State == "ended" || run.State == "gone") {
				rec.Receipt.State = "complete"
				rec.Receipt.Result = &workerproto.Effect{}
			} else {
				rec.Receipt.State = "unknown"
			} // never stop a later turn on recovery
		default:
			rec.Receipt.State = "unknown" // a write may have happened; never replay it
		}
	}
	return r.save()
}

func (r *Resident) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.ready = false
	if r.listener != nil {
		r.listener.Close()
	}
	if r.socketInfo != nil {
		if info, e := os.Lstat(r.socket); e == nil && os.SameFile(info, r.socketInfo) && info.Mode()&os.ModeSocket != 0 {
			os.Remove(r.socket)
		}
	}
	for c := range r.connections {
		c.Close()
	}
	// No host.Close/Stop or process signalling belongs here.
	err := releaseLock(r.lock)
	r.lock = nil
	if e := r.root.Close(); err == nil {
		err = e
	}
	return err
}

func safeEnvironment(env []string) []string {
	var out []string
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "SSH_") || strings.HasPrefix(key, "HATAORI_") && key != host.HookEnvVar {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (r *Resident) probe(ctx context.Context, key workerproto.AgentConfig) workerproto.AgentHealth {
	h := workerproto.AgentHealth{AgentConfig: key, Login: "unknown", Installation: "unknown"}
	if ctx.Err() != nil {
		return h
	}
	a, ok := r.adapters[key]
	if !ok {
		return h
	}
	cfg := r.configs[key]
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	state, err := a.Probe(ctx, agent.Spec{ConfigDir: cfg.ConfigDir, NoProjectSettings: true})
	h.Installed = state.Installed
	if state.Installed {
		h.Installation = "installed"
	}
	if err != nil || state.Unknown || ctx.Err() != nil {
		return h
	}
	if !state.Installed {
		h.Installation = "missing"
		return h
	}
	h.Installation = "installed"
	h.Login = "logged_out"
	if state.LoggedIn {
		h.Login = "logged_in"
	}
	return h
}

func (r *Resident) health(ctx context.Context) workerproto.Health {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	h := workerproto.Health{Boot: r.boot, Ready: r.ready && !r.closed, Agents: []workerproto.AgentHealth{}}
	keys := make([]workerproto.AgentConfig, 0, len(r.adapters))
	for key := range r.adapters {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Agent != keys[j].Agent {
			return keys[i].Agent < keys[j].Agent
		}
		return keys[i].Config < keys[j].Config
	})
	for _, key := range keys {
		h.Agents = append(h.Agents, r.probe(ctx, key))
	}
	h.Telemetry = r.options.Telemetry()
	if !workerproto.FreshTelemetry(h.Telemetry, r.options.Now().UnixMilli()) {
		h.Telemetry = workerproto.Telemetry{ObservedMS: r.options.Now().UnixMilli(), ValidMS: 10000}
	}
	return h
}

func (r *Resident) inventory() workerproto.Inventory {
	i := workerproto.Inventory{Boot: r.boot, Complete: true, Hosts: []workerproto.HostRecord{}}
	for _, run := range r.state.Runs {
		rec := workerproto.HostRecord{Target: run.Target, State: run.State, Turn: run.Turn}
		if rec.State == "prepared" {
			rec.State = "reserved"
		}
		if run.State == "running" {
			c, err := r.owned(run)
			if err != nil {
				if r.ended(run) {
					run.State, rec.State = "ended", "ended"
					if r.save() != nil {
						rec.State = "unknown"
					}
				} else {
					rec.State = "unknown"
				}
				if rec.State == "unknown" {
					i.Complete = false
					r.ready = false
				}
			} else {
				calls, err := c.Waiting()
				c.Close()
				if err != nil {
					rec.State = "unknown"
					i.Complete = false
					r.ready = false
				} else {
					for _, call := range calls {
						rec.Hooks = append(rec.Hooks, call.ID)
					}
				}
			}
		}
		if rec.State == "unknown" {
			i.Complete = false
		}
		i.Hosts = append(i.Hosts, rec)
	}
	sort.Slice(i.Hosts, func(a, b int) bool { return targetKey(i.Hosts[a].Target) < targetKey(i.Hosts[b].Target) })
	return i
}

func failure(code, message string) *workerproto.Refusal {
	return &workerproto.Refusal{Code: code, Message: message}
}

func (r *Resident) String() string { return fmt.Sprintf("worker installation %s", r.binding.Worker) }
