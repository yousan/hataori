package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/workerproto"
)

func (r *Runner) storageEffect(ctx context.Context, id int64, remote *machineio.Remote, op, base string, body any) (workerproto.Effect, error) {
	m, err := remote.Request(op, base, body)
	if err != nil {
		return workerproto.Effect{}, err
	}
	var previous, state string
	err = r.Store.DB.QueryRowContext(ctx, `SELECT request,state FROM session_machine_actions WHERE session_id=? AND run=? AND operation=? AND key LIKE ? ORDER BY id DESC LIMIT 1`, id, remote.Target.Run, op, base+"%").Scan(&previous, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return workerproto.Effect{}, err
	}
	if err == nil {
		if state == "refused" {
			m.Key = base + "-" + runID()
		} else if err := json.Unmarshal([]byte(previous), &m); err != nil {
			return workerproto.Effect{}, err
		}
	}
	if m.Binding != remote.Binding || m.Target == nil || *m.Target != remote.Target {
		return workerproto.Effect{}, errors.New("runner: storage request binding differs")
	}
	b, _ := json.Marshal(m)
	if _, err := r.Store.DB.ExecContext(ctx, `INSERT INTO session_machine_actions(session_id,run,operation,key,request,state) VALUES(?,?,?,?,?,'pending') ON CONFLICT(session_id,run,operation,key) DO NOTHING`, id, remote.Target.Run, op, m.Key, string(b)); err != nil {
		return workerproto.Effect{}, err
	}
	effect, err := remote.Effect(ctx, m)
	state = "complete"
	if err != nil {
		state = "unknown"
		var no *holder.NotSent
		if errors.As(err, &no) {
			state = "refused"
		}
	}
	_, saveErr := r.Store.DB.ExecContext(ctx, `UPDATE session_machine_actions SET state=? WHERE session_id=? AND run=? AND operation=? AND key=?`, state, id, remote.Target.Run, op, m.Key)
	return effect, errors.Join(err, saveErr)
}

func (r *Runner) workerWorkspace(ctx context.Context, id int64) (*machineio.Remote, string, error) {
	remote, err := r.remoteSession(ctx, id)
	if err != nil {
		return nil, "", err
	}
	var handle string
	err = r.Store.DB.QueryRowContext(ctx, `SELECT workspace FROM session_machine_runs WHERE session_id=?`, id).Scan(&handle)
	return remote, handle, err
}

func workerWorkspaceStatus(ctx context.Context, remote *machineio.Remote, handle string) (workerproto.WorkspaceStatus, error) {
	m, err := remote.Request("workspace_status", "", workerproto.Workspace{Workspace: handle})
	if err != nil {
		return workerproto.WorkspaceStatus{}, err
	}
	reply, err := remote.Call(ctx, m)
	if err != nil {
		return workerproto.WorkspaceStatus{}, err
	}
	var status workerproto.WorkspaceStatus
	if err := json.Unmarshal(reply.Body, &status); err != nil {
		return status, err
	}
	if status.Workspace != handle {
		return status, errors.New("runner: workspace status handle differs")
	}
	return status, nil
}

func (r *Runner) RemoveRemoteWorkspace(ctx context.Context, id int64, cycle int64) (string, error) {
	remote, handle, err := r.workerWorkspace(ctx, id)
	if err != nil {
		return "", err
	}
	// This negotiates the 1.3 storage contract before a destructive request.
	if _, err := workerWorkspaceStatus(ctx, remote, handle); err != nil {
		return "", err
	}
	effect, err := r.storageEffect(ctx, id, remote, "remove", "remove-"+remote.Target.Run+"-"+strconv.FormatInt(cycle, 10), workerproto.Workspace{Workspace: handle})
	if err == nil && (effect.Workspace != handle || effect.Trash == "") {
		return "", errors.New("runner: removal receipt lacks the original workspace/trash")
	}
	return effect.Trash, err
}

func (r *Runner) AcknowledgeRemoteTrash(ctx context.Context, id int64, trash string) error {
	remote, _, err := r.workerWorkspace(ctx, id)
	if err != nil {
		return err
	}
	_, err = r.storageEffect(ctx, id, remote, "acknowledge", "ack-"+trash, workerproto.Trash{Trash: trash})
	return err
}

func (r *Runner) ReopenRemoteWorkspace(ctx context.Context, id int64, cycle int64) (string, string, error) {
	remote, handle, err := r.workerWorkspace(ctx, id)
	if err != nil {
		return "", "", err
	}
	status, err := workerWorkspaceStatus(ctx, remote, handle)
	if err != nil {
		return "", "", err
	}
	key := remote.Target.Run + "-" + strconv.FormatInt(cycle, 10)
	how := control.ReopenKept
	switch status.State {
	case "present":
		// A completed restore/remake whose main commit was lost retains
		// its original request and receipt, including its reported outcome.
		var op string
		err := r.Store.DB.QueryRowContext(ctx, `SELECT operation FROM session_machine_actions WHERE session_id=? AND run=? AND key LIKE ? AND operation IN ('restore','remake_workspace') ORDER BY id DESC LIMIT 1`, id, remote.Target.Run, "reopen-"+key+"%").Scan(&op)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		if op == "restore" {
			how = control.ReopenRestored
		} else if op == "remake_workspace" {
			how = control.ReopenRemade
		}
		return handle, how, nil
	case "trashed":
		effect, err := r.storageEffect(ctx, id, remote, "restore", "reopen-"+key, workerproto.Trash{Trash: status.Trash})
		if err != nil {
			return "", "", err
		}
		if effect.Workspace != handle {
			return "", "", errors.New("runner: restored workspace differs")
		}
		how = control.ReopenRestored
	case "purged":
		effect, err := r.storageEffect(ctx, id, remote, "remake_workspace", "reopen-"+key, workerproto.Workspace{Workspace: handle})
		if err != nil {
			return "", "", err
		}
		if effect.Workspace != handle {
			return "", "", errors.New("runner: remade workspace differs")
		}
		how = control.ReopenRemade
	default:
		return "", "", errors.New("runner: workspace state is unknown")
	}
	return handle, how, nil
}
