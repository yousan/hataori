#!/usr/bin/env python3
"""Capture image paths pasted into the real CLI (#915).

usage: image_paths.py RAW_DIR [SCENARIO ...]

Uses a pseudo-terminal, an isolated HOME and config, dummy PNGs and the
loopback API stand-in from mention.py. No login, user files or tmux. Each
scenario starts and resumes the same session. outcome.json records the
actual UserPromptSubmit prompt and which dummy images reached the API.
Run scrub.py before committing the output.
"""

import base64
import json
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import time
import uuid
import zlib

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from capture import HOOK, Session
from mention import API, KEY, environ, written

PREFIX = "(message) "
IMAGE_END = re.compile(r"\.(png|jpe?g|gif|webp)[\"']?$", re.I)
SLASH_COMMAND = re.compile(r"^/[A-Za-z][A-Za-z0-9_:-]*(?:[ \t]|$)")
TEXTS = [
    ("whole_inside", "{cwd}/{p}-whole.png"),
    ("line_inside", "Look at this\n{cwd}/{p}-line.png\nthen answer"),
    ("whole_outside", "{out}/{p}-outside.png"),
    ("line_outside", "Look at this\n{out}/{p}-line-outside.png"),
    ("link_outside", "Look at this\n{cwd}/{p}-link.png"),
    ("link_inside", "Look at this\n{cwd}/{p}-inside-link.png"),
    ("quoted", 'Look at this\n"{cwd}/{p} spaces.PNG"'),
    ("single_quoted", "Look at this\n'{cwd}/{p} single.png'"),
    ("escaped", "Look at this\n{cwd}/{p}\\ escaped.PNG"),
    ("escaped_extension", "Look at this\n{cwd}/{p}-extension.p\\ng"),
    ("invisible_before", "Look at this\n\u200b{cwd}/{p}-invisible-before.png"),
    ("invisible_extension", "Look at this\n{cwd}/{p}-invisible-extension.p\u200bng"),
    ("invisible_after", "Look at this\n{cwd}/{p}-invisible-after.png\u200b"),
    ("terminal_sequence", "Look at this\n\x1b[31m{cwd}/{p}-terminal.png"),
    ("jpg", "Look at this\n{cwd}/{p}-image.jpg"),
    ("jpeg", "Look at this\n{cwd}/{p}-image.jpeg"),
    ("gif", "Look at this\n{cwd}/{p}-image.gif"),
    ("webp", "Look at this\n{cwd}/{p}-image.webp"),
    ("two_paths", "Look at these\n{out}/{p}-one.png {out}/{p}-two.png"),
    ("relative", "Look at this\n{p}-relative.png"),
    ("wake", "A turn ended.\n> {cwd}/{p}-wake.png"),
]


def no_images(text, shell=False):
    lines = text.split("\n")
    for i, line in enumerate(lines):
        first = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", line).strip()
        path = first.strip("\"'")
        absolute = path.startswith(("/", "\\")) or len(path) > 2 and path[1] == ":"
        if not absolute and (shell or "/" in path or "\\" in path or any(c.isspace() for c in path)):
            continue
        if not first.startswith((PREFIX, "> ")) and IMAGE_END.search(first.replace("\\", "")):
            lines[i] = PREFIX + line
    return "\n".join(lines)


def rewritten(text, confined=False):
    if confined:
        return no_images(written(text))
    first = text.lstrip()
    shell = first.startswith("!")
    if shell or SLASH_COMMAND.match(first):
        end = re.search(r"[\r\n]", first)
        if end is None:
            return text
        offset = len(text) - len(first) + end.start()
        return text[:offset] + no_images(text[offset:], shell)
    return no_images(text)


def png(pixel):
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0)) +
            chunk(b"IDAT", zlib.compress(b"\x00" + bytes([pixel, 73, 121]))) + chunk(b"IEND", b""))


class World:
    def __init__(self):
        self.top = tempfile.mkdtemp(prefix="capture-")
        self.cwd, self.out, self.home, self.config = (os.path.join(self.top, n) for n in ("cwd", "outside", "home", "config"))
        for d in (self.cwd, self.out, self.home, self.config):
            os.makedirs(d)
        self.images = {}
        for p in ("start", "resume"):
            for d, names in ((self.cwd, [p + "-whole.png", p + "-line.png", p + " spaces.PNG", p + " escaped.PNG",
                                         p + " single.png", p + "-extension.png", p + "-image.jpg", p + "-image.jpeg",
                                         p + "-image.gif", p + "-image.webp",
                                         p + "-invisible-before.png", p + "-invisible-extension.png",
                                         p + "-invisible-after.png", p + "-terminal.png",
                                         p + "-inside-linked.png", p + "-relative.png", p + "-wake.png"]),
                             (self.out, [p + "-outside.png", p + "-line-outside.png", p + "-one.png", p + "-two.png", p + "-linked.png"])):
                for name in names:
                    data = png(len(self.images) + 1)
                    with open(os.path.join(d, name), "wb") as f:
                        f.write(data)
                    self.images[base64.b64encode(data).decode()] = name
            os.symlink(os.path.join(self.out, p + "-linked.png"), os.path.join(self.cwd, p + "-link.png"))
            os.symlink(os.path.join(self.cwd, p + "-inside-linked.png"), os.path.join(self.cwd, p + "-inside-link.png"))
        hook = self.put("hook.py", HOOK)
        self.settings = self.put("settings.json", json.dumps({
            "hooks": {ev: [{"hooks": [{"type": "command", "command": "%s %s" % (sys.executable, hook)}]}]
                      for ev in ("SessionStart", "UserPromptSubmit", "Stop", "SessionEnd")},
            "remoteControlAtStartup": False, "editorMode": "normal"}))
        with open(os.path.join(self.config, ".claude.json"), "w") as f:
            json.dump({"hasCompletedOnboarding": True, "theme": "dark", "customApiKeyResponses": {"approved": [KEY[-20:]], "rejected": []},
                       "projects": {os.path.realpath(self.cwd): {"hasTrustDialogAccepted": True}}}, f)

    def put(self, name, text):
        path = os.path.join(self.top, name)
        with open(path, "w") as f:
            f.write(text)
        return path


def images(sent, known):
    """Images in the latest user message, excluding earlier conversation.

    The CLI may re-encode an image, so retain an unnamed one too. Names
    identify dummies when the bytes are unchanged; their count is the
    assertion for both unchanged and re-encoded images.
    """
    found = []
    def visit(value):
        if isinstance(value, dict):
            if value.get("type") == "image":
                data = value.get("source", {}).get("data")
                found.append(known.get(data, "re-encoded-dummy"))
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)
    most = []
    requests = 0
    for body in sent:
        found = []
        messages = json.loads(body).get("messages", [])
        latest = next((m for m in reversed(messages) if m.get("role") == "user"), None)
        if latest is not None:
            requests += 1
            visit(latest.get("content"))
            if len(found) > len(most):
                most = found
    if requests == 0:
        raise RuntimeError("no request carrying the latest user message reached the API")
    return sorted(most)


def scenario(out, name, confined, restricted, rewrite):
    w, api, sid, cases = World(), API(), str(uuid.uuid4()), []
    target = os.path.join(out, name)
    os.makedirs(target)
    try:
        for phase in ("start", "resume"):
            args = ["claude", "--settings", w.settings, "--setting-sources", "user", "--model", "haiku",
                    "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--permission-mode", "default"]
            if confined:
                args += ["--tools", "Read,Glob,Grep"]
            if restricted:
                args += ["--restricted"]
            args += ["--resume", sid] if phase == "resume" else ["--session-id", sid]
            s = Session(target, phase, args, w.cwd, environ(w, api, confined))
            try:
                s.wait("SessionStart", timeout=60)
                time.sleep(4)
                for case, template in TEXTS:
                    text = template.format(cwd=w.cwd, out=w.out, p=phase)
                    paste = rewritten(text, confined) if rewrite else text
                    prompts, stops, sent = len(s.calls("UserPromptSubmit")), len(s.calls("Stop")), len(api.sent)
                    s.pasted(paste)
                    s.wait("Stop", stops + 1, timeout=60)
                    got = s.calls("UserPromptSubmit")
                    if len(got) != prompts + 1:
                        raise RuntimeError("%s: expected one prompt" % case)
                    try:
                        found = images(api.sent[sent:], w.images)
                    except RuntimeError as e:
                        raise RuntimeError("%s %s %s: %s" % (name, phase, case, e)) from e
                    cases.append({"session": phase, "case": case, "text": text, "pasted": paste,
                                  "prompt": s.payload(got[-1])["prompt"], "images": found})
                    print(name, phase, case, found, file=sys.stderr, flush=True)
            finally:
                s.exit()
    finally:
        api.server.shutdown()
        os.makedirs(os.path.join(target, "debug"))
        with open(os.path.join(target, "debug", "api-requests.json"), "w") as f:
            json.dump([json.loads(body) for body in api.sent], f, ensure_ascii=False)
        shutil.rmtree(w.top)
    with open(os.path.join(target, "outcome.json"), "w") as f:
        json.dump(cases, f, ensure_ascii=False, indent=1)
        f.write("\n")


SCENARIOS = {
    "image_paths_ordinary": (False, False, False),
    "image_paths_confined": (True, True, False),
    "image_paths_ordinary_pasted": (False, False, True),
    "image_paths_confined_pasted": (True, True, True),
    "image_paths_unrestricted_pasted": (True, False, True),
}


def shell_scenario(out, name, rewrite):
    """An opening ! does not exempt later lines from image recognition."""
    target, cases = os.path.join(out, name), []
    os.makedirs(target)
    texts = [("single", "!printf '%s\\n' {p}-relative.png"),
             ("multiline", "!printf '%s\\n' \\\n{cwd}/{p}-line.png")]
    for case, template in texts:
        w, api, sid = World(), API(), str(uuid.uuid4())
        try:
            for phase in ("start", "resume"):
                args = ["claude", "--settings", w.settings, "--setting-sources", "user", "--model", "haiku",
                        "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--permission-mode", "default"]
                args += ["--resume", sid] if phase == "resume" else ["--session-id", sid]
                s = Session(os.path.join(target, case), phase, args, w.cwd, environ(w, api, False))
                try:
                    s.wait("SessionStart", timeout=60)
                    time.sleep(4)
                    text = template.format(cwd=w.cwd, p=phase)
                    paste = rewritten(text) if rewrite else text
                    sent, prompts = len(api.sent), len(s.calls("UserPromptSubmit"))
                    s.pasted(paste)
                    time.sleep(3)
                    command_sent = len(api.sent)
                    command_images = images(api.sent[sent:command_sent], w.images) if command_sent > sent else None
                    command_prompts = [s.payload(n)["prompt"] for n in s.calls("UserPromptSubmit")[prompts:]]
                    after = "Say ok after %s %s" % (phase, case)
                    stops = len(s.calls("Stop"))
                    s.pasted(after)
                    s.wait("Stop", stops + 1, timeout=60)
                    got = s.calls("UserPromptSubmit")
                    if len(got) <= prompts:
                        raise RuntimeError("no prompt for the follow-up input")
                    found = images(api.sent[command_sent:], w.images)
                    cases.append({"session": phase, "case": case, "text": text, "pasted": paste,
                                  "after": after, "prompt": s.payload(got[-1])["prompt"],
                                  "command_requests": command_sent - sent, "command_images": command_images,
                                  "command_prompts": command_prompts, "images": found})
                    print(name, phase, case, found, file=sys.stderr, flush=True)
                finally:
                    s.exit()
        finally:
            api.server.shutdown()
            shutil.rmtree(w.top)
    with open(os.path.join(target, "outcome.json"), "w") as f:
        json.dump(cases, f, ensure_ascii=False, indent=1)
        f.write("\n")


def main():
    out = os.path.abspath(sys.argv[1])
    os.makedirs(out, exist_ok=True)
    # The version probe must use no user configuration either.
    with tempfile.TemporaryDirectory(prefix="capture-config-") as config:
        env = {k: os.environ[k] for k in ("PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "SHELL", "TMPDIR") if k in os.environ}
        env.update({"HOME": config, "CLAUDE_CONFIG_DIR": config, "BROWSER": "true",
                    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS": "1",
                    "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1", "ENABLE_CLAUDEAI_MCP_SERVERS": "false"})
        version = subprocess.run(["claude", "--version"], env=env, cwd=config,
                                 capture_output=True, text=True, check=True).stdout.strip()
    with open(os.path.join(out, "VERSION"), "w") as f:
        f.write("%s\n%s\n" % (version, time.strftime("%Y-%m-%d")))
    for name, how in SCENARIOS.items():
        if not sys.argv[2:] or name in sys.argv[2:]:
            scenario(out, name, *how)
    for name, rewrite in (("image_shell_ordinary", False), ("image_shell_pasted", True)):
        if not sys.argv[2:] or name in sys.argv[2:]:
            shell_scenario(out, name, rewrite)


if __name__ == "__main__":
    main()
