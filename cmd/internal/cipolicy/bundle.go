package cipolicy

import (
	"fmt"
	"strings"
)

const ProofWorkflow = ".github/workflows/integration-proof.yml"

var compatibilityVersions = []string{"3.2", "3.2a", "3.4", "3.8", "3.0a", "3.1c", "3.3a", "3.5a", "3.6b", "3.7c"}

type ActivationBundle struct {
	RootTemplate                        AuthorityRoot
	PolicyTemplate                      Policy
	Status, Revision, CoordinatorDigest string
	ExecutionFiles                      map[string]string
	Requirement                         ControlRequirement
	Missing                             []string
	Files                               []string
	Commands                            []string
}

// Bundle reads immutable objects only; it does not sign, provision credentials,
// activate a route or assert approval of the source revision or baseline.
func Bundle(repo, revision, binary string) (ActivationBundle, error) {
	b := ActivationBundle{Status: "UNSIGNED / INACTIVE / actual dispatch unproved", Revision: revision, CoordinatorDigest: binary,
		Missing:  []string{"approved merged control and immutable policy bytes", "independently provisioned authority public key/signature", "actual repository/workflow/dispatcher/owner/reviewer identities", "default-branch workflow registration and frozen-ref dispatch evidence", "actual applicable full bootstrap run/attempt/tuple", "parent compatibility and owner opt-in/Done adoption"},
		Files:    []string{"authority.pub (base64 public key)", "root.json (canonical signed bytes)", "root.sig (base64 detached signature)", "dispatcher.token (private external credential)", "coverage.sqlite (created once; retain on rollback)"},
		Commands: []string{"ci-coordinator bootstrap-prepare AUTHORITY_DIR", "ci-coordinator bootstrap-pump AUTHORITY_DIR", "ci-coordinator bootstrap AUTHORITY_DIR", "ci-coordinator bootstrap-recover AUTHORITY_DIR --reason DIAGNOSIS", "ci-coordinator observe AUTHORITY_DIR", "ci-coordinator admission AUTHORITY_DIR --pull N", "ci-coordinator integrate AUTHORITY_DIR --pull N", "ci-coordinator pump AUTHORITY_DIR", "ci-coordinator status AUTHORITY_DIR", "ci-coordinator control-transition AUTHORITY_DIR", "ci-coordinator recover AUTHORITY_DIR --reason DIAGNOSIS", "ci-coordinator done AUTHORITY_DIR --pull N", "ci-coordinator summary AUTHORITY_DIR --run N"}}
	if !digestOK(binary) {
		return b, fmt.Errorf("coordinator binary digest required")
	}
	tree, err := snapshot(repo, revision)
	if err != nil {
		return b, err
	}
	closure := map[string]string{}
	for name := range tree.files {
		critical := name == ProofWorkflow || strings.HasPrefix(name, ".github/ci/") || name == "go.mod" || name == "go.sum" || name == "web/package.json" || name == "web/package-lock.json" || name == "web/.nvmrc" ||
			((strings.HasPrefix(name, "cmd/internal/cipolicy/") || strings.HasPrefix(name, "cmd/ci-coordinator/") || strings.HasPrefix(name, "cmd/ci-feedback/")) && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"))
		if critical {
			data, e := tree.read(name)
			if e != nil {
				return b, e
			}
			closure[name] = Digest(data)
		}
	}
	for _, required := range []string{ProofWorkflow, ".github/ci/proof.py", ".github/ci/tmux-proof.sh", ".github/ci/tmux-sha256.txt", "cmd/ci-feedback/main.go", "cmd/ci-coordinator/main.go", "go.mod", "go.sum", "web/package.json", "web/package-lock.json", "web/.nvmrc"} {
		if closure[required] == "" {
			return b, fmt.Errorf("execution closure incomplete")
		}
	}
	steps := map[string][]string{"contract": {"Validate frozen dispatch"}, "development": {"Development static checks", "Targeted and cross-module smoke"}, "linux": {"Full Linux suite", "Cross-build macOS"}, "macos": {"Full macOS suite"}, "web": {"Reproducible web build"}}
	labels := map[string][]string{"contract": {"ubuntu-latest"}, "development": {"ubuntu-latest"}, "linux": {"ubuntu-latest"}, "macos": {"macos-latest"}, "web": {"ubuntu-latest"}}
	comp := []string{}
	for _, version := range compatibilityVersions {
		name := "compatibility (" + version + ")"
		comp = append(comp, name)
		steps[name] = []string{"Full applicable tmux compatibility"}
		labels[name] = []string{"ubuntu-24.04"}
	}
	b.ExecutionFiles = closure
	b.Commands = append(b.Commands, "ci-coordinator candidate-freeze AUTHORITY_DIR --pull N", "ci-coordinator candidate-verify AUTHORITY_DIR --pull N", "ci-coordinator candidate-main AUTHORITY_DIR --pull N", "ci-coordinator candidate-status AUTHORITY_DIR --pull N", "ci-coordinator candidate-invalidate AUTHORITY_DIR --reason DIAGNOSIS")
	b.Requirement = ControlRequirement{WorkflowPath: ProofWorkflow, ExecutionFiles: closure, LinuxJobs: []string{"contract", "linux", "web"}, MacOSJobs: []string{"macos"}, CompatibilityJobs: comp, DevelopmentJobs: []string{"contract", "development"}, RequiredSteps: steps, RunnerLabels: labels, OptionalSkippedJobs: []string{"development", "compatibility", "compatibility ()"}}
	b.RootTemplate = AuthorityRoot{Version: 1, MaxRequests: 128, CoordinatorDigest: binary, ControlRef: "main", IntegrationRef: "develop", CurrentControl: revision, Controls: map[string]ControlRequirement{revision: b.Requirement}, RoutingEnabled: false}
	b.PolicyTemplate = Policy{Version: 1, Profile: "throughput", RollbackRoute: "main-full", Reason: "UNSIGNED TEMPLATE; actual owner/validity/rollback and merged policy required", Limits: Limits{MaxChanges: 10, MaxAgeSeconds: 21600, LeadTimeSeconds: 1800}}
	return b, nil
}
