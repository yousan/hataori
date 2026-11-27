package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/workerproto"
)

func (r *Resident) workspaceStatus(target workerproto.Target, handle string) (workerproto.WorkspaceStatus, *workerproto.Refusal) {
	w, ok := r.state.Workspaces[workspaceKey(target, handle)]
	if !ok || w.Target != target || w.Handle != handle {
		return workerproto.WorkspaceStatus{}, failure("wrong_target", "workspace is not registered for this run")
	}
	status := workerproto.WorkspaceStatus{Workspace: handle, State: w.State}
	switch w.State {
	case "", "present":
		if _, err := r.ownedWorkspace(target, handle); err != nil {
			return status, failure("unavailable", "workspace identity cannot be established")
		}
		status.State = "present"
	case "trashed":
		for _, tr := range r.state.Trashes {
			if tr.Target == target && tr.Workspace == handle && tr.State == "retained" {
				if status.Trash != "" {
					return status, failure("unavailable", "several trash records match")
				}
				info, err := r.root.Lstat(filepath.Join("trash", "worker-"+tr.Handle))
				if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return status, failure("unavailable", "trash identity cannot be established")
				}
				d, i, ok := directoryIdentity(info)
				if !ok || d != tr.Device || i != tr.Inode {
					return status, failure("unavailable", "trash identity changed")
				}
				status.Trash = tr.Handle
			}
		}
		if status.Trash == "" {
			return status, failure("unavailable", "retained trash is not registered")
		}
	case "purged":
		if _, err := r.root.Lstat(filepath.Join("workspaces", handle)); !errors.Is(err, os.ErrNotExist) {
			return status, failure("unavailable", "purged workspace place is not proven empty")
		}
	default:
		return status, failure("unavailable", "workspace transition is not reconciled")
	}
	return status, nil
}

// remakeWorkspace makes a confirmed purged workspace at its original handle,
// never at another machine or directory. Registered clone refuses every entry
// already at the place and journals the new identity before publication.
func (r *Resident) remakeWorkspace(ctx context.Context, m workerproto.Message) *workerproto.Refusal {
	var b workerproto.Workspace
	json.Unmarshal(m.Body, &b)
	target := *m.Target
	w, ok := r.state.Workspaces[workspaceKey(target, b.Workspace)]
	run := r.state.Runs[targetKey(target)]
	if !ok || w.Target != target || w.State != "purged" || run == nil || (run.State != "ended" && run.State != "gone" && run.State != "prepared") {
		return failure("not_ready", "only a confirmed purged workspace of an ended run can be remade")
	}
	if _, refusal := r.workspaceStatus(target, b.Workspace); refusal != nil {
		return refusal
	}
	repository, ok := r.workspaceConfig.Repositories[w.Repository]
	if !ok {
		return failure("denied", "workspace repository is no longer configured")
	}
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "remake intent could not be committed")
	}
	rec.Plan = &workspacePlan{Workspace: w.Handle}
	if err := r.save(); err != nil {
		return r.fileFailure(rec, err, "remake plan could not be committed")
	}
	_, err = r.maker().CloneRegistered(ctx, w.Handle, repository, func(staging string, p core.Placement) error {
		w.Device, w.Inode, w.State, w.Staging = p.Dev, p.Inode, "cloning", staging
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
		rec.Plan = &workspacePlan{Workspace: w.Handle, Staging: staging, Device: p.Dev, Inode: p.Inode}
		if err := r.save(); err != nil {
			return err
		}
		return r.point("remake_before_rename")
	})
	if err != nil {
		return r.fileFailure(rec, err, "remake failed or its publication is uncertain")
	}
	if err := r.point("remake_after_rename"); err != nil {
		return r.fileFailure(rec, err, "remake result is uncertain")
	}
	if !placementExists(r.placement(w).Dir, w.Device, w.Inode) {
		return r.fileFailure(rec, errors.New("identity"), "remake identity changed")
	}
	w.State, w.Staging = "present", ""
	r.state.Workspaces[workspaceKey(target, w.Handle)] = w
	return r.complete(rec, workerproto.Effect{Workspace: w.Handle})
}
