"""Exercise component guards serially through Go overlays, without editing sources."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[3]
SOURCES = {p.name: p.read_text() for p in (ROOT / "internal/nativehttp").glob("*.go")}


def change(file, before, after):
    assert before in SOURCES[file], (file, before)
    return SOURCES[file].replace(before, after, 1)


OLD_PRIVATE = """func privateFile(root *os.Root, name string, flags int) (*os.File, error) {
 f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
 if err != nil { return nil, err }
 info, err := f.Stat()
 if err == nil { err = private(info, 0, 0o600) }
 if err == nil { err = checkACL(filepath.Join(root.Name(), name)) }
 if err != nil { f.Close(); return nil, err }
 return f, nil
}
"""
MUTANTS = [
    ("relative-in-root-links", "private.go", SOURCES["private.go"].split("func privateFile(")[0] + OLD_PRIVATE, "^TestDiscoveryRefusesChangedBindings/relative-link$"),
    ("private-mode", "private.go", change("private.go", "info.Mode().Perm() != perm", "false"), "^TestRefusePrivateDirectoryFailures/root-mode$"),
    ("foreign-owner", "private.go", change("private.go", "int(stat.Uid) != os.Geteuid()", "false"), "^TestPrivateRefusesForeignOrUnknownOwner$"),
    ("native-lease", "listener.go", change("listener.go", "syscall.LOCK_EX|syscall.LOCK_NB); err != nil {", "syscall.LOCK_EX|syscall.LOCK_NB); err != nil && false {"), "^TestIncompatibleBusyInstancePreservesLockAndDiscovery$"),
    ("root-binding", "discovery.go", change("discovery.go", "d.Root != root.Name()", "false"), "^TestDiscoveryRefusesChangedBindings/root$"),
    ("root-directory-identity", "discovery.go", change("discovery.go", "d.RootInode != uint64(rootStat.Ino)", "false"), "^TestLongRootReplacementRefusesCopiedDiscovery$"),
    ("installation-binding", "discovery.go", change("discovery.go", "d.Instance != instance", "false"), "^TestDiscoveryRefusesChangedBindings/installation$"),
    ("version-binding", "discovery.go", change("discovery.go", "d.Version != cfg.Version", "false"), "^TestIncompatibleBusyInstancePreservesLockAndDiscovery$"),
    ("socket-redirection", "discovery.go", change("discovery.go", "d.Socket != expected", "false"), "^TestDiscoveryRefusesChangedBindings/socket$"),
    ("socket-inode", "discovery.go", change("discovery.go", "uint64(stat.Dev) != d.Device || uint64(stat.Ino) != d.Inode", "uint64(stat.Dev) != d.Device"), "^TestDiscoveryRefusesChangedBindings/inode$"),
    ("replacement-cleanup", "listener.go", change("listener.go", "os.SameFile(current, l.socket)", "current != nil"), "^TestSocketReplacementAndModesRefuse/regular$"),
    ("initialization-cleanup", "listener.go", change("listener.go", "l.socket != nil && os.SameFile(current, l.socket)", "l.socket != nil && false && os.SameFile(current, l.socket)"), "^TestSocketInitializationFailureCleansItsOwnSocket$"),
    ("unavailable-peer-credentials", "peer.go", change("peer.go", 'return fmt.Errorf("native HTTP: peer credentials: %w", err)', "return nil"), "^TestKernelPeerUID$"),
]
record = {
    "scope": "Unintegrated native Unix transport only; not desktop support or full issue acceptance.",
    "source_sha256": {"internal/nativehttp/" + name: hashlib.sha256(source.encode()).hexdigest() for name, source in sorted(SOURCES.items())},
    "negative_guards": [],
    "positive_runs": [],
    "limits": [
        "Cross-user fixture skipped on this unprivileged runner; no sudo or host account changes.",
        "Darwin libc ACL code and fixtures have not executed; no Mac access.",
        "Darwin builds without cgo refuse native access; cross-build is not runtime acceptance.",
        "HTTP authority/bootstrap/stream/WebSocket and daemon integration remain pending explicit handoff.",
        "No local tmux or full-suite tests.",
    ],
}
env = dict(os.environ, GOMAXPROCS="1")
with tempfile.TemporaryDirectory(prefix="native-overlay-") as directory:
    temp = Path(directory)
    for name, file, content, test in MUTANTS:
        mutated = temp / file
        mutated.write_text(content)
        overlay = temp / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(ROOT / "internal/nativehttp" / file): str(mutated)}}))
        cmd = ["go", "test", "-overlay", str(overlay), "-p", "1", "-parallel", "1", "-count=1", "-run", test, "./internal/nativehttp"]
        result = subprocess.run(cmd, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
        behavioral = result.returncode != 0 and "--- FAIL:" in result.stdout and "[build failed]" not in result.stdout
        evidence = [re.sub(r"/(?:private/)?tmp/[^\s:;]+", "<fixture>", line.strip()) for line in result.stdout.splitlines() if "--- FAIL:" in line or ".go:" in line]
        record["negative_guards"].append({"guard": name, "file": "internal/nativehttp/" + file, "test": test, "exit_code": result.returncode, "behavioral_failure": behavioral, "evidence": evidence})
        print(name + ": " + ("behavioral failure observed" if behavioral else "NOT ESTABLISHED"), flush=True)
        if not behavioral:
            raise SystemExit(result.stdout)

for race in [False, True]:
    cmd = ["go", "test", "-json", "-p", "1", "-parallel", "1", "-count=1"] + (["-race"] if race else []) + ["./internal/nativehttp"]
    result = subprocess.run(cmd, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
    events = []
    for line in result.stdout.splitlines():
        event = json.loads(line)
        if event["Action"] in ["pass", "fail", "skip"]:
            events.append({key: event[key] for key in ["Action", "Test", "Elapsed"] if key in event})
    record["positive_runs"].append({"command": "GOMAXPROCS=1 " + " ".join(cmd), "exit_code": result.returncode, "results": events})
    print(("race" if race else "serial") + " baseline exit " + str(result.returncode), flush=True)
    if result.returncode:
        raise SystemExit(result.stdout)

(Path(__file__).parent / "validation.json").write_text(json.dumps(record, indent=2) + "\n")
