package cipolicy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func command(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command(args[0], args[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatal(args, e, string(b))
	}
	return string(b)
}

func TestDispatchRequestMatchesRepositoryValidator(t *testing.T) {
	s, f, _, _ := storeFixture(t)
	ctx := context.Background()
	snap, o := observed(t, s, f, 1)
	if _, err := s.Advance(ctx, snap.Version, o); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Read(ctx)
	control, err := f.adapter.control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.adapter.prepareDispatch(ctx, control, snap.Intent.Sweep)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(prepared.request.Body)
	var body struct {
		Ref    string
		Inputs map[string]string
	}
	if err = json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../.github/ci/proof.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "rerun", "wrong-ref", "wrong-head", "wrong-title", "missing-authority", "wrong-purpose", "unknown-input"} {
		t.Run(scenario, func(t *testing.T) {
			inputs := map[string]string{}
			for k, v := range body.Inputs {
				inputs[k] = v
			}
			attempt := "1"
			ref := "refs/heads/" + body.Ref
			head := snap.Intent.Sweep.Target
			switch scenario {
			case "rerun":
				attempt = "2"
			case "wrong-ref":
				ref = "refs/heads/main"
			case "wrong-head":
				head = sha(999)
			case "wrong-title":
				inputs["title"] = "candidate success"
			case "missing-authority":
				delete(inputs, "authority")
			case "wrong-purpose":
				inputs["purpose"] = "release"
			case "unknown-input":
				inputs["cheap"] = "true"
			}
			event, _ := json.Marshal(map[string]any{"inputs": inputs})
			file := filepath.Join(t.TempDir(), "event.json")
			os.WriteFile(file, event, 0600)
			cmd := exec.Command("python3", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_RUN_ATTEMPT=" + attempt, "GITHUB_REF=" + ref, "GITHUB_SHA=" + head, "GITHUB_EVENT_PATH=" + file}
			output, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "valid") {
				t.Fatal(scenario, string(output), err)
			}
		})
	}
}

func TestBundlePinsClosureAndDevelopmentStaticContract(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "git", "init", "-q")
	command(t, repo, "git", "config", "user.email", "fixture@example.invalid")
	command(t, repo, "git", "config", "user.name", "Fixture")
	for _, name := range []string{ProofWorkflow, ".github/ci/proof.py", ".github/ci/tmux-proof.sh", ".github/ci/tmux-sha256.txt", "go.mod", "go.sum", "web/package.json", "web/package-lock.json", "web/.nvmrc", "cmd/ci-feedback/main.go", "cmd/ci-coordinator/main.go", "internal/cipolicy/actions.go"} {
		b, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(repo, name)
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, b, 0600)
	}
	command(t, repo, "git", "add", ".")
	command(t, repo, "git", "commit", "-qm", "fixture")
	revision := strings.TrimSpace(command(t, repo, "git", "rev-parse", "HEAD"))
	b, err := Bundle(repo, revision, Digest([]byte("binary")))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "UNSIGNED / INACTIVE / actual dispatch unproved" || b.Requirement.WorkflowID != 0 || len(b.Requirement.CompatibilityJobs) != 10 || len(b.Requirement.RequiredSteps["development"]) != 2 {
		t.Fatal(b)
	}
	workflow, _ := os.ReadFile(filepath.Join(repo, ProofWorkflow))
	text := string(workflow)
	for _, step := range b.Requirement.RequiredSteps["development"] {
		if !strings.Contains(text, "name: "+step) {
			t.Fatal("manifest/workflow step mismatch", step)
		}
	}
	if strings.Contains(text, "pull_request:") || strings.Contains(text, "cancel-in-progress:") || strings.Contains(text, "schedule:") {
		t.Fatal("proof workflow auto-activated")
	}
	for _, name := range []string{"go.mod", "go.sum", "web/package-lock.json", ".github/ci/tmux-sha256.txt", "cmd/ci-feedback/main.go", "internal/cipolicy/actions.go"} {
		if b.ExecutionFiles[name] == "" {
			t.Fatal("unbound execution dependency", name)
		}
	}
	os.WriteFile(filepath.Join(repo, ".github/ci/proof.py"), []byte("candidate override"), 0600)
	same, err := Bundle(repo, revision, Digest([]byte("binary")))
	if err != nil || same.ExecutionFiles[".github/ci/proof.py"] != b.ExecutionFiles[".github/ci/proof.py"] {
		t.Fatal("candidate working tree changed closure", err)
	}
}

func TestLiveGitHubSummary(t *testing.T) {
	if !*liveGitHub {
		t.Skip("explicit read-only metadata probe")
	}
	a := &Adapter{root: AuthorityRoot{MaxRequests: 32, RepositoryID: 1454788309, Repository: "yousan/hataori", ControlRef: "main", ExpiresAt: time.Now().Add(time.Hour)}, base: "https://api.github.com", client: &http.Client{Timeout: 30 * time.Second}, clock: time.Now}
	summary, err := a.Summarize(context.Background(), 46097032227)
	if err != nil {
		t.Fatal(err)
	}
	skip := 0
	for _, j := range summary.Jobs {
		if j.Conclusion == "skipped" {
			skip++
			if j.ElapsedSeconds != nil {
				t.Fatal("skip given runtime", j)
			}
		}
	}
	if summary.Head != "dfe9ff8baaf7b7d863b1c6b154a0fab5079e7c82" || skip != 2 || summary.Tokens != "unknown" || summary.Money != "unknown" {
		t.Fatal(summary)
	}
	t.Log("actual run/jobs metadata parsed: four successes, two full-suite skips; tokens/money unknown; no approval or validation credit")
}
