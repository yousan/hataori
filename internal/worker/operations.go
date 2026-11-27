package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

// Dispatch validates an envelope before resolving any host or filesystem
// resource. Preliminary JSON is used only to route to in-memory registrations.
// Only prepare can reserve a fresh target, under the already bound main.
func (r *Resident) Dispatch(ctx context.Context, c *workerproto.Connection, data []byte) workerproto.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var route workerproto.Message
	_ = json.Unmarshal(data, &route)
	reply := workerproto.Message{Kind: "refusal", ID: route.ID, Binding: r.binding, Target: route.Target, Op: route.Op, Key: route.Key}
	if !workerproto.ValidID(reply.ID) {
		reply.ID = "invalid"
	}
	if !workerproto.ValidID(reply.Op) {
		reply.Op = "invalid"
	}
	if !workerproto.ValidID(reply.Key) {
		reply.Key = ""
	}
	deny := func(code, message string) workerproto.Message {
		reply.Kind = "refusal"
		reply.Body, _ = json.Marshal(failure(code, message))
		return reply
	}
	if c == nil || c.Hello().Binding != r.binding {
		return deny("binding", "registered installation binding does not match")
	}
	if r.visibleRoot() != nil {
		r.ready = false
		return deny("unavailable", "registered root pathname changed")
	}
	if r.closed {
		return deny("unavailable", "resident is closed")
	}
	var expected *workerproto.Target
	if route.Target != nil {
		if run := r.state.Runs[targetKey(*route.Target)]; run != nil {
			expected = &run.Target
		} else if route.Op == "prepare" && r.admissible(*route.Target) {
			// This is an explicit creation authority under this registered main,
			// not adoption of an existing host/handle or a peer's claimed record.
			fresh := *route.Target
			expected = &fresh
		}
	}
	m, err := c.Decode(data, expected)
	if err != nil {
		var refusal *workerproto.Refusal
		if errors.As(err, &refusal) {
			return deny(refusal.Code, refusal.Message)
		}
		return deny("invalid_message", "invalid request")
	}
	if m.Kind != "request" {
		return deny("invalid_message", "resident accepts requests only")
	}
	reply.ID, reply.Op, reply.Key, reply.Target = m.ID, m.Op, m.Key, m.Target
	finish := func(value any) workerproto.Message {
		reply.Kind = "reply"
		reply.Body, _ = json.Marshal(value)
		return reply
	}
	switch m.Op {
	case "health":
		return finish(r.health(ctx))
	case "inventory":
		return finish(r.inventory())
	case "probe":
		var key workerproto.AgentConfig
		json.Unmarshal(m.Body, &key)
		if _, ok := r.adapters[key]; !ok {
			return deny("not_found", "agent configuration is not registered")
		}
		return finish(r.probe(ctx, key))
	case "describe":
		info, refusal := r.describe(ctx, *m.Target)
		if refusal != nil {
			return deny(refusal.Code, refusal.Message)
		}
		return finish(info)
	case "follow":
		var b workerproto.Follow
		json.Unmarshal(m.Body, &b)
		source, refusal := r.followSource(*m.Target, b.Source)
		if refusal != nil {
			return deny(refusal.Code, refusal.Message)
		}
		return finish(source)
	case "read":
		var b workerproto.Read
		json.Unmarshal(m.Body, &b)
		result, refusal := r.readSource(*m.Target, b)
		if refusal != nil {
			return deny(refusal.Code, refusal.Message)
		}
		return finish(result)
	case "inspect":
		var b workerproto.Workspace
		json.Unmarshal(m.Body, &b)
		result, refusal := r.inspectWorkspace(ctx, *m.Target, b.Workspace)
		if refusal != nil {
			return deny(refusal.Code, refusal.Message)
		}
		return finish(result)
	case "download":
		var b workerproto.Download
		json.Unmarshal(m.Body, &b)
		result, refusal := r.download(*m.Target, b)
		if refusal != nil {
			return deny(refusal.Code, refusal.Message)
		}
		return finish(result)
	case "receipt":
		var q workerproto.ReceiptQuery
		json.Unmarshal(m.Body, &q)
		rec := r.state.Receipts[receiptKey(*m.Target, q.Operation, q.Key)]
		if rec == nil {
			return deny("not_found", "operation receipt is not retained")
		}
		return finish(rec.Receipt)
	case "prepare", "start", "end_host", "input", "answer", "clone", "fetch", "remove", "restore", "acknowledge", "purge", "upload", "bind_workspace":
		// All supported effects have a durable receipt, even when a reply is lost.
	default:
		return deny("unsupported_operation", "operation is not implemented by this resident")
	}
	hash, err := workerproto.RequestHash(m)
	if err != nil {
		return deny("invalid_message", "invalid durable operation")
	}
	key := receiptKey(*m.Target, m.Op, m.Key)
	if prior := r.state.Receipts[key]; prior != nil {
		if prior.Receipt.RequestHash != hash {
			return deny("key_conflict", "operation key belongs to different content")
		}
		return finish(prior.Receipt)
	}
	if !r.ready {
		return deny("not_ready", "owned hosts have not been reconciled")
	}
	var refusal *workerproto.Refusal
	switch m.Op {
	case "bind_workspace":
		refusal = r.bindWorkspace(m)
	case "clone", "fetch", "remove", "restore", "acknowledge", "purge":
		refusal = r.workspaceOperation(ctx, m)
	case "upload":
		refusal = r.upload(m)
	case "prepare":
		refusal = r.prepare(m)
	case "start":
		refusal = r.start(ctx, m)
	case "end_host":
		refusal = r.end(m)
	case "input", "answer":
		refusal = r.write(m)
	}
	if refusal != nil {
		return deny(refusal.Code, refusal.Message)
	}
	rec := r.state.Receipts[key]
	if rec == nil {
		return deny("unavailable", "durable result is unavailable")
	}
	return finish(rec.Receipt)
}

func (r *Resident) admissible(t workerproto.Target) bool {
	if workerproto.CheckTarget(t, t) != nil {
		return false
	}
	// A target is one immutable run, including after it ends. The receipt
	// lookup handles an idempotent prepare; a new run needs a fresh target.
	if _, exists := r.state.Runs[targetKey(t)]; exists {
		return false
	}
	for _, run := range r.state.Runs {
		if run.Target.Thread == t.Thread && run.State != "ended" && run.State != "gone" {
			return false
		}
	}
	return true
}

func (r *Resident) intent(m workerproto.Message) (*receiptRecord, error) {
	hash, err := workerproto.RequestHash(m)
	if err != nil {
		return nil, err
	}
	rec := &receiptRecord{Request: m, Receipt: workerproto.Receipt{Operation: m.Op, Key: m.Key, RequestHash: hash, State: "pending"}}
	r.state.Receipts[receiptKey(*m.Target, m.Op, m.Key)] = rec
	if err := r.save(); err != nil {
		r.ready = false
		return nil, err
	}
	if err := r.point(m.Op + "_intent"); err != nil {
		r.ready = false
		return nil, err
	}
	return rec, nil
}

func (r *Resident) complete(rec *receiptRecord, result workerproto.Effect) *workerproto.Refusal {
	rec.Receipt.State = "complete"
	rec.Receipt.Result = &result
	if err := r.save(); err != nil {
		rec.Receipt.State = "unknown"
		rec.Receipt.Result = nil
		r.ready = false
		return failure("unavailable", "result could not be committed")
	}
	if err := r.point(rec.Request.Op + "_complete"); err != nil {
		return failure("unavailable", "operation reply was lost")
	}
	return nil
}

func (r *Resident) uncertain(rec *receiptRecord) *workerproto.Refusal {
	rec.Receipt.State = "unknown"
	rec.Receipt.Result = nil
	rec.Receipt.Refusal = nil
	if err := r.save(); err != nil {
		r.ready = false
	}
	return nil // The unknown receipt is the result; never promise a safe retry.
}

func (r *Resident) prepare(m workerproto.Message) *workerproto.Refusal {
	if !r.admissible(*m.Target) {
		return failure("wrong_target", "thread already has a reserved or live run")
	}
	var b workerproto.Prepare
	json.Unmarshal(m.Body, &b)
	if len(b.Policy) > 0 {
		return failure("not_ready", "uploaded policy handles require the transfer gate")
	}
	if len(r.state.Runs) >= 4096 {
		return failure("limit", "owned run retention limit reached")
	}
	run := &runRecord{Target: *m.Target, Identity: newID(), Boot: r.boot, State: "preparing", Instructions: b.Instructions}
	r.state.Runs[targetKey(run.Target)] = run
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "prepare intent could not be committed")
	}
	if err := r.root.Mkdir(r.runDir(run.Target), 0o700); err != nil {
		run.State = "unknown"
		r.ready = false
		return r.uncertain(rec)
	}
	if err := writeState(r.root, filepath.Join(r.runDir(run.Target), "owner.json"), struct {
		Version  int                 `json:"version"`
		Binding  workerproto.Binding `json:"binding"`
		Target   workerproto.Target  `json:"target"`
		Identity string              `json:"identity"`
	}{1, r.binding, run.Target, run.Identity}, false); err != nil {
		run.State = "unknown"
		r.ready = false
		return r.uncertain(rec)
	}
	run.State = "prepared"
	return r.complete(rec, workerproto.Effect{})
}

// ownedWorkspace is populated by worker-local workspace operations (#961).
// A main-supplied handle never becomes a pathname on either machine.
func (r *Resident) ownedWorkspace(t workerproto.Target, handle string) (string, error) {
	w, ok := r.state.Workspaces[targetKey(t)+"-"+handle]
	if !ok || w.Target != t || w.Handle != handle {
		return "", errors.New("worker: workspace handle is not registered for this run")
	}
	name := filepath.Join("workspaces", handle)
	info, err := r.root.Lstat(name)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || st.Uid != uint32(os.Geteuid()) || uint64(st.Dev) != w.Device || uint64(st.Ino) != w.Inode {
		return "", errors.New("worker: workspace identity changed")
	}
	return filepath.Join(r.dir, name), nil
}

func (r *Resident) writeLaunchFiles(t workerproto.Target, files map[string][]byte) error {
	for name, data := range files {
		switch strings.ToLower(name) {
		case "owner.json", host.SpecFile, host.HostPIDFile, host.PIDFile, host.EventsFile, host.TerminalLog, host.StderrLog, host.SocketFile, host.StatusFile, host.LockFile, "host.sock.path":
			return errors.New("worker: adapter cannot overwrite host state")
		}
		if filepath.IsAbs(name) || filepath.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return errors.New("worker: unsafe adapter policy filename")
		}
		full := filepath.Join(r.runDir(t), name)
		if filepath.Dir(name) != "." {
			if err := privateDir(r.root, filepath.Dir(full)); err != nil {
				return err
			}
		}
		if info, err := r.root.Lstat(full); err == nil {
			if private(info, false) != nil {
				return errors.New("worker: policy destination is not private")
			}
			old, err := r.root.ReadFile(full)
			if err != nil || !bytes.Equal(old, data) {
				return errors.New("worker: refusing to overwrite different session data")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		f, err := r.root.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		cerr := f.Close()
		if err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		if err := syncDir(r.root, filepath.Dir(full)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Resident) start(ctx context.Context, m workerproto.Message) *workerproto.Refusal {
	run := r.state.Runs[targetKey(*m.Target)]
	if run == nil || run.State != "prepared" {
		return failure("stale", "run is not a fresh prepared reservation")
	}
	if err := r.checkMarker(run); err != nil {
		return failure("unavailable", "run state identity cannot be established")
	}
	var b workerproto.Start
	json.Unmarshal(m.Body, &b)
	cfg, ok := r.configs[b.AgentConfig]
	if !ok {
		return failure("not_found", "agent configuration is not registered")
	}
	resources := r.options.Telemetry()
	if !workerproto.FreshTelemetry(resources, r.options.Now().UnixMilli()) || resources.MemoryFree == 0 || resources.DiskFree == 0 {
		return failure("unavailable", "resource observation is unknown or stale")
	}
	h := r.probe(ctx, b.AgentConfig)
	if !h.Installed || h.Installation != "installed" || h.Login != "logged_in" {
		return failure("unavailable", "agent installation or login is unavailable")
	}
	workspace, err := r.ownedWorkspace(run.Target, b.Workspace)
	if err != nil {
		return failure("missing", "workspace handle is not registered or its identity changed")
	}
	spec := agent.Spec{Dir: workspace, StateDir: r.hostDir(run.Target), Model: b.Model, ResumeID: b.Resume,
		Instructions: run.Instructions, ConfigDir: cfg.ConfigDir, Effort: b.Effort, Approvals: agent.AllowAll, NoProjectSettings: cfg.NoProjectSettings || b.NoProjectSettings}
	launch, err := r.adapters[b.AgentConfig].Launch(spec)
	if err != nil || len(launch.Argv) == 0 || spec.NoProjectSettings && !launch.NoProjectSettings {
		return failure("denied", "adapter could not build the allowed launch")
	}
	// ForWorker already excluded ambient pass_env from probe and launch.
	// Exclude SSH/control variables even from explicit adapter configuration;
	// the peer supplied no environment or file path.
	launch.Env = safeEnvironment(launch.Env)
	if err := r.visibleRoot(); err != nil {
		r.ready = false
		return failure("unavailable", "registered root pathname changed")
	}
	if _, ok, err := host.ReadSpec(r.hostDir(run.Target)); err != nil || ok {
		return failure("not_ready", "run already has host launch state")
	}
	run.Agent = b.AgentConfig
	run.Spec, run.Terminal = spec, launch.Terminal
	run.Turn = "startup"
	run.Workspace = b.Workspace
	run.State = "starting"
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "start intent could not be committed")
	}
	if err := r.writeLaunchFiles(run.Target, launch.Files); err != nil {
		run.State = "unknown"
		r.ready = false
		return r.uncertain(rec)
	}
	// Adapter construction/file writes may take time. Recheck the visible
	// pathname immediately before handing paths to the detached host.
	if err := r.visibleRoot(); err != nil {
		run.State = "unknown"
		r.ready = false
		return r.uncertain(rec)
	}
	hostSpec := host.Spec{Identity: run.Identity, Argv: launch.Argv, Env: launch.Env, Dir: workspace, Terminal: launch.Terminal}
	if _, err := host.Start(r.options.HostCommand, r.hostDir(run.Target), hostSpec); err != nil {
		run.State = "unknown"
		r.ready = false
		return r.uncertain(rec)
	}
	if err := r.point("start_effect"); err != nil {
		r.ready = false
		return failure("unavailable", "start reply was lost")
	}
	c, err := r.owned(run)
	if err != nil {
		if _, ok, _ := r.ownedStatus(run); !ok {
			run.State = "unknown"
			r.ready = false
			return r.uncertain(rec)
		}
		run.State = "ended"
	} else {
		c.Close()
		run.State = "running"
	}
	return r.complete(rec, workerproto.Effect{Host: run.Identity})
}

func (r *Resident) end(m workerproto.Message) *workerproto.Refusal {
	run := r.state.Runs[targetKey(*m.Target)]
	if run == nil {
		return failure("wrong_target", "run is not registered")
	}
	if run.State == "ended" || run.State == "gone" || r.ended(run) {
		run.State = "ended"
		rec, err := r.intent(m)
		if err != nil {
			return failure("unavailable", "end intent could not be committed")
		}
		return r.complete(rec, workerproto.Effect{})
	}
	c, err := r.owned(run)
	if err != nil {
		return failure("unavailable", "owned host cannot be checked")
	}
	defer c.Close()
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "end intent could not be committed")
	}
	c.Deadline(time.Now().Add(45 * time.Second))
	if _, err := c.End(100*time.Millisecond, 100*time.Millisecond); err != nil {
		return r.uncertain(rec)
	}
	deadline := time.Now().Add(40 * time.Second)
	for !r.ended(run) {
		if time.Now().After(deadline) {
			return r.uncertain(rec)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.point("end_host_effect"); err != nil {
		return failure("unavailable", "end reply was lost")
	}
	run.State = "ended"
	return r.complete(rec, workerproto.Effect{})
}

func (r *Resident) ended(run *runRecord) bool {
	if _, ok, err := r.ownedStatus(run); err != nil || !ok {
		return false
	}
	// Use the computed private address, never the stored socket pointer.
	socket := strings.TrimPrefix(host.HookEnv(r.hostDir(run.Target)), host.HookEnvVar+"=")
	_, err := os.Lstat(socket)
	return errors.Is(err, os.ErrNotExist)
}

func (r *Resident) write(m workerproto.Message) *workerproto.Refusal {
	// Only pre-write refusals prove nothing was handed to the host.
	deny := func(code, message string) *workerproto.Refusal {
		rec, err := r.intent(m)
		if err != nil {
			return failure("unavailable", "write refusal could not be recorded")
		}
		return r.refuseFile(rec, code, message)
	}
	run := r.state.Runs[targetKey(*m.Target)]
	if run == nil || run.State != "running" {
		return deny("stale", "run is not running")
	}
	c, err := r.owned(run)
	if err != nil {
		return deny("unavailable", "owned host cannot be checked")
	}
	defer c.Close()
	var input workerproto.Input
	var answer workerproto.Answer
	if m.Op == "input" {
		json.Unmarshal(m.Body, &input)
	} else {
		json.Unmarshal(m.Body, &answer)
		if run.Turn != answer.Turn {
			return deny("stale", "answer names another turn")
		}
		calls, err := c.Waiting()
		if err != nil {
			return deny("unavailable", "waiting hooks cannot be checked")
		}
		found := false
		for _, call := range calls {
			found = found || call.ID == answer.Request
		}
		if !found {
			return deny("stale", "hook is no longer waiting")
		}
	}
	if m.Op == "input" {
		run.Turn = input.Turn
	}
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "write intent could not be committed")
	}
	if m.Op == "input" {
		err = c.Input(input.Data)
	} else {
		err = c.Answer(answer.Request, answer.Data)
	}
	if err != nil {
		return r.uncertain(rec)
	}
	if err := r.point(m.Op + "_effect"); err != nil {
		return failure("unavailable", "write reply was lost")
	}
	return r.complete(rec, workerproto.Effect{})
}
