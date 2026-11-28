"""Check native HTTP/daemon guards serially with disposable Go overlays."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[3]
FILES = [
    "internal/dashboard/native.go", "internal/dashboard/native_test.go",
    "internal/dashboard/server.go", "internal/dashboard/operate.go",
    "internal/dashboard/attach.go", "internal/dashboard/api.go", "cmd/hataori/native_dashboard.go",
    "cmd/hataori/native_dashboard_test.go", "cmd/hataori/daemon.go",
    "cmd/hataori/main.go",
]
SOURCES = {name: (ROOT / name).read_text() for name in FILES}


def replace(file, before, after, count=1):
    assert before in SOURCES[file], (file, before)
    return SOURCES[file].replace(before, after, count)


NATIVE = "internal/dashboard/native.go"
SERVER = "internal/dashboard/server.go"
OPERATE = "internal/dashboard/operate.go"
DAEMON = "cmd/hataori/daemon.go"
HELPER = "cmd/hataori/native_dashboard.go"
API = "internal/dashboard/api.go"
MUTANTS = [
    ("fixed-native-host", {SERVER: replace(SERVER, 'return host == "instance"', "return true")}, "./internal/dashboard", "^TestNativeRefusesHostileHTTP/foreign_host$"),
    ("native-proxy-fields", {NATIVE: replace(NATIVE, 'strings.HasPrefix(lower, "x-forwarded-")', "false")}, "./internal/dashboard", "^TestNativeRefusesHostileHTTP/X-Forwarded-Port$"),
    ("native-canonical-path", {NATIVE: replace(NATIVE, 'path.Clean(r.URL.Path) != r.URL.Path', 'path.Clean(r.URL.Path) == ""')}, "./internal/dashboard", "^TestNativeRefusesHostileHTTP/trailing_dot_segment$"),
    ("native-route-allowlist", {NATIVE: replace(NATIVE, 'if method == http.MethodGet || method == http.MethodHead {', 'if method == http.MethodGet || method == http.MethodHead { return true;')}, "./internal/dashboard", "^TestNativeRefusesHostileHTTP/new_route$"),
    ("native-owner-identity", {NATIVE: replace(NATIVE, 'person: l.native.Person', 'person: "guest"')}, "./internal/dashboard", "^TestNativeAuthorityAndIsolation$"),
    ("native-http-bearer-refusal", {
        NATIVE: replace(NATIVE, 'lower == "authorization" || ', ''),
        SERVER: replace(SERVER, 'if r.Header.Get("Authorization") != "" {\n\t\t\treturn session{}, false\n\t\t}', 'if false { return session{}, false }'),
    }, "./internal/dashboard", "^TestNativeAuthorityAndIsolation$"),
    ("native-ws-bearer-refusal", {NATIVE: replace(NATIVE, 'if token != "" {', 'if false {')}, "./internal/dashboard", "^TestNative(OperateAuthenticationAndShutdown|AttachAuthenticationAndShutdown)$"),
    ("missing-write-origin", {SERVER: replace(SERVER, 'o == "" || o == "null" || !l.originOK(o, r.Host)', 'o != o', 2)}, "./internal/dashboard", "^TestNativeRefusesHostileHTTP/missing_write_origin$"),
    ("missing-websocket-origin", {SERVER: replace(SERVER, 'o := r.Header.Get("Origin"); o == "" || o == "null" || !l.originOK(o, r.Host)', 'o := r.Header.Get("Origin"); o != "" && (o == "null" || !l.originOK(o, r.Host))')}, "./internal/dashboard", "^TestNativeWebSocketOrigin$"),
    ("native-request-join", {SERVER: replace(SERVER, 's.requests.Wait()', '// omitted request join')}, "./internal/dashboard", "^TestNativeShutdownWaitsForAPI$"),
    ("native-stream-producer-join", {API: replace(API, 'for range ch {\n\t\t\t}', 'for false {}')}, "./internal/dashboard", "^TestNativeShutdownJoins(StreamProducer|HostScreen)$"),
    ("native-preauth-shutdown", {OPERATE: replace(OPERATE, 'closeOnStop := context.AfterFunc(r.Context(), func() { c.Close() })', 'closeOnStop := context.AfterFunc(context.Background(), func() { c.Close() })')}, "./internal/dashboard", "^TestNativePreauthShutdown$"),
    ("daemon-native-startup", {DAEMON: replace(DAEMON, 'ctx.Value(nativeDashboardKey{}).(bool); enabled {', 'ctx.Value(nativeDashboardKey{}).(bool); enabled && false {')}, "./cmd/hataori", "^TestNativeDaemonLifecycle$"),
    ("native-dashboard-audit-via", {DAEMON: replace(DAEMON, 'Via: "dashboard"', 'Via: ""')}, "./cmd/hataori", "^TestNativeDaemonLifecycle$"),
]

record = {
    "scope": "Native daemon/HTTP integration; page/bootstrap release, platform security acceptance and complete issue acceptance remain open.",
    "source_sha256": {name: hashlib.sha256(source.encode()).hexdigest() for name, source in SOURCES.items()},
    "negative_guards": [], "positive_runs": [],
    "limits": [
        "No page source, generated asset or workflow changes; explicit releases from page owners are pending.",
        "Core.Terminal does not join its nested Runner.Terminal producer; core owners must release that scoped fix before complete lifecycle acceptance.",
        "HTTP page/asset delivery and first-message authentication are tested; Electron custom-scheme fetch/duplex runtime is not established.",
        "Darwin ACL and privileged cross-user fixtures have not executed here; Darwin cgo=0 remains fail-closed.",
        "No local tmux/full suite, real Mac, production/configuration, host actions, Claude or paid API.",
        "Draft checks/cross-builds are not final-head or merge-main full Linux/macOS/security acceptance.",
    ],
}
env = dict(os.environ, GOMAXPROCS="1")


def command(args):
    return subprocess.run(args, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)


with tempfile.TemporaryDirectory(prefix="native-http-overlay-") as directory:
    temp = Path(directory)
    for name, changes, package, test in MUTANTS:
        paths = {}
        for index, (file, source) in enumerate(changes.items()):
            changed = temp / f"{index}.go"
            changed.write_text(source)
            paths[str(ROOT / file)] = str(changed)
        overlay = temp / "overlay.json"
        overlay.write_text(json.dumps({"Replace": paths}))
        cmd = ["go", "test", "-overlay", str(overlay), "-p", "1", "-parallel", "1", "-count=1", "-run", test, package]
        result = command(cmd)
        behavioral = result.returncode != 0 and "--- FAIL:" in result.stdout and "[build failed]" not in result.stdout
        evidence = [re.sub(r"/(?:private/)?tmp/[^\s:;]+", "<fixture>", line.strip()) for line in result.stdout.splitlines() if "--- FAIL:" in line or ".go:" in line]
        record["negative_guards"].append({"guard": name, "files": list(changes), "test": test, "exit_code": result.returncode, "behavioral_failure": behavioral, "evidence": evidence})
        print(name + ": " + ("behavioral failure observed" if behavioral else "NOT ESTABLISHED"), flush=True)
        if not behavioral:
            raise SystemExit(result.stdout)

for package, test in [("./internal/dashboard", None), ("./cmd/hataori", "^TestNative")]:
    for race in [False, True]:
        cmd = ["go", "test", "-json", "-p", "1", "-parallel", "1", "-count=1"] + (["-race"] if race else []) + (["-run", test] if test else []) + [package]
        result = command(cmd)
        events = []
        for line in result.stdout.splitlines():
            event = json.loads(line)
            if event["Action"] in ["pass", "fail", "skip"]:
                events.append({key: event[key] for key in ["Action", "Test", "Elapsed"] if key in event})
        record["positive_runs"].append({"command": "GOMAXPROCS=1 " + " ".join(cmd), "exit_code": result.returncode, "results": events})
        print(package + (" race" if race else " serial") + " baseline exit " + str(result.returncode), flush=True)
        if result.returncode:
            raise SystemExit(result.stdout)

(Path(__file__).parent / "http-validation.json").write_text(json.dumps(record, indent=2) + "\n")
