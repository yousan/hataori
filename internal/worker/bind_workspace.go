package worker

import (
	"encoding/json"
	"github.com/yousan/hataori/internal/workerproto"
)

// Moving an authority record keeps the same directory and inode. Old run
// operations lose their handle before the new run can start; retained old
// receipts remain historical results, never fresh removal authority.
func (r *Resident) transferWorkspace(t workerproto.Target, b workerproto.BindWorkspace) *workerproto.Refusal {
	next, old := r.state.Runs[targetKey(t)], r.state.Runs[targetKey(b.Previous)]
	if next == nil || old == nil || next.State != "prepared" || t == b.Previous || t.Thread != b.Previous.Thread || r.checkMarker(next) != nil || r.checkMarker(old) != nil {
		return failure("wrong_target", "workspace transfer does not name owned runs of one thread")
	}
	if old.State != "ended" && old.State != "gone" {
		return failure("not_ready", "previous host is not confirmed ended")
	}
	if old.State == "ended" && !r.ended(old) {
		return failure("not_ready", "previous host end cannot be verified")
	}
	if _, err := r.ownedWorkspace(b.Previous, b.Workspace); err != nil {
		return failure("missing", "previous workspace identity cannot be verified")
	}
	w := r.state.Workspaces[workspaceKey(b.Previous, b.Workspace)]
	if w.State != "" && w.State != "present" {
		return failure("not_ready", "workspace is not present")
	}
	if _, ok := r.state.Workspaces[workspaceKey(t, b.Workspace)]; ok {
		return failure("denied", "new run already has workspace authority")
	}
	delete(r.state.Workspaces, workspaceKey(b.Previous, b.Workspace))
	w.Target = t
	r.state.Workspaces[workspaceKey(t, b.Workspace)] = w
	return nil
}

func (r *Resident) bindWorkspace(m workerproto.Message) *workerproto.Refusal {
	var b workerproto.BindWorkspace
	json.Unmarshal(m.Body, &b)
	// Validate against a temporary journal copy before accepting the effect.
	before := r.state.Workspaces
	copy := make(map[string]workspaceRecord, len(before))
	for k, v := range before {
		copy[k] = v
	}
	r.state.Workspaces = copy
	refusal := r.transferWorkspace(*m.Target, b)
	r.state.Workspaces = before
	if refusal != nil {
		return refusal
	}
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "workspace binding intent could not be committed")
	}
	if refusal := r.transferWorkspace(*m.Target, b); refusal != nil {
		return r.uncertain(rec)
	}
	return r.complete(rec, workerproto.Effect{Workspace: b.Workspace})
}
