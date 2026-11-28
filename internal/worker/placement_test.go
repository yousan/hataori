package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

func TestPlacementResidentRechecksEveryLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits workerproto.Admission
	}{
		{"resident", workerproto.Admission{MaxResident: 1}},
		{"starting", workerproto.Admission{MaxStarting: 1}},
		{"load", workerproto.Admission{MaxLoad: .1}},
		{"memory", workerproto.Admission{MinFreeMemory: 2 << 30}},
		{"disk", workerproto.Admission{MinFreeDisk: 2 << 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, false)
			f.prepare()
			f.r.options.Telemetry = func() workerproto.Telemetry {
				return workerproto.Telemetry{Known: true, ObservedMS: time.Now().UnixMilli(), ValidMS: 10000, MemoryFree: 1 << 30, DiskFree: 1 << 30, Load: .1}
			}
			if tc.name == "resident" || tc.name == "starting" {
				other := workerproto.Target{Thread: 8, Session: "other-session", Run: "other-run"}
				rec := f.receipt(f.request("prepare", "prepare-other", &other, workerproto.Prepare{Policy: []string{}}))
				if rec.State != "complete" {
					t.Fatal(rec)
				}
			}
			m := f.request("start", "limited-start", &f.target, workerproto.Start{AgentConfig: f.configuration, Holder: "own", Workspace: f.workspace, Admission: &tc.limits})
			reply := f.call(m)
			if reply.Kind != "refusal" {
				t.Fatalf("limit admitted start: %s", reply.Body)
			}
			if _, err := os.Stat(filepath.Join(f.root, "state/worker/observed")); err != nil {
				t.Fatal(err)
			}
			if run := f.r.state.Runs[targetKey(f.target)]; run.State != "prepared" {
				t.Fatal("limit check followed a host effect", run.State)
			}
		})
	}
}

func TestPlacementRepositoryResolutionUsesConfiguredAddresses(t *testing.T) {
	f := newWorkspaceFixture(t)
	address := f.options.Config.Workspaces.Repositories["repository-fixture"]
	reply := f.call(f.request("resolve_repository", "", &f.target, workerproto.RepositoryReference{Repository: address}))
	var ref workerproto.RepositoryReference
	if reply.Kind != "reply" || json.Unmarshal(reply.Body, &ref) != nil || ref.Repository != "repository-fixture" {
		t.Fatal(reply)
	}
	reply = f.call(f.request("resolve_repository", "", &f.target, workerproto.RepositoryReference{Repository: "file:///not-registered"}))
	if reply.Kind != "refusal" {
		t.Fatal("unregistered repository gained a clone grant")
	}
}

func TestPlacementRemakesPurgedWorkspaceAtTheSameHandle(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	remove := f.receipt(f.removal(handle))
	if remove.State != "complete" {
		t.Fatal(remove)
	}
	trash := remove.Result.Trash
	if rec := f.receipt(f.request("acknowledge", "ack", &f.target, workerproto.Trash{Trash: trash})); rec.State != "complete" {
		t.Fatal(rec)
	}
	*f.now = f.now.Add(61 * time.Second)
	if rec := f.receipt(f.request("purge", "purge", &f.target, workerproto.Trash{Trash: trash})); rec.State != "complete" {
		t.Fatal(rec)
	}
	m := f.request("remake_workspace", "remake", &f.target, workerproto.Workspace{Workspace: handle})
	rec := f.receipt(m)
	if rec.State != "complete" || rec.Result.Workspace != handle {
		t.Fatal(rec)
	}
	if _, err := os.Stat(filepath.Join(f.root, "workspaces", handle, "file.txt")); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if rec := f.receipt(m); rec.State != "complete" || rec.Result.Workspace != handle {
		t.Fatal("remake replay changed handle", rec)
	}
}

func TestPlacementRemakeNeverReplacesAnEntry(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	remove := f.receipt(f.removal(handle))
	trash := remove.Result.Trash
	f.receipt(f.request("acknowledge", "ack", &f.target, workerproto.Trash{Trash: trash}))
	*f.now = f.now.Add(61 * time.Second)
	f.receipt(f.request("purge", "purge", &f.target, workerproto.Trash{Trash: trash}))
	place := filepath.Join(f.root, "workspaces", handle)
	if err := os.Mkdir(place, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(place, "only-copy"), []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	reply := f.call(f.request("remake_workspace", "occupied-remake", &f.target, workerproto.Workspace{Workspace: handle}))
	if reply.Kind != "refusal" {
		t.Fatal(reply)
	}
	b, err := os.ReadFile(filepath.Join(place, "only-copy"))
	if err != nil || strings.TrimSpace(string(b)) != "keep me" {
		t.Fatal(string(b), err)
	}
}

func TestPlacementRestoredWorkspaceTransfersAndRestarts(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	removed := f.receipt(f.removal(handle))
	if rec := f.receipt(f.request("restore", "restore-cycle-one", &f.target, workerproto.Trash{Trash: removed.Result.Trash})); rec.State != "complete" {
		t.Fatal(rec)
	}
	previous := f.target
	next := workerproto.Target{Thread: previous.Thread, Session: "next-session", Run: "next-run"}
	// No host was started by this storage-only fixture. The old prepared run
	// can transfer only after its durable gone proof is recorded.
	f.r.state.Runs[targetKey(previous)].State = "gone"
	f.r.state.Runs[targetKey(previous)].Boot = "confirmed-previous-boot"
	if err := f.r.save(); err != nil {
		t.Fatal(err)
	}
	if rec := f.receipt(f.request("prepare", "prepare-next", &next, workerproto.Prepare{Policy: []string{}})); rec.State != "complete" {
		t.Fatal(rec)
	}
	if rec := f.receipt(f.request("bind_workspace", "bind-next", &next, workerproto.BindWorkspace{Previous: previous, Workspace: handle})); rec.State != "complete" {
		t.Fatal(rec)
	}
	f.restart()
	if _, err := f.r.ownedWorkspace(next, handle); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.ownedWorkspace(previous, handle); err == nil {
		t.Fatal("historical trash granted old workspace authority")
	}
	f.target = next
	second := f.receipt(f.request("remove", "remove-cycle-two", &next, workerproto.Workspace{Workspace: handle}))
	if second.State != "complete" {
		t.Fatal(second)
	}
	f.receipt(f.request("acknowledge", "ack-cycle-two", &next, workerproto.Trash{Trash: second.Result.Trash}))
	*f.now = f.now.Add(2 * time.Minute)
	if rec := f.receipt(f.request("purge", "purge-cycle-two", &next, workerproto.Trash{Trash: second.Result.Trash})); rec.State != "complete" {
		t.Fatal(rec)
	}
	if rec := f.receipt(f.request("remake_workspace", "remake-cycle-two", &next, workerproto.Workspace{Workspace: handle})); rec.State != "complete" {
		t.Fatal(rec)
	}
	f.restart()
	if _, err := f.r.ownedWorkspace(next, handle); err != nil {
		t.Fatal(err)
	}
}
