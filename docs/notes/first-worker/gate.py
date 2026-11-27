"""Run the isolated first-worker gate serially; never run the local full suite.

Use --mutations only in a disposable development checkout. Each mutation is
restored in finally; compilation failures and timeouts do not prove a guard.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[3]
ENV = {**os.environ, "GOMAXPROCS": "1"}
CHECKS = [
    ("./cmd/hataori", "^TestFirstWorker"),
    ("./internal/store", "^TestMachineMigrationFromPreviousVersionCopy$"),
    ("./internal/core", "^TestAwayQueuedActionsRecheckAuthorityScopeAndOriginalTargets$"),
    ("./internal/runner", "^TestAway(ExpiredWorkerHookIsWithdrawnBeforeHeldAnswer|HeldAnswerSurvivesClosedMainDatabase|MissingReceiptAfterAppliedWriteNeverAllowsRetry)$"),
    ("./internal/worker", "^TestWorker(CloneIsIndependentAndKeepsMachineIdentity|RemovalAckRetentionAndRestore|WorkspaceCrashIdentityRecovery|RemovalRerunsEveryProofClass|CrashNeverAdoptsWorkspaceReplacements|PurgeAndAcknowledgmentCrashRecovery|ProofCannotRedirectToUnclassifiedDestination|SamePhysicalMachineCatalogIncludesMainRoots)$"),
]
MUTATIONS = [
    ("effect deduplication", "internal/worker/operations.go", "if prior := r.state.Receipts[key]; prior != nil {", 'if prior := r.state.Receipts[key]; prior != nil && m.Op != "input" {', "./cmd/hataori", "^TestFirstWorkerLostWriteReplyDedupAndCatchup$"),
    ("catch-up before dispatch", "internal/store/away.go", "if n != 0 {", "if false && n != 0 {", "./cmd/hataori", "^TestFirstWorkerLostWriteReplyDedupAndCatchup$"),
    ("worker removal proof", "internal/workspace/registered.go", "if err := r.check(ctx, abs, root); err != nil {", "if err := r.check(ctx, abs, root); false && err != nil {", "./cmd/hataori", "^TestFirstWorkerRemovalPreservesSoleCopy$"),
    ("fixture process ownership", "cmd/hataori/first_worker_process_test.go", "owned := r != nil && r.owner == o && o.records[r]", "owned := true", "./cmd/hataori", "^TestFirstWorkerProcessOwnershipAndExitBarrier$"),
    ("fixture child exit barrier", "cmd/hataori/first_worker_process_test.go", "case <-r.done:", "case <-time.After(time.Nanosecond):", "./cmd/hataori", "^TestFirstWorkerProcessOwnershipAndExitBarrier$"),
    ("fixture generation quiescence", "cmd/hataori/first_worker_ssh_test.go", "if err := o.waitHandlers(); err != nil {", "if err := error(nil); err != nil {", "./cmd/hataori", "^TestFirstWorkerGenerationWaitsForAuthorizedHandler$"),
    ("fixture parent lease", "cmd/hataori/first_worker_ssh_test.go", "if err != nil || ended || !((parent == main.pid && birth == main.birth) || (parent == o.cli.pid && birth == o.cli.birth)) {", "if false && (err != nil || ended || !((parent == main.pid && birth == main.birth) || (parent == o.cli.pid && birth == o.cli.birth))) {", "./cmd/hataori", "^TestFirstWorkerSSHAuthorityRejectsForeignConnection$"),

    ("fixture registration freeze", "cmd/hataori/first_worker_ssh_test.go", "func (o *firstWorkerSSHOwner) close() error {\n\to.freeze()\n\trecords := o.processes.snapshot()\n\tchildren, observationErr := firstWorkerDescendants(records)", "func (o *firstWorkerSSHOwner) close() error {\n\trecords := o.processes.snapshot()\n\tchildren, observationErr := firstWorkerDescendants(records)\n\to.freeze()", "./cmd/hataori", "^TestFirstWorkerLateSSHDescendantExitEvidence$"),

]


def run(package, pattern):
    command = ["go", "test", "-p", "1", package, "-run", pattern, "-parallel", "1", "-count=1", "-timeout", "150s"]
    result = subprocess.run(command, cwd=ROOT, env=ENV, capture_output=True, text=True, timeout=180)
    return command, result, result.stdout + result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mutations", action="store_true")
    args = parser.parse_args()
    if not args.mutations:
        for package, pattern in CHECKS:
            command, result, output = run(package, pattern)
            print(" ".join(command), flush=True)
            print(output, end="", flush=True)
            if result.returncode:
                raise SystemExit(result.returncode)
        return
    results = []
    for name, filename, old, new, package, pattern in MUTATIONS:
        path = ROOT / filename
        original = path.read_text()
        if original.count(old) != 1:
            raise RuntimeError("mutation anchor is not unique: " + name)
        try:
            path.write_text(original.replace(old, new, 1))
            command, result, output = run(package, pattern)
            proved = result.returncode != 0 and "--- FAIL: Test" in output and "build failed" not in output and "test timed out" not in output
            expected = {
                "effect deduplication": "all original worker turn ends committed",
                "catch-up before dispatch": "unknown receipt blocks catch-up after restart",
                "worker removal proof": "removal accepted untracked sole-copy file",
                "fixture process ownership": "foreign owner accepted",
                "fixture child exit barrier": "cleanup returned before owned child exit",
                "fixture generation quiescence": "generation cleanup returned before authorized handler finished",
                "fixture parent lease": "foreign main generation was admitted",
                "fixture registration freeze": "late SSH descendant evidence missing",
            }
            proved = proved and expected[name] in output
            results.append({"guard": name, "command": " ".join(command), "expected_failure": proved, "exit_code": result.returncode,
                            "evidence": [line.strip() for line in output.splitlines() if ("FAIL" in line or "_test.go:" in line) and "root=" not in line][:8]})
            print(name + ": " + ("expected assertion failure" if proved else "NOT PROVED"), flush=True)
            if not proved:
                raise RuntimeError(output[-3000:])
        finally:
            path.write_text(original)
    (ROOT / "docs/notes/first-worker/negative-guards.json").write_text(json.dumps(results, indent=2) + "\n")


if __name__ == "__main__":
    main()
