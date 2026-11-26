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
PATH_SEPARATOR = re.compile(r" [\"']?(?:/|[A-Za-z]:\\)")
TEXTS = [
    ("whole_inside", "{cwd}/{p}-whole.png"),
    ("line_inside", "Look at this\n{cwd}/{p}-line.png\nthen answer"),
    ("whole_outside", "{out}/{p}-outside.png"),
    ("line_outside", "Look at this\n{out}/{p}-line-outside.png"),
    ("link_outside", "Look at this\n{cwd}/{p}-link.png"),
    ("link_inside", "Look at this\n{cwd}/{p}-inside-link.png"),
    ("quoted", 'Look at this\n"{cwd}/{p} spaces.PNG"'),
    ("escaped", "Look at this\n{cwd}/{p}\\ escaped.PNG"),
    ("two_paths", "Look at these\n{out}/{p}-one.png {out}/{p}-two.png"),
    ("relative", "Look at this\n{p}-relative.png"),
    ("wake", "A turn ended.\n> {cwd}/{p}-wake.png"),
]


def no_images(text):
    lines = text.split("\n")
    for i, line in enumerate(lines):
        first = line.strip()
        path = first.strip("\"'")
        absolute = path.startswith(("/", "\\")) or len(path) > 2 and path[1] == ":"
        if not absolute and ("/" in path or "\\" in path):
            continue
        next_path = PATH_SEPARATOR.search(first)
        if next_path:
            head = first[:next_path.start()].strip("\"'")
            if head.startswith("/") and "/" not in head[1:]:
                continue
        if not first.startswith((PREFIX, "> ")) and IMAGE_END.search(first.replace("\\", "")):
            lines[i] = PREFIX + line
    return "\n".join(lines)


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
        if messages and messages[-1].get("role") == "user":
            requests += 1
            visit(messages[-1].get("content"))
            if len(found) > len(most):
                most = found
    if requests == 0:
        raise RuntimeError("no request carrying the latest user message reached the API")
    return sorted(most)


def scenario(out, name, confined, restricted, rewrite):
    w, api, sid, cases = World(), API(), str(uuid.uuid4()), []
    try:
        for phase in ("start", "resume"):
            args = ["claude", "--settings", w.settings, "--setting-sources", "user", "--model", "haiku",
                    "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--permission-mode", "default"]
            if confined:
                args += ["--tools", "Read,Glob,Grep"]
            if restricted:
                args += ["--restricted"]
            args += ["--resume", sid] if phase == "resume" else ["--session-id", sid]
            s = Session(os.path.join(w.top, "sessions"), phase, args, w.cwd, environ(w, api, confined))
            try:
                s.wait("SessionStart", timeout=60)
                time.sleep(4)
                for case, template in TEXTS:
                    text = template.format(cwd=w.cwd, out=w.out, p=phase)
                    paste = no_images(written(text) if confined else text) if rewrite else text
                    prompts, stops, sent = len(s.calls("UserPromptSubmit")), len(s.calls("Stop")), len(api.sent)
                    s.pasted(paste)
                    s.wait("Stop", stops + 1, timeout=60)
                    got = s.calls("UserPromptSubmit")
                    if len(got) != prompts + 1:
                        raise RuntimeError("%s: expected one prompt" % case)
                    found = images(api.sent[sent:], w.images)
                    cases.append({"session": phase, "case": case, "text": text, "pasted": paste,
                                  "prompt": s.payload(got[-1])["prompt"], "images": found})
                    print(name, phase, case, found, file=sys.stderr, flush=True)
            finally:
                s.exit()
    finally:
        api.server.shutdown()
        shutil.rmtree(w.top)
    os.makedirs(os.path.join(out, name))
    with open(os.path.join(out, name, "outcome.json"), "w") as f:
        json.dump(cases, f, ensure_ascii=False, indent=1)
        f.write("\n")


SCENARIOS = {
    "image_paths_ordinary": (False, False, False),
    "image_paths_confined": (True, True, False),
    "image_paths_ordinary_pasted": (False, False, True),
    "image_paths_confined_pasted": (True, True, True),
    "image_paths_unrestricted_pasted": (True, False, True),
}


def main():
    out = os.path.abspath(sys.argv[1])
    os.makedirs(out, exist_ok=True)
    version = subprocess.run(["claude", "--version"], capture_output=True, text=True, check=True).stdout.strip()
    with open(os.path.join(out, "VERSION"), "w") as f:
        f.write("%s\n%s\n" % (version, time.strftime("%Y-%m-%d")))
    for name, how in SCENARIOS.items():
        if not sys.argv[2:] or name in sys.argv[2:]:
            scenario(out, name, *how)


if __name__ == "__main__":
    main()
