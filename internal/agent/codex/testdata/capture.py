#!/usr/bin/env python3
"""Capture Codex app-server traffic and observations for adapter development.

usage: capture.py RAW_DIR [SCENARIO ...]       run all scenarios, or the named ones
       capture.py --scrub RAW_DIR OUT_DIR      committable copy (OUT_DIR must not exist)

Each scenario uses a fresh temporary Git workspace and records all stdio JSON
traffic in wire.jsonl, observations and errors in notes.json, and stderr in
stderr.txt. VERSION and methods.json describe the CLI. Workspaces are removed
only after all scenarios finish. The user's config is checked after each
scenario and never opened for writing. Trust runs only with an isolated
CODEX_HOME. Scrubbing removes identifying values and omits stderr.txt.
"""

import hashlib
import json
import os
import platform
import queue
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import uuid

sys.dont_write_bytecode = True
from confine import clean_env, own_config_dir

# In 0.163.0 request_user_input_async is registered only when the model's
# experimental_supported_tools includes it: gpt-6-luna does, gpt-5.6-luna does not.
MODEL = os.environ.get("CAPTURE_MODEL", "gpt-6-luna")
EFFORT = os.environ.get("CAPTURE_EFFORT", "low")
NAMES = ["start", "command", "file_change", "approval_accept", "approval_decline",
         "question_async", "question_after_turn", "question_plan", "steer", "interrupt",
         "resume", "resume_unknown", "subagent", "usage", "background", "exit", "login_status", "trust"]


def env():
    e = clean_env()
    if os.environ.get("CODEX_HOME"):
        e["CODEX_HOME"] = os.environ["CODEX_HOME"]
    return e


def config_hash():
    path = os.path.expanduser("~/.codex/config.toml")
    try:
        with open(path, "rb") as f:
            return hashlib.sha256(f.read()).hexdigest()
    except FileNotFoundError:
        return "absent"


def proc_table():
    result = {}
    for name in os.listdir("/proc"):
        if not name.isdigit():
            continue
        try:
            with open("/proc/%s/stat" % name) as f:
                stat = f.read().rsplit(")", 1)[1].split()
            with open("/proc/%s/cmdline" % name, "rb") as f:
                argv = [x.decode(errors="replace") for x in f.read().split(b"\0") if x]
                if argv:
                    # Wrapper arguments expose mount paths and shell snapshots; keep only the command after the last separator.
                    separators = [i for i, arg in enumerate(argv) if arg == "--"]
                    tail = argv[separators[-1] + 1:] if separators else argv[1:]
                    argv = [arg[:80] for arg in ([os.path.basename(argv[0])] + tail)[:8]]
            result[int(name)] = (int(stat[1]), int(stat[2]), argv, int(stat[19]))
        except (OSError, ValueError, IndexError):
            continue
    return result


def describe(pids, table, root):
    ids = {p: i for i, p in enumerate(sorted(pids))}
    return [{"pid_rel": ids[p], "ppid_rel": ids.get(table[p][0]),
             "pgid_is_ours": table[p][1] == root, "argv": table[p][2]} for p in sorted(pids)]


def descendant_pids(root_pid, table):
    descendants = set()
    parents = {root_pid}
    while parents:
        children = {p for p, (ppid, _, _, _) in table.items() if ppid in parents and p not in descendants}
        descendants.update(children)
        parents = children
    return descendants


def process_snapshot(root_pid, work):
    table = proc_table()
    descendants = descendant_pids(root_pid, table)
    work = os.path.realpath(work)
    selected = set(descendants)
    for pid in table:
        try:
            cwd = os.path.realpath(os.readlink("/proc/%d/cwd" % pid))
            if os.path.commonpath((work, cwd)) == work:
                selected.add(pid)
        except (OSError, ValueError):
            pass
    return {p: table[p] for p in selected}, descendants


def procs(root_pid, work):
    table, descendants = process_snapshot(root_pid, work)
    entries = describe(set(table), table, root_pid)
    for entry, pid in zip(entries, sorted(table)):
        entry["descendant"] = pid in descendants
    return entries


def end_outside(snapshot, root):
    def alive():
        current = proc_table()
        return {p: info for p, info in snapshot.items()
                if p in current and current[p][3] == info[3] and current[p][1] != root}
    outside = alive()
    for p in outside:
        try:
            os.kill(p, signal.SIGTERM)
        except ProcessLookupError:
            pass
    if outside:
        deadline = time.monotonic() + 5
        while alive() and time.monotonic() < deadline:
            time.sleep(.1)
        for p in alive():
            try:
                os.kill(p, signal.SIGKILL)
            except ProcessLookupError:
                pass
    return [{"outside_group": True, "argv": info[2]} for info in outside.values()]


def group(root):
    table = proc_table()
    return describe({p for p, (_, pgid, _, _) in table.items() if pgid == root}, table, root)


class Server:
    def __init__(self, work, directory, handler=None, number=None, trust=True):
        args = ["codex", "app-server"]
        if trust:
            assert "." not in work
            # thread/start otherwise writes the cwd's Git root as trusted in config.toml.
            args += ["-c", 'projects.%s.trust_level="trusted"' % work]
        self.work = work
        self.started = time.monotonic()
        self.stderr = open(os.path.join(directory, "stderr.txt"), "ab")
        self.wire = open(os.path.join(directory, "wire.jsonl"), "a", encoding="utf-8")
        self.lock = threading.RLock()
        self.p = subprocess.Popen(args, cwd=work, env=env(), stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=self.stderr, start_new_session=True)
        self.number, self.handler, self.n = number, handler, 0
        self.q = queue.Queue()
        self.pending = []
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()

    def log(self, direction, **value):
        with self.lock:
            entry = {"t": round(time.monotonic() - self.started, 3), "dir": direction, **value}
            if self.number is not None:
                entry["server"] = self.number
            self.wire.write(json.dumps(entry, ensure_ascii=False) + "\n")
            self.wire.flush()

    def send(self, msg):
        with self.lock:
            self.log("send", msg=msg)
            self.p.stdin.write((json.dumps(msg) + "\n").encode())
            self.p.stdin.flush()

    def read(self):
        for line in self.p.stdout:
            raw = line.decode("utf-8", errors="replace").rstrip("\r\n")
            try:
                msg = json.loads(raw)
            except ValueError:
                self.log("recv", raw=raw)
                continue
            self.log("recv", msg=msg)
            self.q.put(msg)
        self.q.put(None)

    def next(self, timeout):
        try:
            m = self.q.get(timeout=max(0, timeout))
        except queue.Empty:
            raise TimeoutError("waiting for server") from None
        if m is None:
            raise RuntimeError("app-server exited")
        if isinstance(m, dict) and "method" in m and "id" in m:
            answer = self.handler(m) if self.handler else {"decision": "accept"}
            self.send({"id": m["id"], "result": answer})
        return m

    def wait(self, pred, timeout=300):
        deadline = time.monotonic() + timeout
        for i, m in enumerate(self.pending):
            if pred(m):
                return self.pending.pop(i)
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("waiting for message")
            m = self.next(remaining)
            if pred(m):
                return m
            self.pending.append(m)

    def request(self, method, params, timeout=60):
        self.n += 1
        ident = self.n
        self.send({"id": ident, "method": method, "params": params})
        m = self.wait(lambda x: x.get("id") == ident and "method" not in x, timeout)
        if "error" in m:
            raise RuntimeError("%s: %s" % (method, m["error"]))
        return m.get("result")

    def notify(self, method, params=None):
        self.send({"method": method, "params": params or {}})

    def wait_turn_completed(self, thread_id, timeout=300):
        return self.wait(lambda m: m.get("method") == "turn/completed" and
                         m.get("params", {}).get("threadId") == thread_id, timeout)

    def initialize(self):
        self.request("initialize", {"clientInfo": {"name": "hataori-capture", "version": "0"},
                                    "capabilities": {"experimentalApi": True}})
        self.notify("initialized")

    def close(self, grace=10, snapshot=None):
        if snapshot is None:
            snapshot, _ = process_snapshot(self.p.pid, self.work)
        try:
            self.p.stdin.close()
        except (OSError, ValueError):
            pass
        try:
            self.p.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            pass
        if group(self.p.pid):
            try:
                os.killpg(self.p.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            deadline = time.monotonic() + 5
            while group(self.p.pid) and time.monotonic() < deadline:
                time.sleep(.1)
            if group(self.p.pid):
                try:
                    os.killpg(self.p.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        self.p.wait()
        self.reader.join(timeout=5)
        remaining = group(self.p.pid)
        remaining.extend(end_outside(snapshot, self.p.pid))
        self.wire.close()
        self.stderr.close()
        return remaining


def workspace():
    d = tempfile.mkdtemp(prefix="capture-")
    subprocess.run(["git", "init", "-q"], cwd=d, check=True)
    with open(os.path.join(d, "README.md"), "w") as f:
        f.write("Capture workspace.\n")
    subprocess.run(["git", "add", "README.md"], cwd=d, check=True)
    subprocess.run(["git", "-c", "user.name=capture", "-c", "user.email=capture@example.com",
                    "-c", "commit.gpgsign=false", "commit", "-qm", "Initial commit"], cwd=d, check=True)
    return d


def thread(s, work, approval="never", response=False):
    r = s.request("thread/start", {"cwd": work, "model": MODEL, "config": {"model_reasoning_effort": EFFORT},
                                  "approvalPolicy": approval, "sandbox": "workspace-write",
                                  "developerInstructions": "You are running inside an automated capture. Keep answers short."})
    return r if response else r["thread"]["id"]


def turn(s, tid, prompt, **extra):
    r = s.request("turn/start", {"threadId": tid, "input": [{"type": "text", "text": prompt}],
                                 "effort": EFFORT, **extra})
    return r["turn"]["id"]


def item_started(s):
    return s.wait(lambda m: m.get("method") == "item/started" and
                  m.get("params", {}).get("item", {}).get("type") == "commandExecution")


def schema(out):
    with tempfile.TemporaryDirectory(prefix="capture-schema-") as d:
        subprocess.run(["codex", "app-server", "generate-json-schema", "--experimental", "--out", d], check=True, env=env())
        methods = {}
        for filename, key in [("ClientRequest", "client_requests"), ("ClientNotification", "client_notifications"),
                              ("ServerRequest", "server_requests"), ("ServerNotification", "server_notifications")]:
            with open(os.path.join(d, filename + ".json")) as f:
                data = json.load(f)
            found = set()
            def walk(v):
                if isinstance(v, dict):
                    method = v.get("properties", {}).get("method", {})
                    if isinstance(method, dict):
                        found.update(method.get("enum", []))
                        if "const" in method:
                            found.add(method["const"])
                    for x in v.values():
                        walk(x)
                elif isinstance(v, list):
                    for x in v:
                        walk(x)
            walk(data)
            methods[key] = sorted(found)
        with open(os.path.join(out, "methods.json"), "w") as f:
            json.dump(methods, f, indent=2)


def scenario(name, directory, work, start_info):
    notes, servers = {}, []
    def handler(m):
        if m.get("method") == "item/tool/requestUserInput":
            notes["question_request_seen"] = True
            return {"answers": {q["id"]: {"answers": [q["options"][0]["label"]]} for q in m["params"]["questions"]}}
        return {"decision": "decline" if name == "approval_decline" else "accept"}
    def server(number=None, trust=True):
        s = Server(work, directory, handler, number, trust)
        servers.append(s)
        s.initialize()
        return s
    try:
        if name == "login_status":
            argv = ["codex", "login", "status"]
            p = subprocess.run(argv, capture_output=True, text=True, env=env(), timeout=60)
            notes.update(argv=argv, exit_code=p.returncode, stdout=p.stdout, stderr=p.stderr)
            s = server()
            s.request("account/read", {})
        else:
            s = server(1 if name == "exit" else None, name != "trust")
            if name == "resume":
                tid = start_info["thread_id"]
                s.request("thread/resume", {"threadId": tid})
            elif name == "resume_unknown":
                try:
                    s.request("thread/resume", {"threadId": str(uuid.uuid4())})
                except RuntimeError as e:
                    notes["resume_error"] = str(e)
                return notes
            else:
                result = thread(s, work, "untrusted" if name.startswith("approval_") else "never", response=True)
                tid = result["thread"]["id"]
                if name == "trust":
                    notes["sandbox"] = result.get("sandbox")
            if name == "start":
                start_info.update(thread_id=tid, work=work)
                notes["thread_id"] = tid
            if name == "trust":
                turn(s, tid, "Reply with just: ok.")
                s.wait_turn_completed(tid)
                path = os.path.join(own_config_dir(), "config.toml")
                try:
                    import tomllib
                    with open(path, "rb") as f:
                        config = tomllib.load(f)
                    notes["trusted"] = config.get("projects", {}).get(os.path.realpath(work), {}).get("trust_level") == "trusted"
                except FileNotFoundError:
                    notes["trusted"] = False
                return notes
            prompts = {
                "start": "Before doing anything else, say in one short sentence what you will do. Then run the shell command `echo start-ok`. Then answer: what is 2+3?",
                "command": "Run exactly this one shell command: echo capture-ok. Do not do anything else.",
                "file_change": "Create note.txt containing hi using your file-editing tool (apply_patch), not the shell. Do not do anything else.",
                "approval_accept": "Run exactly this one shell command: touch approved.txt. Do not do anything else.",
                "approval_decline": "Run exactly this one shell command: touch approved.txt. Do not do anything else.",
                "question_async": "Use request_user_input_async to ask the user which colour they like, then wait for the answer and reply with it. Do not do anything else.",
                "question_after_turn": "Use request_user_input_async to ask the user which colour they like, then wait for the answer and reply with it. Do not do anything else.",
                "question_plan": "Use your tool to ask the user one multiple-choice question. Do not do anything else.",
                "steer": "Run exactly this one shell command: sleep 20 && echo done. Do not do anything else.",
                "interrupt": "Run exactly this one shell command and wait until it has finished before you reply: sleep 600. Do not do anything else.",
                "resume": "What number did you answer in your last reply? Reply with just the number.",
                "subagent": "Use your tool for starting a sub-agent (spawn_agent) to start one sub-agent that replies 'sub-ok', and reply with what it said.",
                "usage": "Before doing anything else, say in one short sentence what you will do. Then run the shell command `echo start-ok`. Then answer: what is 2+3?",
                "background": "Run exactly this one shell command in the background so that it outlives this turn: `(sleep 40; echo bg-done) &` then reply with just: started.",
                "exit": "Reply with just: ok."
            }
            extra = {}
            if name == "question_plan":
                s.request("collaborationMode/list", {})
                notes["question_request_seen"] = False
                extra["collaborationMode"] = {"mode": "plan", "settings": {"model": MODEL, "reasoning_effort": EFFORT, "developer_instructions": None}}
            turn_id = turn(s, tid, prompts[name], **extra)
            completed = None
            if name in ("question_async", "question_after_turn"):
                notes["question_item_seen"] = False
                def question_or_completion(m):
                    item = m.get("params", {}).get("item", {})
                    questions = item.get("questions")
                    return (m.get("method") == "item/completed" and
                            isinstance(questions, list) and bool(questions)) or (
                            m.get("method") == "turn/completed" and
                            m.get("params", {}).get("threadId") == tid)
                try:
                    event = s.wait(question_or_completion, 120 if name == "question_after_turn" else 300)
                    if event.get("method") == "turn/completed":
                        completed = event
                    else:
                        notes["question_item_seen"] = True
                        if name == "question_async":
                            s.request("turn/steer", {"threadId": tid, "expectedTurnId": turn_id, "input": [{"type": "text", "text": "blue"}]})
                except TimeoutError:
                    if name != "question_after_turn":
                        raise
                    notes["error"] = "turn did not complete within 120 seconds"
                    s.request("turn/interrupt", {"threadId": tid, "turnId": turn_id})
                    completed = s.wait_turn_completed(tid)
            if name in ("steer", "interrupt"):
                item_started(s)
                if name == "steer":
                    s.request("turn/steer", {"threadId": tid, "expectedTurnId": turn_id, "input": [{"type": "text", "text": "After the command finishes, also say the word steered."}]})
                else:
                    try:
                        s.request("turn/interrupt", {"threadId": tid, "turnId": turn_id})
                    except RuntimeError as e:
                        notes["interrupt_error"] = str(e)
            if name == "usage":
                s.wait(lambda m: m.get("method") == "turn/started" and m.get("params", {}).get("turn", {}).get("id") == turn_id)
                s.request("account/rateLimits/read", {})
            try:
                completed = completed or s.wait_turn_completed(tid, 120 if name == "question_after_turn" else 300)
            except TimeoutError:
                if name != "question_after_turn":
                    raise
                notes["error"] = "turn did not complete within 120 seconds"
                s.request("turn/interrupt", {"threadId": tid, "turnId": turn_id})
                s.wait_turn_completed(tid)
            else:
                if name == "question_after_turn" and "error" not in notes:
                    turn(s, tid, "blue")
                    s.wait_turn_completed(tid)
            if name.startswith("approval_"):
                turn(s, tid, "Create patch.txt containing hi using your file-editing tool (apply_patch), not the shell. Do not do anything else.")
                s.wait_turn_completed(tid)
                notes["files_exist"] = {n: os.path.exists(os.path.join(work, n)) for n in ("approved.txt", "patch.txt")}
            if name == "interrupt":
                notes["interrupted_turn_status"] = completed.get("params", {}).get("turn", {}).get("status")
                notes["procs_after_interrupt"] = procs(s.p.pid, work)
                time.sleep(15)
                notes["procs_after_15s"] = procs(s.p.pid, work)
                s.request("thread/backgroundTerminals/clean", {"threadId": tid})
                time.sleep(3)
                notes["procs_after_clean"] = procs(s.p.pid, work)
                turn(s, tid, "Reply with just: ok.")
                s.wait_turn_completed(tid)
            if name == "background":
                notes["procs_at_0s"] = procs(s.p.pid, work)
                started = time.monotonic()
                count = 0
                messages = []
                for seconds in (45, 60):
                    deadline = started + seconds
                    while time.monotonic() < deadline:
                        try:
                            m = s.next(deadline - time.monotonic())
                            method = m.get("method")
                            count += method == "turn/started"
                            if method and method not in ("thread/tokenUsage/updated", "account/rateLimits/updated"):
                                messages.append(method)
                        except TimeoutError:
                            break
                    if seconds == 45:
                        notes["procs_at_45s"] = procs(s.p.pid, work)
                notes["unrequested_turns"] = count
                notes["messages_after_turn"] = messages
                notes["procs_at_60s"] = procs(s.p.pid, work)
            if name == "exit":
                before = time.monotonic()
                notes["server_1_left_after_end"] = s.close(grace=30)
                notes["server_1_exit_code"] = s.p.returncode
                notes["server_1_seconds_until_exit"] = round(time.monotonic() - before, 3)
                servers.remove(s)
                s = server(2)
                tid = thread(s, work)
                turn(s, tid, "Run exactly this one shell command: sleep 600. Do not do anything else.")
                item_started(s)
                snapshot, _ = process_snapshot(s.p.pid, work)
                os.killpg(s.p.pid, signal.SIGKILL)
                notes["server_2_left_after_end"] = s.close(grace=5, snapshot=snapshot)
                servers.remove(s)
                s = server(3)
                result = s.request("thread/resume", {"threadId": tid})
                notes["resumed_turn_statuses"] = [t.get("status") for t in result.get("thread", {}).get("turns", [])]
    except Exception as e:
        notes["error"] = "%s: %s" % (type(e).__name__, e)
    finally:
        for s in servers:
            left = s.close()
            if s.number:
                notes["server_%s_left_after_end" % s.number] = left
            else:
                notes["left_after_end"] = left
    return notes


def scrub(raw, out):
    sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "claudecode", "testdata"))
    from scrub import Scrubber
    s = Scrubber()
    ids = {}
    def number(m):
        prefix = m.group(1)
        by_prefix = ids.setdefault(prefix, {})
        return by_prefix.setdefault(m.group(), prefix + b"_%06d" % (len(by_prefix) + 1))
    def sensitive(v, account=False):
        if isinstance(v, list):
            return [sensitive(x, account) for x in v]
        if isinstance(v, dict):
            return {k: ("REDACTED" if k in ("encrypted_content", "encryptedContent") or (isinstance(x, str) and
                         ((account and k in ("accountId", "account_id", "userId", "user_id", "id")) or
                          k.lower() in ("plan", "plantype", "plan_type", "accountid", "account_id", "userid", "user_id")))
                         else sensitive(x, account or k == "account")) for k, x in v.items()}
        return v
    def token(m):
        value = m.group()
        if re.fullmatch(rb"[0-9a-fA-F]{32,}", value):
            return b"REDACTED"
        if (re.search(rb"[A-Z]", value) and re.search(rb"[a-z]", value) and
                sum(48 <= c <= 57 for c in value) * 10 >= len(value)):
            return b"REDACTED"
        return value
    os.mkdir(out)
    for root, dirs, files in os.walk(raw):
        dirs.sort()
        for name in sorted(files):
            if name == "stderr.txt":
                continue
            src = os.path.join(root, name)
            dst = os.path.join(out, os.path.relpath(src, raw))
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            with open(src, "rb") as f:
                data = f.read()
            if name.endswith(".jsonl"):
                lines = []
                for line in data.splitlines():
                    try:
                        v = json.loads(line)
                        line = json.dumps(sensitive(v), ensure_ascii=False).encode()
                    except ValueError:
                        pass
                    lines.append(line)
                data = b"\n".join(lines) + (b"\n" if data.endswith(b"\n") else b"")
            elif name.endswith(".json"):
                data = json.dumps(sensitive(json.loads(data)), ensure_ascii=False, indent=2).encode() + b"\n"
            data = s.bytes(data)
            data = re.sub(rb"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_-]+)?", b"REDACTED", data)
            data = re.sub(rb"sk-[A-Za-z0-9_-]{20,}", b"REDACTED", data)
            data = re.sub(rb"\b([a-z]+)_([0-9a-f]{20,})\b", number, data)
            data = re.sub(rb"[A-Za-z0-9+/_=-]{32,}", token, data)
            with open(dst, "wb") as f:
                f.write(data)


def main():
    if len(sys.argv) == 4 and sys.argv[1] == "--scrub":
        return scrub(sys.argv[2], sys.argv[3])
    if len(sys.argv) < 2 or sys.argv[1].startswith("--"):
        sys.exit(__doc__.split("\n\n")[1])
    out = os.path.abspath(sys.argv[1])
    wanted = sys.argv[2:] or NAMES
    unknown = set(wanted) - set(NAMES)
    if unknown:
        sys.exit("unknown scenarios: " + ", ".join(sorted(unknown)))
    if "resume" in wanted and "start" not in wanted:
        wanted = list(wanted) + ["start"]
    os.makedirs(out, exist_ok=True)
    original = config_hash()
    works = []
    start_info = {}
    changed = None
    try:
        version = subprocess.run(["codex", "--version"], capture_output=True, text=True, check=True, env=env(), timeout=60).stdout.strip()
        with open(os.path.join(out, "VERSION"), "w") as f:
            f.write("%s\n%s\n%s\nmodel %s\neffort %s\n" % (version, time.strftime("%Y-%m-%d"), platform.system(), MODEL, EFFORT))
        schema(out)
        for name in NAMES:
            if name not in wanted:
                continue
            print("scenario", name, file=sys.stderr, flush=True)
            directory = os.path.join(out, name)
            os.mkdir(directory)
            if name == "trust" and own_config_dir() is None:
                notes = {"not_run": "needs CODEX_HOME set to a directory of its own with a login made for it; the user's credentials are never copied"}
            elif name == "resume" and not start_info.get("thread_id"):
                notes = {"not_run": "start did not produce a thread id"}
            else:
                work = start_info["work"] if name == "resume" else workspace()
                if name != "resume":
                    works.append(work)
                notes = scenario(name, directory, work, start_info)
            with open(os.path.join(directory, "notes.json"), "w") as f:
                json.dump(notes, f, indent=2)
                f.write("\n")
            if config_hash() != original:
                changed = name
                print("ERROR: Codex config changed after %s" % name, file=sys.stderr)
                break
    finally:
        for work in works:
            shutil.rmtree(work)
        if config_hash() != original and changed is None:
            changed = "end"
            print("ERROR: Codex config changed at end", file=sys.stderr)
    if changed:
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
