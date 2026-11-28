"""Prove the scoped Terminal child joins with serial disposable overlays."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[3]
CORE = "internal/core/core.go"
TEST = "internal/core/terminal_join_test.go"
sources = {name: (ROOT / name).read_text() for name in [CORE, TEST]}
start = sources[CORE].index("func (a *api) Terminal(")
end = sources[CORE].index("\n// workspaceDir", start)
terminal = sources[CORE][start:end]


def change(before, after):
    assert before in terminal, before
    return sources[CORE][:start] + terminal.replace(before, after, 1) + sources[CORE][end:]


mutants = [
    ("terminal-exit-child-join", change("for range in { // the child may still be finishing a store read\n\t\t\t}", "for false {}"), "^TestTerminalJoinsChildBeforeClosing$"),
    ("terminal-replacement-child-join", change("for range in {\n\t\t\t\t}", "for false {}"), "^TestTerminalJoinsOldChildBeforeReplacement/success$"),
    ("terminal-cancellation-during-join", change("if ctx.Err() != nil {", "if false {"), "^TestTerminalCancellationDuringReplacementJoinsOldChild$"),
]
env = dict(os.environ, GOMAXPROCS="1")
record = {
    "scope": "Only core.Terminal exit/replacement child joins; supervisor-authorized disjoint hunks, not full #1071 acceptance.",
    "source_sha256": {name: hashlib.sha256(source.encode()).hexdigest() for name, source in sources.items()},
    "negative_guards": [], "positive_runs": [],
    "limits": [
        "Only disposable in-process core metadata and fake held producers; no agent or host process starts.",
        "No page/build edit; #1013 release does not release #1054/#1108 shared generated hunks.",
        "Darwin ACL/cross-user execution, preserved provider gates and full final-head/exact-main CI remain unmet.",
        "No Mac, local tmux/full suite, Claude/paid API, production/configuration/credential/host action or CI rerun.",
    ],
}


def run(cmd):
    return subprocess.run(cmd, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)


with tempfile.TemporaryDirectory(prefix="terminal-join-overlay-") as directory:
    temp = Path(directory)
    for name, content, test in mutants:
        mutated = temp / "core.go"
        mutated.write_text(content)
        overlay = temp / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(ROOT / CORE): str(mutated)}}))
        cmd = ["go", "test", "-overlay", str(overlay), "-p", "1", "-parallel", "1", "-count=1", "-run", test, "./internal/core"]
        result = run(cmd)
        behavioral = result.returncode != 0 and "--- FAIL:" in result.stdout and "[build failed]" not in result.stdout
        evidence = [re.sub(r"/(?:private/)?tmp/[^\s:;]+", "<fixture>", line.strip()) for line in result.stdout.splitlines() if "--- FAIL:" in line or ".go:" in line]
        record["negative_guards"].append({"guard": name, "file": CORE, "test": test, "exit_code": result.returncode, "behavioral_failure": behavioral, "evidence": evidence})
        print(name + ": " + ("behavioral failure observed" if behavioral else "NOT ESTABLISHED"), flush=True)
        if not behavioral:
            raise SystemExit(result.stdout)

focused = "^Test(Terminal(Joins|CancellationDuringReplacement|Of)|NoTextTerminalIsRefused)"
runs = [
    ["-run", focused, "./internal/core"],
    ["-race", "-run", focused, "./internal/core"],
    ["-run", "^TestNative", "./internal/dashboard", "./cmd/hataori"],
]
for args in runs:
    cmd = ["go", "test", "-json", "-p", "1", "-parallel", "1", "-count=1"] + args
    result = run(cmd)
    events = []
    for line in result.stdout.splitlines():
        event = json.loads(line)
        if event["Action"] in ["pass", "fail", "skip"]:
            events.append({key: event[key] for key in ["Action", "Package", "Test", "Elapsed"] if key in event})
    record["positive_runs"].append({"command": "GOMAXPROCS=1 " + " ".join(cmd), "exit_code": result.returncode, "results": events})
    print(" ".join(args) + ": exit " + str(result.returncode), flush=True)
    if result.returncode:
        raise SystemExit(result.stdout)

(Path(__file__).parent / "terminal-validation.json").write_text(json.dumps(record, indent=2) + "\n")
