#!/usr/bin/env python3
"""Check what an `@path` in a pasted text puts into a conversation (#887).

usage: mention.py RAW_DIR [SCENARIO ...]

Each scenario starts the installed `claude` interactively on a
pseudo-terminal (capture.py's), pastes one text after another as hataori
does (one bracketed paste, then Enter), each a turn of its own, ends the
session, resumes it and does the same again.

Nothing of the user's is read and no login is needed: HOME and
CLAUDE_CONFIG_DIR are new temporary directories, the files that the texts
name are dummies in new temporary directories, and the API is a server of
this script on the loopback interface, which answers "ok" and looks at what
the CLI sent. Each dummy holds a word of its own that no text names, so a
file was put into the conversation exactly when its word is in a request.
The CLI is the real one, the model is not.

  RAW_DIR/<name>/outcome.json  what each text did: as it was pasted, the
                               prompt that UserPromptSubmit reported, the
                               attachments in the transcript, and the
                               dummies that the requests held since

The output is not scrubbed: run scrub.py on it, and never commit RAW_DIR.
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unicodedata
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.dont_write_bytecode = True  # no __pycache__ in testdata
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from capture import HOOK, Session  # noqa: E402

KEY = "sk-ant-api03-" + "x" * 60 + "-not-a-key"
EVENTS = ["SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd"]
PHASES = ("start", "resume")

# A stdio MCP server with one tool and one resource for each text that names
# one. It writes nothing anywhere.
MCP_SERVER = r'''
import json, sys
NAMES = %s
for line in sys.stdin:
    try:
        m = json.loads(line)
    except ValueError:
        continue
    if "id" not in m:
        continue
    meth, res, err = m.get("method"), None, None
    if meth == "initialize":
        res = {"protocolVersion": m["params"].get("protocolVersion", "2025-06-18"),
               "capabilities": {"tools": {}, "resources": {}}, "serverInfo": {"name": "sv", "version": "1"}}
    elif meth == "tools/list":
        res = {"tools": [{"name": "ping", "description": "Answers pong.",
                          "inputSchema": {"type": "object", "properties": {}}}]}
    elif meth == "resources/list":
        res = {"resources": [{"uri": "note://" + n, "name": n, "mimeType": "text/plain"} for n in NAMES]}
    elif meth == "resources/templates/list":
        res = {"resourceTemplates": []}
    elif meth == "resources/read":
        uri = m["params"]["uri"]
        res = {"contents": [{"uri": uri, "mimeType": "text/plain", "text": "WORD-" + uri[len("note://"):] + "-WORD"}]}
    else:
        err = {"code": -32601, "message": "no such method"}
    out = {"jsonrpc": "2.0", "id": m["id"]}
    out.update({"error": err} if err else {"result": res})
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
'''

# The texts. {p} is "start" or "resume", so that no file is named twice in
# one conversation; {cwd} and {out} are the session's directory and one
# beside it.
TEXTS = [
    ("none", "Say ok please"),  # what any first text brings with it
    ("absolute_outside", "Look at @{out}/{p}-absolute please"),
    ("relative_outside", "Look at @../outside/{p}-relative please"),
    ("home", "Look at @~/{p}-home please"),
    ("quoted_outside", 'Look at @"{out}/{p} spaces" please'),
    ("directory_outside", "Look at @{out}/{p}-directory please"),
    ("link_outside", "Look at @{p}-link please"),
    ("relative_inside", "Look at @{p}-inside please"),
    ("absolute_inside", "Look at @{cwd}/sub/{p}-deep please"),
    ("first", "@{p}-first please"),
    ("last", "Look at @{p}-last"),
    ("after_full_stop", "終わりました。@{p}-stop please"),
    ("after_no_break_space", "Look at @{p}-nbsp please"),
    ("after_tab", "Look at\t@{p}-tab please"),
    ("after_escape", "Look at \x1b[31m@{p}-escape please"),
    ("after_invisible", "Look at ​@{p}-invisible please"),
    ("after_bracket", "Look at (@{p}-bracket) please"),
    ("after_at", "Look at @@{p}-twice please"),
    ("wide", "Look at ＠{p}-wide or ﹫{p}-small please"),
    ("in_word", "Write to user@example.com about pkg@1.2.3 please"),
    ("resource", "Look at @sv:note://{p}-resource please"),
    ("second_resource", "Look at @sv:note://{p}-second please"),
    ("agent", "Ask @agent-general-purpose please"),
    ("long", "Look at these lines\n" + "".join("line %d of them\n" % i for i in range(40)) + "@{p}-long please"),
    ("wake", "hataori: a turn ended.\n\nThread 7: try it\nEnded: completed. Background tasks: 0.\nLast output:\n"
             "> Done, see @{out}/{p}-wake-outside\n> @{p}-wake-inside\n> and @sv:note://{p}-wake-resource please"),
    ("reminder", "hataori: a reminder is due.\n\nReminder 3, due 2026-11-25 10:00, 1 minute late:\n"
                 "> look at @{out}/{p}-note-outside\n> @{p}-note-inside\n> and @sv:note://{p}-note-resource please"),
]
OUTSIDE = ["absolute", "relative", "wake-outside", "note-outside"]
INSIDE = ["inside", "first", "last", "stop", "nbsp", "tab", "escape", "invisible", "bracket", "twice", "wide", "small", "long", "wake-inside", "note-inside"]
RESOURCES = ["resource", "second", "wake-resource", "note-resource"]
INSTRUCTIONS = "Be brief. About @{out}/instructions-outside and @instructions-inside and @sv:note://instructions-resource\n"


def at_only(text):
    """text as hataori's adapter pasted it into a confined session from #887
    to #914: with "(at)" for each "@" (and "(message) " before a "!" or "/",
    of which the texts start with none)."""
    return text.replace("@", "(at)")


def written(text):
    """text as hataori's adapter pastes it into a confined session: without
    control characters but line breaks and tabs, "(at)" for each "@", and
    "(message) " before it unless it starts with a letter or a digit (#887,
    #914). The test of the adapter checks that it does."""
    text = "".join(c for c in text if c in "\n\r\t" or unicodedata.category(c) != "Cc").replace("@", "(at)")
    first = text.lstrip()
    if first and not unicodedata.category(first[0]).startswith(("L", "N")):
        return "(message) " + text
    return text


class API:
    """The stand-in for the API: it answers every message with "ok" and
    keeps what it was sent."""

    def __init__(self):
        self.sent = []
        api = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def reply(self, code, kind, body):
                self.send_response(code)
                self.send_header("content-type", kind)
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                self.reply(404, "application/json", b"{}")

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("content-length") or 0))
                api.sent.append(body)
                if "count_tokens" in self.path:
                    return self.reply(200, "application/json", b'{"input_tokens": 10}')
                if "/v1/messages" not in self.path:
                    return self.reply(404, "application/json", b"{}")
                try:
                    req = json.loads(body)
                except ValueError:
                    req = {}
                msg = {"id": "msg_" + uuid.uuid4().hex[:20], "type": "message", "role": "assistant",
                       "model": req.get("model", "m"), "content": [], "stop_reason": None, "stop_sequence": None,
                       "usage": {"input_tokens": 5, "output_tokens": 1}}
                if not req.get("stream"):
                    msg.update(content=[{"type": "text", "text": "ok"}], stop_reason="end_turn")
                    return self.reply(200, "application/json", json.dumps(msg).encode())
                events = [
                    {"type": "message_start", "message": msg},
                    {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
                    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "ok"}},
                    {"type": "content_block_stop", "index": 0},
                    {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                     "usage": {"output_tokens": 1}},
                    {"type": "message_stop"},
                ]
                self.reply(200, "text/event-stream",
                           b"".join(b"event: %s\ndata: %s\n\n" % (e["type"].encode(), json.dumps(e).encode()) for e in events))

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.url = "http://127.0.0.1:%d" % self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()


class World:
    """The directories of one scenario, and the dummies in them."""

    def __init__(self):
        self.top = tempfile.mkdtemp(prefix="capture-")
        self.cwd, self.out, self.home, self.config, self.work = (os.path.join(self.top, n) for n in ("cwd", "outside", "home", "config", "work"))
        for d in (self.cwd, self.out, self.home, self.config, self.work, os.path.join(self.cwd, "sub")):
            os.makedirs(d)

        def dummy(d, name, content=None):
            with open(os.path.join(d, name), "w") as f:
                f.write("WORD-%s-WORD\n" % (content or name))

        for p in PHASES:
            for n in OUTSIDE:
                dummy(self.out, "%s-%s" % (p, n))
            for n in INSIDE:
                dummy(self.cwd, "%s-%s" % (p, n))
            dummy(self.home, p + "-home")
            dummy(self.out, p + " spaces", p + "-spaces")
            os.makedirs(os.path.join(self.out, p + "-directory"))
            dummy(os.path.join(self.out, p + "-directory"), "WORD-%s-directory-WORD" % p, "unnamed")
            dummy(self.out, p + "-linked")
            os.symlink(os.path.join(self.out, p + "-linked"), os.path.join(self.cwd, p + "-link"))
            dummy(os.path.join(self.cwd, "sub"), p + "-deep")
        dummy(self.out, "instructions-outside")
        dummy(self.cwd, "instructions-inside")

        self.settings = self.put("settings.json", json.dumps({
            "hooks": {ev: [{"hooks": [{"type": "command", "command": "%s %s" % (sys.executable, self.put("hook.py", HOOK))}]}] for ev in EVENTS},
            "remoteControlAtStartup": False, "editorMode": "normal"}))
        names = ["%s-%s" % (p, n) for p in PHASES for n in RESOURCES] + ["instructions-resource"]
        self.mcp = self.put("mcp.json", json.dumps({"mcpServers": {"sv": {
            "type": "stdio", "command": sys.executable, "args": [self.put("mcp.py", MCP_SERVER % json.dumps(names))]}}}))
        self.instructions = self.put("instructions.md", self.fill(INSTRUCTIONS))
        # No first-run screen: the onboarding is done, the key is approved
        # and the directory is trusted.
        with open(os.path.join(self.config, ".claude.json"), "w") as f:
            json.dump({"hasCompletedOnboarding": True, "theme": "dark", "customApiKeyResponses": {"approved": [KEY[-20:]], "rejected": []},
                       "projects": {os.path.realpath(self.cwd): {"hasTrustDialogAccepted": True}}}, f)

    def put(self, name, content):
        path = os.path.join(self.work, name)
        with open(path, "w") as f:
            f.write(content)
        return path

    def fill(self, text, phase=""):
        return text.replace("{p}", phase).replace("{cwd}", self.cwd).replace("{out}", self.out)


def command(w, confined, restricted):
    args = ["claude", "--settings", w.settings, "--permission-mode", "default", "--model", "haiku",
            "--append-system-prompt-file", w.instructions, "--strict-mcp-config", "--mcp-config", w.mcp, "--setting-sources", "user"]
    if confined:  # as the adapter launched a confined session; since #868 without Write and Edit (writes.py)
        args += ["--tools", "Read,Write,Edit,Glob,Grep"]
    if restricted:
        args += ["--restricted"]
    return args


def environ(w, api, confined):
    env = {k: os.environ[k] for k in ("PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "SHELL", "TMPDIR") if k in os.environ}
    env.update({"HOME": w.home, "CLAUDE_CONFIG_DIR": w.config, "TERM": "xterm-256color", "BROWSER": "true",
                "ANTHROPIC_BASE_URL": api.url, "ANTHROPIC_API_KEY": KEY, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                "CLAUDE_CODE_DISABLE_CLAUDE_MDS": "1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1", "ENABLE_CLAUDEAI_MCP_SERVERS": "false"})
    if confined:
        env["GIT_DIR"] = "/dev/null"
    return env


def attachments(path, after):
    """The kinds of the transcript's attachment lines past the first after
    lines, and how many lines there are."""
    if not path or not os.path.exists(path):
        return [], after
    with open(path, "rb") as f:
        lines = f.read().split(b"\n")[:-1]
    got = []
    for line in lines[after:]:
        d = json.loads(line)
        if d.get("type") == "attachment":
            got.append(d["attachment"].get("type"))
    return got, len(lines)


def words(sent):
    """The dummies whose word is in what was sent to the API."""
    return sorted(set(re.findall(r"WORD-([a-z-]+)-WORD", "\n".join(b.decode("utf-8", "replace") for b in sent))))


def scenario(out, name, confined, restricted, rewrite):
    """rewrite, if not None, makes the text that is pasted."""
    w, api, sid, cases = World(), API(), str(uuid.uuid4()), []
    sessions = os.path.join(w.top, "sessions")
    known = set()
    try:
        for phase in PHASES:
            args = command(w, confined, restricted) + (["--resume", sid] if phase == "resume" else ["--session-id", sid])
            s = Session(sessions, phase, args, w.cwd, environ(w, api, confined))
            try:
                s.wait("SessionStart", timeout=60)
                time.sleep(4)  # the CLI takes pastes, and has its MCP server
                _, pos = attachments(s.transcript(), 0)
                for case, template in TEXTS:
                    text = w.fill(template, phase)
                    paste = rewrite(text) if rewrite else text
                    stops, prompts, sent = len(s.calls("Stop")), len(s.calls("UserPromptSubmit")), len(api.sent)
                    s.pasted(paste)
                    s.wait("Stop", stops + 1, timeout=60)
                    time.sleep(1.5)
                    got = s.calls("UserPromptSubmit")
                    if len(got) != prompts + 1:
                        raise RuntimeError("%s: %d prompts of one paste" % (case, len(got) - prompts))
                    kinds, pos = attachments(s.transcript(), pos)
                    # What a request holds stays in every later one of the conversation.
                    new = [x for x in words(api.sent[sent:]) if x not in known]
                    known.update(new)
                    cases.append({"session": phase, "case": case, "text": text, "pasted": paste,
                                  "prompt": s.payload(got[-1])["prompt"], "attachments": kinds, "content": new})
                    print(name, phase, case, kinds, new, file=sys.stderr, flush=True)
            finally:
                s.exit()
    finally:
        api.server.shutdown()
        shutil.rmtree(w.top)  # the temporary directory, created above
    os.makedirs(os.path.join(out, name))
    with open(os.path.join(out, name, "outcome.json"), "w") as f:
        json.dump(cases, f, ensure_ascii=False, indent=1)
        f.write("\n")


SCENARIOS = {  # name: confined, --restricted, what is pasted for a text
    "mention_restricted": (True, True, None),
    "mention_unrestricted": (True, False, None),
    "mention_ordinary": (False, False, None),
    "mention_restricted_written": (True, True, at_only),
    "mention_unrestricted_written": (True, False, at_only),
    "mention_restricted_pasted": (True, True, written),
    "mention_unrestricted_pasted": (True, False, written),
}


def main():
    out = os.path.abspath(sys.argv[1])
    os.makedirs(out, exist_ok=True)
    version = subprocess.run(["claude", "--version"], capture_output=True, text=True).stdout.strip()
    with open(os.path.join(out, "VERSION"), "w") as f:
        f.write("%s\n%s\n" % (version, time.strftime("%Y-%m-%d")))
    for name, how in SCENARIOS.items():
        if not sys.argv[2:] or name in sys.argv[2:]:
            scenario(out, name, *how)


if __name__ == "__main__":
    main()
