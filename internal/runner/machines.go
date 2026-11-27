package runner

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/placement"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/workerproto"
)

// RemoteMachine is configured authority, never populated from worker output.
// Open verifies the pinned binding independently on each new connection.
type RemoteMachine struct {
	Status            func() control.MachineAvailability
	Observe           func(context.Context, string) (placement.Observation, error)
	Admission         *workerproto.Admission
	Binding           workerproto.Binding
	Destination, Root string
	Open              func(context.Context) (net.Conn, *workerproto.Connection, error)
}

func runID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// PlacementEnded never probes a remote path or interprets worker metadata as a
// local holder. Remote release comes only from bound complete inventory.
func (r *Runner) PlacementEnded(ctx context.Context, id int64) (bool, error) {
	remote, err := r.isRemote(ctx, id)
	if err != nil || remote {
		return false, err
	}
	ids, err := r.priorPlacementSessions(ctx, id, true)
	if err != nil {
		return false, err
	}
	for _, sid := range ids {
		if !r.alive(StateDir(r.Root, sid)).Ended() {
			return false, nil
		}
	}
	return true, nil
}

func (r *Runner) isRemote(ctx context.Context, id int64) (bool, error) {
	if r.Store == nil {
		return false, nil
	}
	var name string
	err := r.Store.DB.QueryRowContext(ctx, `SELECT t.machine FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE s.id=?`, id).Scan(&name)
	return name != store.LocalMachine, err
}

func (r *Runner) remoteSession(ctx context.Context, id int64) (*machineio.Remote, error) {
	var machine, run, pinned string
	var thread uint64
	var configured bool
	err := r.Store.DB.QueryRowContext(ctx, `SELECT t.machine,t.id,m.installation,m.configured,x.run FROM sessions s
		JOIN threads t ON t.id=s.thread_id JOIN machines m ON m.key=t.machine JOIN session_machine_runs x ON x.session_id=s.id AND x.machine=t.machine WHERE s.id=?`, id).Scan(&machine, &thread, &pinned, &configured, &run)
	if err != nil {
		return nil, err
	}
	m, ok := r.Remotes[machine]
	if !ok || !configured || pinned == "" || pinned != m.Binding.Worker || m.Open == nil {
		return nil, errors.New("runner: owning machine is unavailable or its binding differs")
	}
	return &machineio.Remote{Binding: m.Binding, Target: workerproto.Target{Thread: thread, Session: "s-" + strconv.FormatInt(id, 10), Run: run}, Open: m.Open}, nil
}

// remoteDir accepts only the main's own logical session-directory spelling.
// It never resolves worker metadata by a path, which can be equal on machines.
func (r *Runner) remoteDir(dir string) (*machineio.Remote, bool, error) {
	if r.Store == nil {
		return nil, false, nil
	}
	id, err := strconv.ParseInt(filepath.Base(dir), 10, 64)
	if err != nil || dir != StateDir(r.Root, id) {
		return nil, false, nil
	}
	remote, err := r.isRemote(context.Background(), id)
	if err != nil {
		return nil, true, err
	}
	if !remote {
		return nil, false, nil
	}
	s, err := r.remoteSession(context.Background(), id)
	return s, true, err
}

func (r *Runner) reserveRun(ctx context.Context, id int64) error {
	var machine string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT t.machine FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE s.id=?`, id).Scan(&machine); err != nil {
		return err
	}
	if machine == store.LocalMachine {
		return errors.New("runner: cannot reserve a worker run for a local session")
	}
	return r.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_machine_runs(session_id,machine,run,workspace,agent_config,phase) VALUES(?,?,?,'','default','new') ON CONFLICT(session_id) DO NOTHING`, id, machine, runID()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO machine_run_history(machine,thread_id,session_id,run) SELECT x.machine,s.thread_id,s.id,x.run FROM session_machine_runs x JOIN sessions s ON s.id=x.session_id WHERE s.id=?`, id)
		return err
	})
}

// MakeRemoteWorkspace is used after placement, before launch. The recorded
// target and clone key survive main failure, and no worker path is opened.
func (r *Runner) MakeRemoteWorkspace(ctx context.Context, id int64, repo string) (string, error) {
	if err := r.reserveRun(ctx, id); err != nil {
		return "", err
	}
	remote, err := r.remoteSession(ctx, id)
	if err != nil {
		return "", err
	}
	if err := r.prepareRemote(ctx, remote); err != nil {
		return "", err
	}
	var machine string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT machine FROM session_machine_runs WHERE session_id=?`, id).Scan(&machine); err != nil {
		return "", err
	}
	if r.Remotes[machine].Admission != nil {
		q, err := remote.Request("resolve_repository", "", workerproto.RepositoryReference{Repository: repo})
		if err != nil {
			return "", err
		}
		reply, err := remote.Call(ctx, q)
		if err != nil {
			return "", err
		}
		var ref workerproto.RepositoryReference
		if err := json.Unmarshal(reply.Body, &ref); err != nil {
			return "", err
		}
		repo = ref.Repository
	}
	m, err := remote.Request("clone", "clone-"+remote.Target.Run, workerproto.Clone{Repository: repo})
	if err != nil {
		return "", err
	}
	effect, err := r.storageEffect(ctx, id, remote, "clone", m.Key, workerproto.Clone{Repository: repo})
	if err != nil {
		return "", err
	}
	_, err = r.Store.DB.ExecContext(ctx, `UPDATE session_machine_runs SET workspace=?,phase='workspace' WHERE session_id=? AND run=?`, effect.Workspace, id, remote.Target.Run)
	return effect.Workspace, err
}

func (r *Runner) prepareRemote(ctx context.Context, remote *machineio.Remote) error {
	m, err := remote.Request("prepare", "prepare-"+remote.Target.Run, workerproto.Prepare{Instructions: r.Instructions, Policy: []string{}})
	if err != nil {
		return err
	}
	_, err = remote.Effect(ctx, m)
	return err
}

func (r *Runner) launchRemote(ctx context.Context, id int64, name string, spec agent.Spec, first firstInput) (agent.Spec, error) {
	ad, ok := r.Adapters[name]
	if !ok {
		return agent.Spec{}, errors.New("runner: worker adapter unavailable")
	}
	if _, ok := ad.NewCodec(spec).(agent.InputRefuser); !ok {
		return agent.Spec{}, errors.New("runner: worker codec cannot reject unsupported input actions")
	}
	thread, _, confined, err := r.confinement(ctx, id)
	if err != nil {
		return agent.Spec{}, err
	}
	if confined || spec.Confine != nil {
		return agent.Spec{}, errors.New("runner: confined sessions must stay local")
	}
	var machine string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT no_project_settings,machine FROM threads WHERE id=?`, int64(thread)).Scan(&spec.NoProjectSettings, &machine); err != nil {
		return agent.Spec{}, err
	}
	if err := r.reserveRun(ctx, id); err != nil {
		return agent.Spec{}, err
	}
	remote, err := r.remoteSession(ctx, id)
	if err != nil {
		return agent.Spec{}, err
	}
	var workspace, config, phase string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT workspace,agent_config,phase FROM session_machine_runs WHERE session_id=?`, id).Scan(&workspace, &config, &phase); err != nil {
		return agent.Spec{}, err
	}
	if workspace == "" {
		if err := r.inheritWorkspace(ctx, id, spec.Dir, remote); err != nil {
			return agent.Spec{}, err
		}
		workspace = spec.Dir
	}
	if err := r.prepareRemote(ctx, remote); err != nil {
		return agent.Spec{}, err
	}
	var previous string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT previous_run FROM session_machine_runs WHERE session_id=?`, id).Scan(&previous); err != nil {
		return agent.Spec{}, err
	}
	if previous != "" && phase != "running" {
		var prior workerproto.Target
		if err := json.Unmarshal([]byte(previous), &prior); err != nil {
			return agent.Spec{}, err
		}
		m, err := remote.Request("bind_workspace", "bind-"+remote.Target.Run, workerproto.BindWorkspace{Previous: prior, Workspace: workspace})
		if err != nil {
			return agent.Spec{}, err
		}
		if _, err := remote.Effect(ctx, m); err != nil {
			return agent.Spec{}, err
		}
	}
	if phase != "starting" && phase != "running" {
		if err := r.newAgent(ctx, id); err != nil {
			return agent.Spec{}, err
		}
		if _, err := r.Store.DB.ExecContext(ctx, `UPDATE session_machine_runs SET phase='starting' WHERE session_id=? AND run=?`, id, remote.Target.Run); err != nil {
			return agent.Spec{}, err
		}
	}
	m, err := remote.Request("start", "start-"+remote.Target.Run, workerproto.Start{Admission: r.Remotes[machine].Admission, AgentConfig: workerproto.AgentConfig{Agent: name, Config: config}, Holder: "own", Workspace: workspace, Model: spec.Model, Effort: spec.Effort, Resume: spec.ResumeID, NoProjectSettings: spec.NoProjectSettings})
	if err != nil {
		return agent.Spec{}, err
	}
	if err := r.startEffect(ctx, id, remote, m); err != nil {
		return agent.Spec{}, err
	}
	metadata, err := remote.Metadata(ctx)
	if err != nil {
		return agent.Spec{}, err
	}
	spec = metadata.Spec
	first.Dir = workspace
	fb, _ := json.Marshal(first)
	sb, _ := json.Marshal(storedSpec{Agent: name, Spec: spec})
	err = r.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET spec=?,first_input=?,launch=0,host_pid=0,io_error=CASE WHEN io_error=? THEN '' ELSE io_error END WHERE id=?`, string(sb), string(fb), core.ErrPlacementWait.Error(), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE session_machine_runs SET phase='running' WHERE session_id=? AND run=?`, id, remote.Target.Run)
		return err
	})
	return spec, err
}

func (r *Runner) resumeRemote(ctx context.Context, id int64, first firstInput) error {
	remote, err := r.remoteSession(ctx, id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	old := r.live[id]
	r.mu.Unlock()
	if old != nil {
		select {
		case <-old.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
			return errors.New("runner: worker follower has not ended")
		}
	}
	var phase, previous string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT phase,previous_run FROM session_machine_runs WHERE session_id=?`, id).Scan(&phase, &previous); err != nil {
		return err
	}
	resuming := phase != "running" && previous != ""
	l, err := remote.Alive(ctx)
	if err != nil {
		if !resuming {
			return err
		}
	}
	if !resuming && !l.Ended() {
		return errors.New("runner: previous worker host is not confirmed ended")
	}
	var stored, resume, workspace string
	var aside sql.NullString
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT s.spec,s.agent_session_id,x.workspace,s.set_aside FROM sessions s JOIN session_machine_runs x ON x.session_id=s.id WHERE s.id=?`, id).Scan(&stored, &resume, &workspace, &aside); err != nil {
		return err
	}
	var spec storedSpec
	if err := json.Unmarshal([]byte(stored), &spec); err != nil {
		return err
	}
	if aside.Valid {
		resume = ""
	}
	if resume == "" && first.NoInput {
		return errors.New("runner: worker session has no conversation to resume")
	}
	if resuming {
		spec.Spec.ResumeID = resume
		spec.Spec, err = r.launchRemote(ctx, id, spec.Agent, spec.Spec, first)
		if err != nil {
			return err
		}
		ad, ok := r.Adapters[spec.Agent]
		if !ok {
			return errors.New("runner: worker adapter is unavailable")
		}
		return r.follow(id, ad, spec.Spec, fresh)
	}
	// The old run must be drained before its offsets are retired. It remains
	// retained on the worker; this does not release or delete history.
	prior := remote.Target
	priorJSON, _ := json.Marshal(prior)
	next := runID()
	if err := r.Store.Update(ctx, func(tx *store.Tx) error {
		for _, table := range []string{"session_read_proofs", "session_reads", "session_io_sources", "session_files"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id=?`, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET log_start=0,log_offset=0,screen_offset=0,io_error='' WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE session_machine_runs SET run=?,phase='new',previous_run=? WHERE session_id=? AND run=?`, next, string(priorJSON), id, prior.Run); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO machine_run_history(machine,thread_id,session_id,run) SELECT machine,?,session_id,run FROM session_machine_runs WHERE session_id=?`, prior.Thread, id)
		return err
	}); err != nil {
		return err
	}
	remote, err = r.remoteSession(ctx, id)
	if err != nil {
		return err
	}
	if err := r.prepareRemote(ctx, remote); err != nil {
		return err
	}
	m, err := remote.Request("bind_workspace", "bind-"+next, workerproto.BindWorkspace{Previous: prior, Workspace: workspace})
	if err != nil {
		return err
	}
	if _, err := remote.Effect(ctx, m); err != nil {
		return err
	}
	spec.Spec.ResumeID = resume
	spec.Spec, err = r.launchRemote(ctx, id, spec.Agent, spec.Spec, first)
	if err != nil {
		return err
	}
	ad, ok := r.Adapters[spec.Agent]
	if !ok {
		return fmt.Errorf("runner: adapter unavailable")
	}
	return r.follow(id, ad, spec.Spec, fresh)
}

func (r *Runner) inheritWorkspace(ctx context.Context, id int64, workspace string, remote *machineio.Remote) error {
	if !workerproto.ValidID(workspace) {
		return errors.New("runner: worker workspace has not been prepared")
	}
	var prior workerproto.Target
	var priorID int64
	err := r.Store.DB.QueryRowContext(ctx, `SELECT s.id,s.thread_id,x.run FROM sessions s JOIN session_machine_runs x ON x.session_id=s.id
		WHERE s.thread_id=? AND s.id!=? AND x.workspace=? ORDER BY s.id DESC LIMIT 1`, remote.Target.Thread, id, workspace).Scan(&priorID, &prior.Thread, &prior.Run)
	if err != nil {
		return err
	}
	prior.Session = "s-" + strconv.FormatInt(priorID, 10)
	old := &machineio.Remote{Binding: remote.Binding, Target: prior, Open: remote.Open}
	if _, err := old.End(ctx, "end-"+prior.Run, time.Second, 2*time.Second); err != nil {
		return err
	}
	previous, _ := json.Marshal(prior)
	_, err = r.Store.DB.ExecContext(ctx, `UPDATE session_machine_runs SET workspace=?,previous_run=? WHERE session_id=? AND run=?`, workspace, string(previous), id, remote.Target.Run)
	return err
}

// actRemote persists the exact effect before sending it. The main's written
// input marker is already durable. Replay rebuilds codecs without this method.
func (r *Runner) actRemote(lv *live, a agent.Actions, failed func(bool)) error {
	remote, ok := lv.io.(*machineio.Remote)
	if !ok {
		return errors.New("runner: remote effect transport is unavailable")
	}
	ctx := context.Background()
	info, err := remote.Info(ctx)
	if err != nil {
		failed(true)
		return err
	}
	turn := info.Turn
	if len(a.Inputs) > 0 {
		turn = a.Inputs[0]
	}
	if turn == "" {
		turn = "startup"
	}
	chunks := append([][]byte{}, a.Write...)
	for _, part := range a.Keys {
		b, err := holder.Encode(part)
		if err != nil {
			failed(true)
			return &holder.NotSent{Err: err}
		}
		chunks = append(chunks, b)
	}
	if len(chunks) > 0 {
		key := "input-" + runID()
		var data []byte
		for _, b := range chunks {
			data = append(data, b...)
		}
		if len(data) > workerproto.MaxChunk {
			if len(a.Inputs) > 0 {
				refuser, ok := lv.codec.(agent.InputRefuser)
				if !ok {
					return errors.New("runner: worker codec cannot reject an unsupported input action")
				}
				if err := r.do(lv, refuser.RefuseInput(a.Inputs, agent.NotDelivered)); err != nil {
					return err
				}
			}
			return fmt.Errorf("runner: worker input exceeds %d encoded bytes; nothing was sent", workerproto.MaxChunk)
		}
		m, err := remote.Request("input", key, workerproto.Input{Turn: turn, Data: data})
		if err != nil {
			failed(true)
			return err
		}
		if err := r.machineEffect(ctx, lv.id, remote, m, a.Inputs...); err != nil {
			var nothing *holder.NotSent
			failed(errors.As(err, &nothing))
			return err
		}
	}
	for _, rep := range a.Reply {
		base := "hook-" + digestRead(append(append([]byte(rep.Call), 0), rep.Body...))
		key := base
		var priorKey, state, request string
		err := r.Store.DB.QueryRowContext(ctx, `SELECT key,state,request FROM session_machine_actions WHERE session_id=? AND run=? AND operation='answer' AND key LIKE ? ORDER BY id DESC LIMIT 1`, lv.id, remote.Target.Run, base+"%").Scan(&priorKey, &state, &request)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			key = priorKey
			if state == "refused" {
				key = base + "-" + runID()
			}
		}
		m, err := remote.Request("answer", key, workerproto.Answer{Turn: turn, Request: rep.Call, Data: rep.Body})
		if err == nil && request != "" && state != "refused" {
			err = json.Unmarshal([]byte(request), &m)
		}
		if err != nil {
			return err
		}
		if err := r.machineEffect(ctx, lv.id, remote, m); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) machineEffect(ctx context.Context, id int64, remote *machineio.Remote, m workerproto.Message, inputs ...string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	inputJSON, _ := json.Marshal(inputs)
	_, err = r.Store.DB.ExecContext(ctx, `INSERT INTO session_machine_actions(session_id,run,operation,key,request,state,inputs) VALUES(?,?,?,?,?,'pending',?) ON CONFLICT(session_id,run,operation,key) DO NOTHING`, id, remote.Target.Run, m.Op, m.Key, string(b), string(inputJSON))
	if err != nil {
		return &holder.NotSent{Err: err}
	}
	_, err = remote.Effect(ctx, m)
	state := "complete"
	if err != nil {
		state = "unknown"
		var no *holder.NotSent
		if errors.As(err, &no) {
			state = "refused"
		}
	}
	if _, e := r.Store.DB.ExecContext(ctx, `UPDATE session_machine_actions SET state=? WHERE session_id=? AND run=? AND operation=? AND key=?`, state, id, remote.Target.Run, m.Op, m.Key); e != nil {
		return errors.Join(err, e)
	}
	return err
}

func (r *Runner) reconcileMachineActions(lv *live) error {
	remote, ok := lv.io.(*machineio.Remote)
	if !ok {
		return nil
	}
	ctx := context.Background()
	rows, err := r.Store.DB.Query(`SELECT id,request,inputs FROM session_machine_actions WHERE session_id=? AND run=? AND state IN ('pending','unknown') ORDER BY id`, lv.id, remote.Target.Run)
	if err != nil {
		return err
	}
	type pending struct {
		id              int64
		request, inputs string
	}
	var actions []pending
	for rows.Next() {
		var a pending
		if err := rows.Scan(&a.id, &a.request, &a.inputs); err != nil {
			rows.Close()
			return err
		}
		actions = append(actions, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range actions {
		var original workerproto.Message
		if err := json.Unmarshal([]byte(a.request), &original); err != nil {
			return err
		}
		if original.Target == nil || *original.Target != remote.Target || original.Binding != remote.Binding {
			return errors.New("runner: recorded worker action binding differs")
		}
		query, err := remote.Request("receipt", "", workerproto.ReceiptQuery{Operation: original.Op, Key: original.Key})
		if err != nil {
			return err
		}
		reply, err := remote.Call(ctx, query)
		if err != nil {
			var refusal *workerproto.Refusal
			if errors.As(err, &refusal) && refusal.Code == "not_found" {
				continue
			}
			return err
		}
		var receipt workerproto.Receipt
		if err := json.Unmarshal(reply.Body, &receipt); err != nil {
			return err
		}
		if err := workerproto.CheckReceipt(original, receipt); err != nil {
			return err
		}
		if receipt.State == "refused" {
			var inputs []string
			json.Unmarshal([]byte(a.inputs), &inputs)
			r.unmarkWritten(lv, inputs)
		}
		if _, err := r.Store.DB.Exec(`UPDATE session_machine_actions SET state=? WHERE id=?`, receipt.State, a.id); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) watchRemoteScreen(lv *live, b *batch) {
	// Terminal bytes use a separate retained source; they never become host
	// hook/output lines. Source read failures pause this start visibly.
	data, err := r.remoteScreen(lv)
	if err != nil {
		b.err = err
		return
	}
	if len(data) > 0 {
		s := lv.sources["@screen"]
		b.screenOffset = lv.seenTo + uint64(len(data))
		b.reads = append(b.reads, readRange{path: "@screen", start: int64(lv.seenTo), end: int64(b.screenOffset), source: s, digest: digestRead(data)})
		r.decode(lv, b, agent.Screen, data)
	}
}

func (r *Runner) remoteScreen(lv *live) ([]byte, error) {
	s, err := r.sourceFor(lv, "@screen")
	if err != nil {
		return nil, err
	}
	b, err := lv.io.Read(context.Background(), s, int64(lv.seenTo), machineio.MaxRead)
	if err != nil {
		return nil, err
	}
	return b.Data, nil
}

func (r *Runner) WorkerAttachment(ctx context.Context, id int64) (control.WorkerAttachment, error) {
	remote, err := r.remoteSession(ctx, id)
	if err != nil {
		return control.WorkerAttachment{}, err
	}
	m, err := remote.Info(ctx)
	if err != nil {
		return control.WorkerAttachment{}, err
	}
	if !m.Terminal || m.State != "running" {
		return control.WorkerAttachment{}, errors.New("runner: worker terminal is not running")
	}
	var machine string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT machine FROM session_machine_runs WHERE session_id=?`, id).Scan(&machine); err != nil {
		return control.WorkerAttachment{}, err
	}
	cfg := r.Remotes[machine]
	return control.WorkerAttachment{Number: int64(remote.Target.Thread), Destination: cfg.Destination, Root: cfg.Root, Main: remote.Binding.Main, Installation: remote.Binding.Worker, Thread: remote.Target.Thread, Session: remote.Target.Session, Run: remote.Target.Run}, nil
}

func (r *Runner) remoteTerminal(ctx context.Context, id int64, run string, after uint64) (<-chan control.TerminalChunk, error) {
	remote, err := r.remoteSession(ctx, id)
	if err != nil {
		return nil, err
	}
	metadata, err := remote.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	if !metadata.Terminal {
		return nil, errors.New("runner: worker agent has no terminal")
	}
	out := make(chan control.TerminalChunk)
	go func() {
		defer close(out)
		var src machineio.Source
		var sourceRun string
		for ctx.Err() == nil {
			current, err := r.remoteSession(ctx, id)
			if err != nil {
				return
			}
			if sourceRun != current.Target.Run {
				src, err = current.Source(ctx, "@screen")
				if err != nil {
					return
				}
				sourceRun = current.Target.Run
			}
			reset := run != sourceRun
			if reset {
				after = 0
				run = sourceRun
			}
			b, err := current.Read(ctx, src, int64(after), machineio.MaxRead)
			if err != nil {
				return
			}
			after += uint64(len(b.Data))
			if reset || len(b.Data) > 0 {
				select {
				case out <- control.TerminalChunk{Run: run, Pos: after, Reset: reset, Data: b.Data, Cols: int(r.Cols), Rows: int(r.Rows)}:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.poll()):
			}
		}
	}()
	return out, nil
}

// Cleanup retries only the durable current targets of terminal sessions. A
// disconnected worker never turns Unknown into Gone or grants a local path.
func (r *Runner) cleanupRemoteSleeper(ctx context.Context, id int64) {
	var run string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT x.run FROM session_machine_runs x JOIN sessions s ON s.id=x.session_id WHERE s.id=? AND s.state IN ('asleep','closed','failed')`, id).Scan(&run); err != nil {
		return
	}
	remote, err := r.remoteSession(ctx, id)
	if err != nil || remote.Target.Run != run {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := remote.Alive(ctx); err != nil {
		return
	}
	if _, err := remote.End(ctx, "end-"+remote.Target.Run, time.Second, 2*time.Second); err != nil {
		r.Log.Warn("worker sleeper cleanup pending", "session", id, "err", err)
	}
}
func (r *Runner) startRemoteCleanup() {
	if len(r.Remotes) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cleanupCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cleanupCancel, r.cleanupDone = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rows, err := r.Store.DB.QueryContext(ctx, `SELECT s.id FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE t.machine!='local' AND s.state IN ('asleep','closed','failed') AND s.spec!=''`)
				if err != nil {
					continue
				}
				var ids []int64
				for rows.Next() {
					var id int64
					if rows.Scan(&id) == nil {
						ids = append(ids, id)
					}
				}
				rows.Close()
				for _, id := range ids {
					if ctx.Err() != nil {
						return
					}
					r.cleanupRemoteSleeper(ctx, id)
				}
			}
		}
	}()
}

// Superseding a conversation never grants room for a second local host. End
// only this thread's recorded earlier runs and require positive holder proof.
func (r *Runner) priorPlacementSessions(ctx context.Context, id int64, includeCurrent bool) ([]int64, error) {
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT id FROM sessions WHERE thread_id=(SELECT thread_id FROM sessions WHERE id=?) AND (id<? OR (? AND id=?)) AND spec!='' ORDER BY id`, id, id, includeCurrent, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		ids = append(ids, sid)
	}
	return ids, rows.Err()
}
func (r *Runner) endPriorPlacements(ctx context.Context, id int64) error {
	remote, err := r.isRemote(ctx, id)
	if err != nil || remote {
		return err
	}
	ids, err := r.priorPlacementSessions(ctx, id, false)
	if err != nil {
		return err
	}
	for _, sid := range ids {
		if err := r.endEarlier(ctx, sid, StateDir(r.Root, sid)); err != nil {
			return fmt.Errorf("%w: earlier local host end is unconfirmed", core.ErrPlacementWait)
		}
	}
	return nil
}

// PlacementRecovered recognizes only the original phase-two host. Its own
// resource use must not block reconnecting it. Matching the prior run's PID
// does not prove this new launch happened, even if the old run has ended.
func (r *Runner) PlacementRecovered(ctx context.Context, id int64) (bool, error) {
	remote, err := r.isRemote(ctx, id)
	if err != nil || remote {
		return false, err
	}
	var phase, recorded int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT launch,host_pid FROM sessions WHERE id=?`, id).Scan(&phase, &recorded); err != nil {
		return false, err
	}
	if phase != 2 {
		return false, nil
	}
	dir := StateDir(r.Root, id)
	b, err := os.ReadFile(filepath.Join(dir, host.HostPIDFile))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || pid == recorded {
		return false, nil
	}
	l := r.alive(dir)
	return l.State == holder.Running || l.Ended(), nil
}
