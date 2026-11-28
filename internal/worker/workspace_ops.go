package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/workerproto"
	"github.com/yousan/hataori/internal/workspace"
)

type workspacePlan struct {
	Workspace string `json:"workspace,omitempty"`
	Trash     string `json:"trash,omitempty"`
	Staging   string `json:"staging,omitempty"`
	Device    uint64 `json:"device,omitempty"`
	Inode     uint64 `json:"inode,omitempty"`
}
type trashRecord struct {
	Target       workerproto.Target `json:"target"`
	Handle       string             `json:"handle"`
	Workspace    string             `json:"workspace"`
	Device       uint64             `json:"device"`
	Inode        uint64             `json:"inode"`
	RemovedMS    int64              `json:"removed_ms"`
	Acknowledged bool               `json:"acknowledged"`
	State        string             `json:"state"`
}

func validHandle(handle string) bool                          { return workerproto.ValidID(handle) }
func workspaceKey(t workerproto.Target, handle string) string { return targetKey(t) + "-" + handle }
func (r *Resident) placement(w workspaceRecord) core.Placement {
	return core.Placement{Dir: filepath.Join(r.maker().Root, workspace.Dir, w.Handle), Dev: w.Device, Inode: w.Inode}
}
func (r *Resident) trashPath(handle string) string {
	return filepath.Join(r.maker().Root, workspace.TrashDir, "worker-"+handle)
}
func placementExists(path string, dev, ino uint64) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	d, i, ok := directoryIdentity(info)
	return ok && ino != 0 && d == dev && i == ino
}
func directoryIdentity(info os.FileInfo) (uint64, uint64, bool) { return fileIdentity(info) }
func (r *Resident) workspaceOperation(ctx context.Context, m workerproto.Message) *workerproto.Refusal {
	target := *m.Target
	switch m.Op {
	case "remake_workspace":
		return r.remakeWorkspace(ctx, m)
	case "clone":
		var b workerproto.Clone
		json.Unmarshal(m.Body, &b)
		repository, ok := r.workspaceConfig.Repositories[b.Repository]
		if !ok {
			return failure("denied", "repository is not configured on this worker")
		}
		for _, w := range r.state.Workspaces {
			if w.Target == target && w.State != "purged" {
				return failure("key_conflict", "run already owns a workspace")
			}
		}
		rec, err := r.intent(m)
		if err != nil {
			return failure("unavailable", "clone intent could not be committed")
		}
		handle := newID()
		rec.Plan = &workspacePlan{Workspace: handle}
		if err := r.save(); err != nil {
			return r.fileFailure(rec, err, "clone handle could not be committed")
		}
		_, err = r.maker().CloneRegistered(ctx, handle, repository, func(staging string, p core.Placement) error {
			w := workspaceRecord{Target: target, Handle: handle, Device: p.Dev, Inode: p.Inode, Repository: b.Repository, State: "cloning", Staging: staging}
			r.state.Workspaces[workspaceKey(target, handle)] = w
			rec.Plan = &workspacePlan{Workspace: handle, Staging: staging, Device: p.Dev, Inode: p.Inode}
			if err := r.save(); err != nil {
				return err
			}
			return r.point("clone_before_rename")
		})
		if err != nil {
			return r.fileFailure(rec, err, "clone failed or its publication is uncertain")
		}
		if err := r.point("clone_after_rename"); err != nil {
			return r.fileFailure(rec, err, "clone result is uncertain")
		}
		w := r.state.Workspaces[workspaceKey(target, handle)]
		if !placementExists(r.placement(w).Dir, w.Device, w.Inode) {
			return r.fileFailure(rec, errors.New("identity"), "clone identity changed")
		}
		w.State = "present"
		w.Staging = ""
		r.state.Workspaces[workspaceKey(target, handle)] = w
		return r.complete(rec, workerproto.Effect{Workspace: handle})
	case "fetch", "remove":
		var b workerproto.Workspace
		json.Unmarshal(m.Body, &b)
		dir, err := r.ownedWorkspace(target, b.Workspace)
		if err != nil {
			return failure("wrong_target", "workspace is not owned by this run")
		}
		w := r.state.Workspaces[workspaceKey(target, b.Workspace)]
		if w.State != "" && w.State != "present" {
			return failure("not_ready", "workspace is not present")
		}
		// File-only and prepared sessions may remove work; a live/uncertain host
		// must end first, even if a process-use snapshot would happen to miss it.
		if m.Op == "remove" {
			run := r.state.Runs[targetKey(target)]
			if run == nil || (run.State != "prepared" && run.State != "ended" && run.State != "gone") {
				return failure("not_ready", "host must end before workspace removal")
			}
		}
		rec, err := r.intent(m)
		if err != nil {
			return failure("unavailable", "workspace intent could not be committed")
		}
		rec.Plan = &workspacePlan{Workspace: w.Handle, Device: w.Device, Inode: w.Inode}
		if m.Op == "fetch" {
			repository, ok := r.workspaceConfig.Repositories[w.Repository]
			if !ok {
				return r.refuseFile(rec, "denied", "repository is not configured on this worker")
			}
			if err := r.save(); err != nil {
				return r.fileFailure(rec, err, "fetch intent could not be committed")
			}
			if err := r.maker().FetchRegistered(ctx, dir, repository); err != nil {
				return r.fileFailure(rec, err, "fetch outcome is uncertain")
			}
			return r.complete(rec, workerproto.Effect{Workspace: w.Handle})
		}
		trash := newID()
		rec.Plan.Trash = trash
		tr := &trashRecord{Target: target, Handle: trash, Workspace: w.Handle, Device: w.Device, Inode: w.Inode, State: "removing"}
		r.state.Trashes[workspaceKey(target, trash)] = tr
		if err := r.save(); err != nil {
			return r.fileFailure(rec, err, "removal plan could not be committed")
		}
		if err := r.point("remove_before_rename"); err != nil {
			return r.fileFailure(rec, err, "removal outcome is uncertain")
		}
		if err := r.remover().RemoveRegistered(ctx, dir, r.trashPath(trash), r.placement(w)); err != nil {
			var refusal *workspace.Refusal
			if errors.As(err, &refusal) {
				tr.State = "refused"
				return r.refuseFile(rec, "proof_refused", "worker-local removal proof refused")
			}
			return r.fileFailure(rec, err, "removal outcome is uncertain")
		}
		if err := r.point("remove_after_rename"); err != nil {
			return r.fileFailure(rec, err, "removal outcome is uncertain")
		}
		tr.State = "retained"
		tr.RemovedMS = r.options.Now().UnixMilli()
		w.State = "trashed"
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
		return r.complete(rec, workerproto.Effect{Workspace: w.Handle, Trash: trash})
	case "restore", "acknowledge", "purge":
		var b workerproto.Trash
		json.Unmarshal(m.Body, &b)
		tr := r.state.Trashes[workspaceKey(target, b.Trash)]
		if tr == nil || tr.Target != target || tr.Handle != b.Trash {
			return failure("wrong_target", "trash handle is not registered for this run")
		}
		w, ok := r.state.Workspaces[workspaceKey(target, tr.Workspace)]
		if !ok || w.Target != target || w.Device != tr.Device || w.Inode != tr.Inode {
			return failure("wrong_target", "trash workspace identity is not registered")
		}
		if m.Op == "purge" && (!tr.Acknowledged || r.workspaceConfig.RetentionSeconds <= 0 || r.options.Now().UnixMilli()-tr.RemovedMS <= r.workspaceConfig.RetentionSeconds*1000) {
			return failure("denied", "trash requires durable acknowledgment and elapsed retention")
		}
		if m.Op == "restore" && tr.State != "retained" {
			return failure("not_ready", "trash is not available for restore")
		}
		if m.Op == "purge" && tr.State != "retained" {
			return failure("not_ready", "trash is not available for purge")
		}
		rec, err := r.intent(m)
		if err != nil {
			return failure("unavailable", "trash intent could not be committed")
		}
		rec.Plan = &workspacePlan{Workspace: w.Handle, Trash: tr.Handle, Device: tr.Device, Inode: tr.Inode}
		if m.Op == "acknowledge" {
			if tr.State != "retained" {
				return r.refuseFile(rec, "not_ready", "trash is not retained")
			}
			tr.Acknowledged = true
			return r.complete(rec, workerproto.Effect{Trash: tr.Handle})
		}
		if m.Op == "restore" {
			if err := r.save(); err != nil {
				return r.fileFailure(rec, err, "restore plan could not be committed")
			}
			if err := r.point("restore_before_rename"); err != nil {
				return r.fileFailure(rec, err, "restore outcome is uncertain")
			}
			if err := r.remover().RestoreRegistered(ctx, r.trashPath(tr.Handle), r.placement(w)); err != nil {
				return r.fileFailure(rec, err, "restore refused or uncertain")
			}
			if err := r.point("restore_after_rename"); err != nil {
				return r.fileFailure(rec, err, "restore outcome is uncertain")
			}
			tr.State = "restored"
			w.State = "present"
			r.state.Workspaces[workspaceKey(target, w.Handle)] = w
			return r.complete(rec, workerproto.Effect{Workspace: w.Handle, Trash: tr.Handle})
		}
		tr.State = "purging"
		if err := r.save(); err != nil {
			return r.fileFailure(rec, err, "purge plan could not be committed")
		}
		if err := r.point("purge_before_delete"); err != nil {
			return r.fileFailure(rec, err, "purge outcome is uncertain")
		}
		if err := r.remover().PurgeRegistered(r.trashPath(tr.Handle), r.placement(w)); err != nil {
			return r.fileFailure(rec, err, "purge refused or uncertain")
		}
		if err := r.point("purge_after_delete"); err != nil {
			return r.fileFailure(rec, err, "purge outcome is uncertain")
		}
		tr.State = "purged"
		w.State = "purged"
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
		return r.complete(rec, workerproto.Effect{Trash: tr.Handle})
	}
	return failure("unsupported_operation", "workspace operation is not supported")
}
func (r *Resident) fileFailure(rec *receiptRecord, err error, message string) *workerproto.Refusal {
	// Keep the on-disk pending intent for identity reconciliation after reopen.
	// Never return a successful result for an undurable transition.
	rec.Receipt.State = "unknown"
	rec.Receipt.Result = nil
	r.ready = false
	return failure("unavailable", message)
}
func (r *Resident) refuseFile(rec *receiptRecord, code, message string) *workerproto.Refusal {
	rec.Receipt.State = "refused"
	rec.Receipt.Refusal = failure(code, message)
	if err := r.save(); err != nil {
		r.ready = false
		return failure("unavailable", "refusal could not be committed")
	}
	return nil
}
func (r *Resident) inspectWorkspace(ctx context.Context, t workerproto.Target, handle string) (workerproto.Inspection, *workerproto.Refusal) {
	dir, err := r.ownedWorkspace(t, handle)
	if err != nil {
		return workerproto.Inspection{}, failure("wrong_target", "workspace is not owned by this run")
	}
	reasons := []string{}
	if err := r.remover().Check(ctx, dir); err != nil {
		var refusal *workspace.Refusal
		if errors.As(err, &refusal) {
			reasons = append(reasons, string(refusal.Reason))
		} else {
			reasons = append(reasons, "proof unavailable")
		}
	}
	return workerproto.Inspection{Workspace: handle, Identity: handle, Clean: len(reasons) == 0, Reasons: reasons}, nil
}
func (r *Resident) recoverWorkspaces() error {
	for _, w := range r.state.Workspaces {
		switch w.State {
		case "", "cloning", "present", "trashed", "purged":
		default:
			return errors.New("worker: invalid workspace state")
		}
	}
	for key, tr := range r.state.Trashes {
		if tr == nil || !validHandle(tr.Handle) || !validHandle(tr.Workspace) || tr.Inode == 0 || workerproto.CheckTarget(tr.Target, tr.Target) != nil || key != workspaceKey(tr.Target, tr.Handle) {
			return errors.New("worker: invalid trash journal")
		}
		w, ok := r.state.Workspaces[workspaceKey(tr.Target, tr.Workspace)]
		// Terminal trash entries are history, never current workspace authority.
		// A restored workspace can move to a new run, and a purged place can
		// be remade with a new inode. Active trash still requires exact identity.
		if tr.State != "restored" && tr.State != "purged" && (!ok || w.Target != tr.Target || w.Device != tr.Device || w.Inode != tr.Inode) {
			return errors.New("worker: invalid trash workspace identity")
		}
		switch tr.State {
		case "removing", "refused":
			if tr.Acknowledged {
				return errors.New("worker: unremoved trash acknowledged")
			}
		case "retained", "restored", "purging", "purged":
			if tr.RemovedMS <= 0 {
				return errors.New("worker: invalid trash retention time")
			}
		default:
			return errors.New("worker: invalid trash state")
		}
	}
	return r.validateFiles()
}

func (r *Resident) syncWorkspaceTransition(op string) error {
	dirs := []string{"workspaces"}
	if op == "clone" {
		dirs = append(dirs, "state/clones", "state")
	} else {
		dirs = append(dirs, "trash")
	}
	for _, dir := range dirs {
		if err := syncDir(r.root, dir); err != nil {
			return err
		}
	}
	return syncDir(r.root, ".")
}
func (r *Resident) recoverFileOperation(rec *receiptRecord) error {
	if rec.Request.Op == "upload" {
		return r.recoverUpload(rec)
	}
	if rec.Request.Op == "fetch" {
		return errors.New("fetch outcome is unknown; never replay")
	}
	if rec.Plan == nil || !validHandle(rec.Plan.Workspace) || rec.Plan.Inode == 0 {
		return errors.New("worker: missing workspace operation identity")
	}
	target := *rec.Request.Target
	p := rec.Plan
	w, ok := r.state.Workspaces[workspaceKey(target, p.Workspace)]
	if !ok || w.Device != p.Device || w.Inode != p.Inode {
		return errors.New("worker: unknown workspace identity")
	}
	placed := r.placement(w)
	effect := workerproto.Effect{Workspace: w.Handle}
	switch rec.Request.Op {
	case "clone", "remake_workspace":
		if !placementExists(placed.Dir, w.Device, w.Inode) {
			if err := r.maker().RecoverClone(context.Background(), p.Staging, placed); err != nil {
				return err
			}
		}
		w.State = "present"
		w.Staging = ""
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
	case "remove":
		tr := r.state.Trashes[workspaceKey(target, p.Trash)]
		if tr == nil {
			return errors.New("worker: removal has no trash identity")
		}
		if !placementExists(r.trashPath(p.Trash), w.Device, w.Inode) {
			// An unperformed destructive intent is never queued for recovery. Keep
			// the directory and report refusal; a new request must prove again.
			if placementExists(placed.Dir, w.Device, w.Inode) {
				tr.State = "refused"
				rec.Receipt.State = "refused"
				rec.Receipt.Refusal = failure("proof_refused", "removal did not reach its recorded trash")
				return nil
			}
			return errors.New("worker: removal identity is unresolved")
		}
		if _, err := os.Lstat(placed.Dir); err == nil || !errors.Is(err, os.ErrNotExist) {
			return errors.New("worker: workspace replacement appeared after removal")
		}
		tr.State = "retained"
		// Start retention conservatively when recovery confirms publication,
		// never at the earlier intent or proof start.
		tr.RemovedMS = r.options.Now().UnixMilli()
		w.State = "trashed"
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
		effect.Trash = p.Trash
	case "restore":
		tr := r.state.Trashes[workspaceKey(target, p.Trash)]
		if tr == nil {
			return errors.New("worker: restore has no trash identity")
		}
		if !placementExists(placed.Dir, w.Device, w.Inode) {
			if err := r.remover().RestoreRegistered(context.Background(), r.trashPath(tr.Handle), placed); err != nil {
				return err
			}
		}
		tr.State = "restored"
		w.State = "present"
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
		effect.Trash = p.Trash
	case "acknowledge":
		tr := r.state.Trashes[workspaceKey(target, p.Trash)]
		if tr == nil || tr.State != "retained" {
			return errors.New("worker: cannot reconcile acknowledgment")
		}
		tr.Acknowledged = true
		effect = workerproto.Effect{Trash: tr.Handle}
	case "purge":
		tr := r.state.Trashes[workspaceKey(target, p.Trash)]
		if tr == nil || !tr.Acknowledged || r.workspaceConfig.RetentionSeconds <= 0 || r.options.Now().UnixMilli()-tr.RemovedMS <= r.workspaceConfig.RetentionSeconds*1000 {
			return errors.New("worker: purge authorization is unknown")
		}
		if err := r.remover().PurgeRegistered(r.trashPath(tr.Handle), placed); err != nil {
			return err
		}
		tr.State = "purged"
		w.State = "purged"
		r.state.Workspaces[workspaceKey(target, w.Handle)] = w
		effect = workerproto.Effect{Trash: tr.Handle}
	default:
		return errors.New("worker: unsupported file recovery")
	}
	if rec.Request.Op == "clone" || rec.Request.Op == "remove" || rec.Request.Op == "restore" || rec.Request.Op == "purge" {
		if err := r.syncWorkspaceTransition(rec.Request.Op); err != nil {
			return err
		}
	}
	rec.Receipt.State = "complete"
	rec.Receipt.Result = &effect
	rec.Receipt.Refusal = nil
	return nil
}
