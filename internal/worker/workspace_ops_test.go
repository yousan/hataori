package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

type workspaceFixture struct {
	*fixture
	repository, seed string
	env              []string
	now              *time.Time
}

func (f *workspaceFixture) git(dir string, args ...string) string {
	f.t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=WorkerFixture", "-c", "user.email=worker@example.invalid", "-c", "commit.gpgSign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	command.Env = f.env
	out, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fixture git %v: %v %s", args, err, out)
	}
	return string(out)
}
func newWorkspaceFixture(t *testing.T) *workspaceFixture {
	t.Helper()
	base := newFixture(t, false)
	private := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := &workspaceFixture{fixture: base, repository: filepath.Join(private, "independent.git"), seed: filepath.Join(private, "seed"), now: &now}
	f.env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if err := os.Mkdir(f.seed, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(f.seed, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(f.seed, "file.txt"), []byte("tracked\n"), 0600)
	os.WriteFile(filepath.Join(f.seed, ".gitignore"), []byte("*.ignored\n"), 0600)
	f.git(f.seed, "add", ".")
	f.git(f.seed, "commit", "-qm", "initial")
	f.git(private, "clone", "--bare", f.seed, f.repository)
	address := (&url.URL{Scheme: "file", Path: f.repository}).String()
	cfg := WorkspaceConfig{Machine: base.binding.Worker, MainMachine: base.binding.Main, Repositories: map[string]string{"repository-fixture": address}, RootsComplete: true, Roots: []RootLocation{{Machine: base.binding.Worker, Path: base.root}, {Machine: base.binding.Main, Path: filepath.Join(private, "main-root")}}, ProofDestinations: []ProofDestination{{Address: address, Machine: base.binding.Worker, Path: f.repository, Independent: true}}, RetentionSeconds: 60}
	base.r.Close()
	copyConfig := *base.options.Config
	copyConfig.Workspaces = cfg
	base.options.Config = &copyConfig
	base.options.repositoryEnvironment = func() []string { return append([]string(nil), f.env...) }
	base.options.Now = func() time.Time { return *f.now }
	base.open()
	rec := base.receipt(base.request("prepare", "prepare-files", &base.target, workerproto.Prepare{Policy: []string{}}))
	if rec.State != "complete" {
		t.Fatal(rec)
	}
	return f
}
func (f *workspaceFixture) clone() string {
	f.t.Helper()
	rec := f.receipt(f.request("clone", "clone-fixture", &f.target, workerproto.Clone{Repository: "repository-fixture"}))
	if rec.State != "complete" || rec.Result == nil {
		f.t.Fatal(rec)
	}
	return rec.Result.Workspace
}
func (f *workspaceFixture) restart() { f.t.Helper(); f.r.Close(); f.options.checkpoint = nil; f.open() }
func (f *workspaceFixture) removal(handle string) workerproto.Message {
	return f.request("remove", "remove-fixture", &f.target, workerproto.Workspace{Workspace: handle})
}
func TestWorkerCloneIsIndependentAndKeepsMachineIdentity(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	dir, err := f.r.ownedWorkspace(f.target, handle)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Join(f.root, "workspaces") {
		t.Fatal("workspace left selected machine", dir)
	}
	if got := f.git(dir, "remote", "get-url", "origin"); got != f.options.Config.Workspaces.Repositories["repository-fixture"]+"\n" {
		t.Fatal("origin was mirror", got)
	}
	// Only this fixture's cache, never a workspace or operator data.
	if err := os.RemoveAll(filepath.Join(f.root, "mirrors")); err != nil {
		t.Fatal(err)
	}
	f.git(dir, "fsck", "--full")
	inspection := f.call(f.request("inspect", "", &f.target, workerproto.Workspace{Workspace: handle}))
	var result workerproto.Inspection
	if inspection.Kind != "reply" || json.Unmarshal(inspection.Body, &result) != nil || !result.Clean {
		t.Fatalf("inspection: %s", inspection.Body)
	}
	if f.call(f.request("clone", "unknown-repository", &f.target, workerproto.Clone{Repository: "unknown"})).Kind != "refusal" {
		t.Fatal("unregistered repository accepted")
	}
	t.Log("ran: worker-only independent clone, origin, mirror deletion and full proof inspection")
}
func TestWorkerRemovalAckRetentionAndRestore(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	removed := f.receipt(f.removal(handle))
	if removed.State != "complete" {
		t.Fatal(removed)
	}
	trash := removed.Result.Trash
	if again := f.receipt(f.removal(handle)); again.Result.Trash != trash {
		t.Fatal("reply loss duplicated removal")
	}
	*f.now = f.now.Add(2 * time.Minute)
	if f.call(f.request("purge", "unacknowledged", &f.target, workerproto.Trash{Trash: trash})).Kind != "refusal" {
		t.Fatal("unacknowledged trash purged")
	}
	if rec := f.receipt(f.request("restore", "restore-fixture", &f.target, workerproto.Trash{Trash: trash})); rec.State != "complete" || rec.Result.Workspace != handle {
		t.Fatal(rec)
	}
	if _, err := f.r.ownedWorkspace(f.target, handle); err != nil {
		t.Fatal(err)
	}
	removed = f.receipt(f.request("remove", "remove-second", &f.target, workerproto.Workspace{Workspace: handle}))
	if removed.State != "complete" {
		t.Fatal(removed)
	}
	trash = removed.Result.Trash
	if rec := f.receipt(f.request("acknowledge", "ack-fixture", &f.target, workerproto.Trash{Trash: trash})); rec.State != "complete" {
		t.Fatal(rec)
	}
	if f.call(f.request("purge", "too-early", &f.target, workerproto.Trash{Trash: trash})).Kind != "refusal" {
		t.Fatal("retention bypassed")
	}
	*f.now = f.now.Add(2 * time.Minute)
	purged := f.receipt(f.request("purge", "purge-fixture", &f.target, workerproto.Trash{Trash: trash}))
	if purged.State != "complete" {
		t.Fatal(purged)
	}
	if _, err := os.Stat(filepath.Join(f.r.trashPath(trash), "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("acknowledged retained content was not purged", err)
	}
	if entries, err := os.ReadDir(f.r.trashPath(trash)); err != nil || len(entries) != 0 {
		t.Fatal("purge tombstone not empty", err)
	}
	t.Log("ran: duplicate reply receipt, restore same inode, acknowledgment and elapsed retention gate, pinned purge")
}
func TestWorkerWorkspaceCrashIdentityRecovery(t *testing.T) {
	for _, point := range []string{"clone_before_rename", "clone_after_rename", "remove_before_rename", "remove_after_rename", "restore_before_rename", "restore_after_rename"} {
		t.Run(point, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			var m workerproto.Message
			var handle string
			switch point[:5] {
			case "clone":
				m = f.request("clone", "crash-clone", &f.target, workerproto.Clone{Repository: "repository-fixture"})
			default:
				handle = f.clone()
				if point[:6] == "remove" {
					m = f.removal(handle)
				} else {
					removed := f.receipt(f.removal(handle))
					if removed.State != "complete" {
						t.Fatal(removed)
					}
					m = f.request("restore", "crash-restore", &f.target, workerproto.Trash{Trash: removed.Result.Trash})
				}
			}
			f.r.options.checkpoint = func(at string) error {
				if at == point {
					return fmt.Errorf("fixture crash")
				}
				return nil
			}
			reply := f.call(m)
			if reply.Kind != "refusal" {
				t.Fatal("crash acknowledged success", reply)
			}
			f.restart()
			recovered := f.receipt(m)
			if point == "remove_before_rename" {
				if recovered.State != "refused" {
					t.Fatal("destructive intent replayed", recovered)
				}
				if _, err := f.r.ownedWorkspace(f.target, handle); err != nil {
					t.Fatal(err)
				}
			} else if recovered.State != "complete" {
				t.Fatalf("identity not reconciled: %+v", recovered)
			}
			if point == "clone_before_rename" || point == "clone_after_rename" {
				handle = recovered.Result.Workspace
			}
			if point == "remove_after_rename" {
				if !placementExists(f.r.trashPath(recovered.Result.Trash), f.r.state.Workspaces[workspaceKey(f.target, handle)].Device, f.r.state.Workspaces[workspaceKey(f.target, handle)].Inode) {
					t.Fatal("removed identity lost")
				}
			}
			t.Log("ran: recorded inode recovery at", point, "without blind destructive replay")
		})
	}
}
func TestWorkerRemovalProofUsesDeclaredIndependentDestinations(t *testing.T) {
	for _, kind := range []string{"catalog_unknown", "undeclared", "rewritten", "other_machine_root", "unreachable"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			handle := f.clone()
			dir, _ := f.r.ownedWorkspace(f.target, handle)
			switch kind {
			case "catalog_unknown":
				f.r.workspaceConfig.RootsComplete = false
			case "undeclared":
				f.r.workspaceConfig.ProofDestinations = nil
			case "rewritten":
				f.git(dir, "config", "remote.origin.url", "fixture-alias:repo")
				f.git(dir, "config", "url."+f.options.Config.Workspaces.Repositories["repository-fixture"]+".insteadOf", "fixture-alias:repo")
			case "other_machine_root":
				f.r.workspaceConfig.Roots = append(f.r.workspaceConfig.Roots, RootLocation{Machine: "other-machine", Path: "/srv/worker-fixture"})
				p := &f.r.workspaceConfig.ProofDestinations[0]
				p.Machine = "other-machine"
				p.Path = "/srv/worker-fixture/repository.git"
			case "unreachable":
				gone := filepath.Join(t.TempDir(), "missing.git")
				address := (&url.URL{Scheme: "file", Path: gone}).String()
				f.git(dir, "config", "remote.origin.url", address)
				f.r.workspaceConfig.ProofDestinations = []ProofDestination{{Address: address, Machine: f.binding.Worker, Path: gone, Independent: true}}
			}
			receipt := f.receipt(f.removal(handle))
			if receipt.State != "refused" || receipt.Refusal.Code != "proof_refused" {
				t.Fatal("independence proof was bypassed", receipt)
			}
			if _, err := f.r.ownedWorkspace(f.target, handle); err != nil {
				t.Fatal("refusal lost workspace", err)
			}
		})
	}
}

func TestWorkerRetentionStartsAfterProof(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	f.r.options.checkpoint = func(at string) error {
		if at == "remove_before_rename" {
			*f.now = f.now.Add(2 * time.Minute)
		}
		return nil
	}
	removed := f.receipt(f.removal(handle))
	if removed.State != "complete" {
		t.Fatal(removed)
	}
	trash := workerproto.Trash{Trash: removed.Result.Trash}
	f.receipt(f.request("acknowledge", "slow-proof-ack", &f.target, trash))
	refused(t, f.call(f.request("purge", "slow-proof-purge", &f.target, trash)), "denied")
}

func TestWorkerProofRejectsOtherConfiguredMachineOutsideRoot(t *testing.T) {
	f := newWorkspaceFixture(t)
	address := "ssh://main.example.invalid/independent/repository.git"
	f.r.workspaceConfig.ProofDestinations = []ProofDestination{{Address: address, Machine: f.binding.Main, Path: "/independent/repository.git", Independent: true}}
	if err := f.r.authorizeProof(context.Background(), address, address); err == nil {
		t.Fatal("other configured machine accepted outside root")
	}
}

func TestWorkerRemovalRerunsEveryProofClass(t *testing.T) {
	for _, kind := range []string{"modified", "staged", "untracked", "ignored", "unpushed", "reflog", "stash", "nested", "submodule", "process", "git_error", "lsof_error", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			handle := f.clone()
			dir, _ := f.r.ownedWorkspace(f.target, handle)
			// A clean dashboard inspection is diagnostic, never permission to remove.
			var inspection workerproto.Inspection
			inspected := f.call(f.request("inspect", "", &f.target, workerproto.Workspace{Workspace: handle}))
			if inspected.Kind != "reply" || json.Unmarshal(inspected.Body, &inspection) != nil || !inspection.Clean {
				t.Fatal("fixture not clean", string(inspected.Body))
			}
			write := func(name string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte("only worker copy\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			commit := func() { write("only.txt"); f.git(dir, "add", "only.txt"); f.git(dir, "commit", "-qm", "worker only") }
			switch kind {
			case "modified":
				write("file.txt")
			case "staged":
				write("file.txt")
				f.git(dir, "add", "file.txt")
			case "untracked":
				write("untracked.txt")
			case "ignored":
				write("only.ignored")
			case "unpushed":
				commit()
			case "reflog":
				commit()
				f.git(dir, "reset", "--hard", "HEAD~1")
			case "stash":
				write("file.txt")
				f.git(dir, "stash", "push", "-m", "worker-only")
			case "nested":
				nested := filepath.Join(dir, "nested")
				os.Mkdir(nested, 0700)
				f.git(nested, "init", "-q")
				os.WriteFile(filepath.Join(nested, "only"), []byte("only"), 0600)
				f.git(nested, "add", "only")
				f.git(nested, "commit", "-qm", "nested only")
			case "submodule":
				f.git(dir, "-c", "protocol.file.allow=always", "submodule", "add", f.repository, "module")
				f.git(dir, "commit", "-qam", "module")
				f.git(dir, "push", "origin", "HEAD")
				os.WriteFile(filepath.Join(dir, "module", "file.txt"), []byte("module only"), 0600)
			case "process":
				command := exec.Command("sleep", "60")
				command.Dir = dir
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { command.Process.Kill(); command.Wait() })
			case "git_error":
				os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("not a valid head\n"), 0600)
			case "lsof_error":
				bin := t.TempDir()
				os.WriteFile(filepath.Join(bin, "lsof"), []byte("#!/bin/sh\nexit 2\n"), 0700)
				t.Setenv("PATH", bin+string(filepath.ListSeparator)+os.Getenv("PATH"))
			}
			request := f.removal(handle)
			var reply workerproto.Message
			if kind == "timeout" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				reply = f.r.Dispatch(ctx, f.connection, testFrame(t, request))
			} else {
				reply = f.call(request)
			}
			var receipt workerproto.Receipt
			if reply.Kind != "reply" || json.Unmarshal(reply.Body, &receipt) != nil || receipt.State != "refused" || receipt.Refusal.Code != "proof_refused" {
				t.Fatal("worker proof bypassed", string(reply.Body))
			}
			if _, err := f.r.ownedWorkspace(f.target, handle); err != nil {
				t.Fatal("proof refusal moved workspace", err)
			}
		})
	}
}

func TestWorkerCrashNeverAdoptsWorkspaceReplacements(t *testing.T) {
	for _, op := range []string{"clone", "remove", "restore", "purge"} {
		t.Run(op, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			var handle string
			var message workerproto.Message
			checkpoint := op + "_before_rename"
			if op == "clone" {
				message = f.request("clone", "replaced-clone", &f.target, workerproto.Clone{Repository: "repository-fixture"})
			} else {
				handle = f.clone()
				if op == "remove" {
					message = f.removal(handle)
				} else {
					removed := f.receipt(f.removal(handle))
					trash := workerproto.Trash{Trash: removed.Result.Trash}
					if op == "purge" {
						f.receipt(f.request("acknowledge", "replacement-ack", &f.target, trash))
						*f.now = f.now.Add(2 * time.Minute)
						checkpoint = "purge_before_delete"
					}
					message = f.request(op, "replaced-"+op, &f.target, trash)
				}
			}
			f.r.options.checkpoint = func(at string) error {
				if at == checkpoint {
					return errors.New("fixture crash")
				}
				return nil
			}
			if f.call(message).Kind != "refusal" {
				t.Fatal("interrupted operation completed")
			}
			rec := f.r.state.Receipts[receiptKey(f.target, op, message.Key)]
			if op == "clone" {
				handle = rec.Plan.Workspace
			}
			w := f.r.state.Workspaces[workspaceKey(f.target, handle)]
			target := f.r.placement(w).Dir
			if op == "purge" {
				target = f.r.trashPath(rec.Plan.Trash)
			}
			if _, err := os.Lstat(target); err == nil {
				if err := os.Rename(target, target+"-original"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(target, "only-copy")
			os.WriteFile(marker, []byte("replacement"), 0600)
			f.restart()
			recovered := f.receipt(message)
			if recovered.State != "unknown" {
				t.Fatal("replacement adopted", recovered)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "replacement" {
				t.Fatal("replacement overwritten/deleted", err)
			}
		})
	}
}

func TestWorkerPurgeAndAcknowledgmentCrashRecovery(t *testing.T) {
	for _, point := range []string{"acknowledge_intent", "acknowledge_complete", "purge_before_delete", "purge_after_delete", "purge_complete"} {
		t.Run(point, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			handle := f.clone()
			removed := f.receipt(f.removal(handle))
			trash := workerproto.Trash{Trash: removed.Result.Trash}
			op := "acknowledge"
			if strings.HasPrefix(point, "purge") {
				f.receipt(f.request("acknowledge", "purge-crash-ack", &f.target, trash))
				*f.now = f.now.Add(2 * time.Minute)
				op = "purge"
			}
			message := f.request(op, "crash-"+point, &f.target, trash)
			f.r.options.checkpoint = func(at string) error {
				if at == point {
					return errors.New("fixture crash")
				}
				return nil
			}
			if f.call(message).Kind != "refusal" {
				t.Fatal("lost reply returned success")
			}
			f.restart()
			rec := f.receipt(message)
			// A bare acknowledgment intent has no plan yet, so cannot authorize purge.
			if point == "acknowledge_intent" {
				if rec.State != "unknown" {
					t.Fatal(rec)
				}
			} else if rec.State != "complete" {
				t.Fatal(rec)
			}
		})
	}
}

func TestWorkerProofCannotRedirectToUnclassifiedDestination(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	dir, _ := f.r.ownedWorkspace(f.target, handle)
	f.git(f.repository, "update-server-info")
	var redirected atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/declared.git/") {
			http.Redirect(w, request, server.URL+strings.Replace(request.URL.Path, "/declared.git/", "/unclassified.git/", 1)+"?"+request.URL.RawQuery, http.StatusFound)
			return
		}
		redirected.Add(1)
		request.URL.Path = strings.TrimPrefix(request.URL.Path, "/unclassified.git")
		http.FileServer(http.Dir(f.repository)).ServeHTTP(w, request)
	}))
	defer server.Close()
	address := server.URL + "/declared.git"
	f.git(dir, "config", "remote.origin.url", address)
	global := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(global, []byte("[http \""+address+"\"]\n followRedirects = true\n"), 0600)
	for i, v := range f.env {
		if strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") {
			f.env[i] = "GIT_CONFIG_GLOBAL=" + global
		}
	}
	f.r.workspaceConfig.ProofDestinations = []ProofDestination{{Address: address, Machine: "independent-server", Path: "/declared.git", Independent: true}}
	rec := f.receipt(f.removal(handle))
	if rec.State != "refused" || redirected.Load() != 0 {
		t.Fatal("proof followed unclassified redirect", rec, redirected.Load())
	}
}

func TestWorkerSelectedMachineOwnsCloneAndInspection(t *testing.T) {
	selected := newWorkspaceFixture(t)
	other := newWorkspaceFixture(t)
	handle := selected.clone()
	request := selected.request("inspect", "", &selected.target, workerproto.Workspace{Workspace: handle})
	// The same logical target on another resident does not grant its binding.
	reply := other.r.Dispatch(context.Background(), other.connection, testFrame(t, request))
	refused(t, reply, "binding")
	if len(other.r.state.Workspaces) != 0 {
		t.Fatal("nonselected machine acquired workspace")
	}
	reply = selected.call(request)
	var inspection workerproto.Inspection
	if reply.Kind != "reply" || json.Unmarshal(reply.Body, &inspection) != nil || !inspection.Clean {
		t.Fatal("selected machine inspection failed", string(reply.Body))
	}
}

func TestWorkerFetchUsesRegisteredRepositoryWithoutReset(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	dir, _ := f.r.ownedWorkspace(f.target, handle)
	os.WriteFile(filepath.Join(f.seed, "file.txt"), []byte("new upstream\n"), 0600)
	f.git(f.seed, "commit", "-qam", "upstream update")
	f.git(f.seed, "push", f.repository, "HEAD")
	f.git(dir, "config", "remote.origin.url", "file:///unconfigured/repository.git")
	rec := f.receipt(f.request("fetch", "registered-fetch", &f.target, workerproto.Workspace{Workspace: handle}))
	if rec.State != "complete" {
		t.Fatal(rec)
	}
	if got := f.git(dir, "rev-parse", "refs/remotes/origin/main"); got != f.git(f.seed, "rev-parse", "HEAD") {
		t.Fatal("fetch used mutable origin")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "file.txt")); err != nil || string(data) != "tracked\n" {
		t.Fatal("fetch reset working tree", err)
	}
}

func TestWorkerSamePhysicalMachineCatalogIncludesMainRoots(t *testing.T) {
	f := newWorkspaceFixture(t)
	c := f.options.Config.Workspaces
	c.Machine = "one-physical-machine"
	c.MainMachine = c.Machine
	for i := range c.Roots {
		c.Roots[i].Machine = c.Machine
	}
	c.ProofDestinations[0].Machine = c.Machine
	if err := f.r.configureWorkspaces(c); err != nil {
		t.Fatal(err)
	}
	independent := c.ProofDestinations[0].Address
	if err := f.r.authorizeProof(context.Background(), independent, independent); err != nil {
		t.Fatal("independent local repository refused", err)
	}
	handle := f.clone()
	mainRepo := filepath.Join(c.Roots[1].Path, "repository.git")
	if err := os.MkdirAll(c.Roots[1].Path, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(c.Roots[1].Path, "clone", "--bare", f.repository, mainRepo)
	address := "file://" + mainRepo
	f.r.workspaceConfig.ProofDestinations = []ProofDestination{{Address: address, Machine: c.Machine, Path: filepath.Join(c.Roots[1].Path, "repository.git"), Independent: true}}
	if err := f.r.authorizeProof(context.Background(), address, address); err == nil {
		t.Fatal("main root on same physical machine counted as independent")
	}
	found := false
	for _, path := range f.r.remover().OtherRoots {
		if path == c.Roots[1].Path {
			found = true
		}
	}
	if !found {
		t.Fatal("main root omitted from local filesystem proof")
	}
	dir, _ := f.r.ownedWorkspace(f.target, handle)
	f.git(dir, "config", "remote.origin.url", address)
	if rec := f.receipt(f.removal(handle)); rec.State != "refused" {
		t.Fatal("main-root repository accepted for removal", rec)
	}
}
