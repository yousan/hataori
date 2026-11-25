#!/usr/bin/env python3
"""Check what a confined session can write with (#868).

usage: writes.py RAW_DIR [SCENARIO ...]

Each scenario starts the installed `claude` interactively on a
pseudo-terminal (capture.py's), in a directory of its own, has the model ask
for one tool after another, each in a turn of its own, ends the session,
resumes it and does the same again.

As in mention.py, whose directories and stand-in for the API this script
uses, nothing of the user's is read and no login is needed: HOME and
CLAUDE_CONFIG_DIR are new temporary directories, and the API is a server of
this script on the loopback interface. The CLI is the real one, the model is
not: to a text that names one of TRIES it answers with that call, whether or
not the session was given the tool, and with "ok" to everything else. The
recorder hook allows each permission request (capture.py), so that a refusal
is the CLI's and not an unanswered prompt.

  RAW_DIR/<name>/outcome.json  for the start and for the resume: the tools
                               of every request to the API that had any; and
                               what came of each call: the hooks, what the
                               CLI answered the model, and the files that
                               the directories held afterwards and had not
                               held before

The output is not scrubbed: run scrub.py on it, and never commit RAW_DIR.
"""

import io
import json
import os
import shutil
import subprocess
import sys
import time
import uuid
from http.server import BaseHTTPRequestHandler

sys.dont_write_bytecode = True  # no __pycache__ in testdata
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mention  # noqa: E402
from capture import Session  # noqa: E402

# The calls that the model asks for, in this order: one that each session may
# make, and the built-in tools that write a file or start something that can,
# in the session's directory, {cwd}. {p} is "start" or "resume".
TRIES = [
    ("read", "Read", {"file_path": "{cwd}/there.md"}),
    ("edit", "Edit", {"file_path": "{cwd}/there.md", "old_string": "there", "new_string": "edited"}),
    ("write_old", "Write", {"file_path": "{cwd}/there.md", "content": "written\n"}),
    ("write_new", "Write", {"file_path": "{cwd}/{p}-new.md", "content": "written\n"}),
    ("multi_edit", "MultiEdit", {"file_path": "{cwd}/there.md", "edits": [{"old_string": "there", "new_string": "written"}]}),
    ("notebook_edit", "NotebookEdit", {"notebook_path": "{cwd}/there.ipynb", "cell_id": "a", "new_source": "written"}),
    ("bash", "Bash", {"command": "echo written > {cwd}/{p}-bash.md", "description": "Write"}),
    ("monitor", "Monitor", {"command": "echo written > {cwd}/{p}-monitor.md", "description": "Write", "timeout_ms": 5000}),
    ("power_shell", "PowerShell", {"command": "echo written > {cwd}/{p}-power.md", "description": "Write"}),
    ("repl", "REPL", {"code": "require('fs').writeFileSync('{cwd}/{p}-repl.md', 'written')"}),
    ("agent", "Agent", {"subagent_type": "general-purpose", "description": "Write", "prompt": "Say ok"}),
    ("task", "Task", {"subagent_type": "general-purpose", "description": "Write", "prompt": "Say ok"}),
    ("skill", "Skill", {"skill": "update-config"}),
    ("placeholder", "DeferredToolPlaceholder", {}),
    ("tool_search", "ToolSearch", {"query": "select:Write,Edit,Bash", "max_results": 3}),
    ("workflow", "Workflow", {"script": "export const meta = {name: 'w', description: 'w'}"}),
    ("enter_worktree", "EnterWorktree", {"name": "{p}-tree"}),
    ("exit_plan_mode", "ExitPlanMode", {"plan": "written"}),
    ("todo_write", "TodoWrite", {"todos": [{"content": "written", "status": "pending", "activeForm": "Writing"}]}),
    ("task_create", "TaskCreate", {"subject": "written", "description": "written"}),
    ("cron_create", "CronCreate", {"cron": "0 0 1 1 *", "prompt": "Say ok"}),
]
FILES = {  # in the session's directory, written anew for each phase
    "there.md": "there\n",
    "there.ipynb": json.dumps({"cells": [{"cell_type": "code", "id": "a", "metadata": {}, "source": "there",
                                          "outputs": [], "execution_count": None}],
                               "metadata": {}, "nbformat": 4, "nbformat_minor": 5}),
}
MARK = "Try the call "


class API(mention.API):
    """mention.py's stand-in, which also makes the calls of TRIES: the text
    of the last message says which, and a result is answered with "ok"."""

    def __init__(self, fill):
        super().__init__()
        self.fill, self.results = fill, {}
        api, inner = self, self.server.RequestHandlerClass

        class Handler(inner):
            def do_POST(self):
                length = int(self.headers.get("content-length") or 0)
                body = self.rfile.read(length)
                call = api.call(self.path, body)
                if call is None:
                    rfile, self.rfile = self.rfile, io.BytesIO(body)
                    try:
                        return inner.do_POST(self)
                    finally:
                        self.rfile = rfile
                api.sent.append(body)
                req, (tool, args) = json.loads(body), call
                block = {"type": "tool_use", "id": "toolu_" + uuid.uuid4().hex[:20], "name": tool, "input": {}}
                msg = {"id": "msg_" + uuid.uuid4().hex[:20], "type": "message", "role": "assistant",
                       "model": req.get("model", "m"), "content": [], "stop_reason": None, "stop_sequence": None,
                       "usage": {"input_tokens": 5, "output_tokens": 1}}
                if not req.get("stream"):
                    msg.update(content=[dict(block, input=args)], stop_reason="tool_use")
                    return self.reply(200, "application/json", json.dumps(msg).encode())
                events = [
                    {"type": "message_start", "message": msg},
                    {"type": "content_block_start", "index": 0, "content_block": block},
                    {"type": "content_block_delta", "index": 0, "delta": {"type": "input_json_delta", "partial_json": json.dumps(args)}},
                    {"type": "content_block_stop", "index": 0},
                    {"type": "message_delta", "delta": {"stop_reason": "tool_use", "stop_sequence": None}, "usage": {"output_tokens": 1}},
                    {"type": "message_stop"},
                ]
                self.reply(200, "text/event-stream",
                           b"".join(b"event: %s\ndata: %s\n\n" % (e["type"].encode(), json.dumps(e).encode()) for e in events))

        self.server.RequestHandlerClass = Handler

    def call(self, path, body):
        """The call to answer a request with, None for "ok"."""
        if "/v1/messages" not in path or "count_tokens" in path:
            return None
        try:
            req = json.loads(body)
            last = [m for m in req["messages"] if m["role"] == "user"][-1]["content"]
        except (ValueError, KeyError, IndexError):
            return None
        if not req.get("tools"):
            return None  # not the conversation: a title, a summary
        blocks = [{"type": "text", "text": last}] if isinstance(last, str) else last
        for b in blocks:
            if b.get("type") == "tool_result":
                c = b.get("content")
                if isinstance(c, list):
                    c = "\n".join(x.get("text", "") for x in c)
                self.results[b["tool_use_id"]] = {"error": bool(b.get("is_error")), "text": c or ""}
                return None
        for b in reversed(blocks):
            text = b.get("text", "")
            if MARK in text:
                name = text.split(MARK, 1)[1].split()[0]
                for n, tool, args in TRIES:
                    if n == name:
                        phase = text.split(MARK, 1)[1].split()[1]
                        return tool, json.loads(self.fill(json.dumps(args), phase))
        return None


def content(*dirs):
    """Every file of dirs, with its content."""
    got = {}
    for d in dirs:
        for at, _, files in os.walk(d):
            for f in files:
                p = os.path.join(at, f)
                try:
                    with open(p, "rb") as fh:
                        got[p] = fh.read()
                except OSError:
                    got[p] = b""
    return got


def command(w, tools):
    return ["claude", "--settings", w.settings, "--permission-mode", "default", "--model", "haiku",
            "--restricted", "--strict-mcp-config", "--mcp-config", w.mcp, "--setting-sources", "user", "--tools", tools]


def tools_of(sent):
    """The lists of tools of the requests, each once, in order."""
    got = []
    for b in sent:
        try:
            names = [t.get("name") for t in json.loads(b).get("tools") or []]
        except ValueError:
            continue
        if names and names not in got:
            got.append(names)
    return got


def scenario(out, name, tools):
    w, sid, phases = mention.World(), str(uuid.uuid4()), {}
    # The hooks of the calls too.
    with open(w.settings) as f:
        s = json.load(f)
    hook = s["hooks"]["Stop"]
    for ev in ("PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "PermissionDenied"):
        s["hooks"][ev] = hook
    with open(w.settings, "w") as f:
        json.dump(s, f)
    api = API(w.fill)
    sessions = os.path.join(w.top, "sessions")
    try:
        for phase in mention.PHASES:
            args = command(w, tools) + (["--resume", sid] if phase == "resume" else ["--session-id", sid])
            s = Session(sessions, phase, args, w.cwd, mention.environ(w, api, True))
            tries, begin = [], len(api.sent)
            for n, text in FILES.items():
                with open(os.path.join(w.cwd, n), "w") as f:
                    f.write(text)
            try:
                s.wait("SessionStart", timeout=60)
                time.sleep(4)  # the CLI takes pastes, and has its MCP server
                for case, tool, _ in TRIES:
                    stops, hooks, before = len(s.calls("Stop")), len(s.calls()), content(w.cwd, w.out)
                    results = set(api.results)
                    s.pasted(MARK + case + " " + phase + " please")
                    s.wait("Stop", stops + 1, timeout=90)
                    time.sleep(1.5)
                    after = content(w.cwd, w.out)
                    events = [s.payload(n) for n in s.calls()[hooks:]]
                    tries.append({
                        "case": case, "tool": tool,
                        "hooks": [p["hook_event_name"] + " " + p["tool_name"] for p in events if p.get("tool_name")],
                        "results": [r for i, r in api.results.items() if i not in results],
                        "changed": sorted(os.path.relpath(p, w.top) for p in after if before.get(p) != after[p])})
                    print(name, phase, tries[-1], file=sys.stderr, flush=True)
            finally:
                s.exit()
            phases[phase] = {"tools": tools_of(api.sent[begin:]), "tries": tries}
    finally:
        api.server.shutdown()
        shutil.rmtree(w.top)  # the temporary directory, created above
    os.makedirs(os.path.join(out, name))
    with open(os.path.join(out, name, "outcome.json"), "w") as f:
        json.dump(dict(phases, flag=tools), f, ensure_ascii=False, indent=1)
        f.write("\n")


SCENARIOS = {  # name: --tools, the rest as the adapter launches a confined session
    "writes_confined": "Read,Glob,Grep",          # as it is launched
    "writes_files": "Read,Write,Edit,Glob,Grep",  # as it was before #868: what the calls do where they are allowed
}


def main():
    out = os.path.abspath(sys.argv[1])
    os.makedirs(out, exist_ok=True)
    version = subprocess.run(["claude", "--version"], capture_output=True, text=True).stdout.strip()
    with open(os.path.join(out, "VERSION"), "w") as f:
        f.write("%s\n%s\n" % (version, time.strftime("%Y-%m-%d")))
    for name, how in SCENARIOS.items():
        if not sys.argv[2:] or name in sys.argv[2:]:
            scenario(out, name, how)


if __name__ == "__main__":
    main()
