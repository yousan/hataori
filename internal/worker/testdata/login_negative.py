#!/usr/bin/env python3
"""Run serial negative controls through Go overlays; never edit live sources."""
import json
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[3]
CASES = [
    ("owner_death_pipe", "internal/worker/login_guard.go",
     "lifetime.Read(b[:])\n\t\tend()", "lifetime.Read(b[:])\n\t\treturn",
     "./internal/worker", "^TestWorkerLoginOwnerDeathClosesGuard$",
     "login child survived abrupt owner death"),
    ("selected_codex_directory", "internal/agent/codex/login.go",
     "Env: a.sessionEnv(spec)", "Env: a.env()",
     "./internal/agent/codex", "^TestWorkerLoginUsesSelectedConfiguration$",
     "login used another configuration"),
    ("login_line_bound", "internal/worker/login_attempt.go",
     "if len(w.pending)+take > loginLineLimit {", "if false {",
     "./internal/worker", "^TestWorkerLoginOutputGuard$", "oversized output was accepted"),
    ("status_output_bound", "internal/worker/login_command.go",
     "if len(b) > w.remaining {", "if false {",
     "./internal/worker", "^TestWorkerLoginStatusExecutionIsBoundedAndMustExitCleanly$/status-flood$",
     "failed/unbounded status command certified login"),
    ("status_clean_exit", "internal/worker/login_guard.go",
     "if cmd.Wait() == nil {", "if cmd.Wait() == nil || true {",
     "./internal/worker", "^TestWorkerLoginStatusExecutionIsBoundedAndMustExitCleanly$/status-failed-claims$",
     "failed/unbounded status command certified login"),
    ("descendant_cleanup", "internal/worker/login_guard.go",
     "status.Close()\n\tend()", "status.Close()\n\treturn 0",
     "./internal/worker", "^TestWorkerLoginGuardEndsDescendantAfterCleanLeaderExit$",
     "guard left a descendant alive after its parent exited"),
    ("configuration_metadata_match", "internal/machineio/login.go",
     " || health.ConfigDir != expectedDir", "",
     "./internal/worker", "^TestWorkerLoginMismatchedDirectoryNeverStartsCommand$",
     "wrong-directory outage started a command"),
    ("owner_start", "internal/relay/relay.go",
     "if !r.isOwner(from) {", "if false {",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "non-owner started worker login"),
    ("closed_outage_input", "internal/relay/relay.go",
     "if !l.Open {", "if l.Open && false {",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "closed outage accepted input before event cleanup"),
    ("outage_machine_match", "internal/session/worker_login.go",
     " || l.Machine != expected.Machine", "",
     "./internal/session", "^TestWorkerLoginClosesOnlyOriginalMachineOutage$",
     "closed = true, want false"),
    ("stored_spec_case", "internal/relay/worker_login.go",
     "$.spec.ConfigDir", "$.Spec.ConfigDir",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "relay: the agent cannot be logged in from here"),
    ("default_directory_match", "internal/relay/worker_login.go",
     "coalesce(json_extract(s.spec,'$.spec.ConfigDir'),'')", "json_extract(s.spec,'$.spec.ConfigDir')",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/default$",
     "relay: the agent cannot be logged in from here"),
    ("disconnect_during_blocked_input", "internal/worker/login_stream.go",
     "defer f.cancel()", "defer func() {}()",
     "./internal/worker", "^TestWorkerLoginDisconnectInterruptsBlockedInput$",
     "disconnect did not interrupt blocked CLI input"),
    ("forbidden_persistence_instrumentation", "internal/worker/login_stream.go",
     "json.Unmarshal(m.Body, &input)",
     "json.Unmarshal(m.Body, &input)\n\t\t\tr.root.WriteFile(\"state/worker/login-unexpected.log\", m.Body, 0600)",
     "./internal/worker", "^TestWorkerLoginOwnerOutageAndPersistence$/explicit$",
     "login marker persisted in login-unexpected.log"),
]


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: login_negative.py OUTPUT_JSON")
    env = dict(os.environ, GOMAXPROCS="1")
    results = []
    with tempfile.TemporaryDirectory(prefix="worker-login-negative-") as temp:
        directory = Path(temp)
        for name, relative, before, after, package, pattern, assertion in CASES:
            source = ROOT / relative
            text = source.read_text()
            if before not in text:
                raise SystemExit(f"missing mutation anchor: {name}")
            modified = directory / (name + ".go")
            # owner_start intentionally mutates only Start, preserving Code.
            modified.write_text(text.replace(before, after, 1))
            overlay = directory / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(source): str(modified)}}))
            command = ["go", "test", "-p=1", "-parallel=1", "-count=1", "-timeout=25s",
                       "-overlay=" + str(overlay), package, "-run", pattern]
            started = time.monotonic()
            run = subprocess.run(command, cwd=ROOT, env=env, stdout=subprocess.PIPE,
                                 stderr=subprocess.STDOUT, text=True, timeout=35)
            output = run.stdout.replace(str(ROOT), "<repo>").replace(temp, "<overlay>")
            accepted = run.returncode != 0 and assertion in output and "[build failed]" not in output
            results.append({"guard": name, "source": relative, "package": package,
                            "kind": "persistence instrumentation" if name == "forbidden_persistence_instrumentation" else "guard removal/reversion",
                            "test": pattern, "exit_code": run.returncode,
                            "seconds": round(time.monotonic() - started, 3),
                            "accepted_negative": accepted, "output": output})
            print(name + ": " + ("observed regression" if accepted else "NOT ESTABLISHED"), flush=True)
    sources = {case[1] for case in CASES}
    sources.update(str(path.relative_to(ROOT)) for path in (ROOT / "internal/worker").glob("login*.go"))
    for pattern in ("internal/agent/codex/worker_login*.go", "internal/session/worker_login*.go",
                    "internal/workerproto/login*.go", "internal/relay/worker_login.go"):
        sources.update(str(path.relative_to(ROOT)) for path in ROOT.glob(pattern))
    hashes = {path: hashlib.sha256((ROOT / path).read_bytes()).hexdigest() for path in sorted(sources)}
    Path(sys.argv[1]).write_text(json.dumps({"method": "Go overlays; serial GOMAXPROCS=1/-p=1/-parallel=1; sources unchanged",
                                          "source_sha256": hashes, "results": results}, indent=2) + "\n")
    return 0 if all(r["accepted_negative"] for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
