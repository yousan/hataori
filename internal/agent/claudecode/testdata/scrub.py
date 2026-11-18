#!/usr/bin/env python3
"""Turn raw captures into committable testdata.

usage: scrub.py RAW_DIR OUT_DIR
       scrub.py --again DIR

OUT_DIR must not exist yet. --again rewrites the files of an already
scrubbed DIR in place with the rules that are safe to run twice (today the
interpreter path); the others would renumber what they replaced before.

Every value that identifies this machine or account is read at run time,
never written here: the home directory, the user name, the host name and
the e-mail addresses found in the captures. The same value is replaced the
same way in every file:

  home directory     -> /home/user        (also in Claude Code's dashed form)
  user name          -> user
  host name          -> host
  TMPDIR, if not /tmp -> /tmp          (also in Claude Code's dashed form)
  capture temp dirs  -> capture-work, capture-config
  UUIDs              -> 00000000-0000-4000-8000-00000000000N, numbered in order of appearance
  e-mail addresses   -> userN@example.com
  an interpreter under the home directory -> /usr/bin/python3
  sk-ant-... tokens, signatures, OAuth link parameters -> REDACTED

Hook payloads are renamed NN-<event>.json in the order they were called.
Only the trust, login_terminal and resume_missing scenarios keep their terminal output. In transcripts, lines
that carry the session's context (attachments, recorded API requests) keep
their shape with every string blanked.
"""

import getpass
import json
import os
import re
import shutil
import socket
import sys
import tempfile

UUID = re.compile(rb"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
EMAIL = re.compile(rb"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
# mkdtemp's eight random characters; Claude Code writes '_' as '-' in its
# dashed form of a path.
TEMP = re.compile(rb"capture-(config-)?[A-Za-z0-9_-]{8}(?![A-Za-z0-9_])")
TOKEN = re.compile(rb"sk-ant-[A-Za-z0-9_-]+")
SIGNATURE = re.compile(rb'("signature"\s*:\s*")[^"]*(")')
HOME_PYTHON = re.compile(rb"/home/user/[^\s\"']*/bin/(python3?)(?![A-Za-z0-9.])")
OAUTH = re.compile(rb"([?&](?:code_challenge|state|code|client_id)=)[^&\s\"'\x1b\x07]+")


class Scrubber:
    def __init__(self):
        home = os.path.expanduser("~").encode()
        user = getpass.getuser().encode()
        host = socket.gethostname().encode()
        dashed = home.replace(b"/", b"-")
        # Longest first, so a path is replaced before the name inside it.
        self.fixed = sorted([
            (home, b"/home/user"),
            (dashed, b"-home-user"),
            (host, b"host"),
            (host.split(b".")[0], b"host"),
            (user, b"user"),
        ], key=lambda kv: -len(kv[0]))
        # The temporary directory the capture ran in, when it is not /tmp
        # (TMPDIR set to a directory of its own): its path can name the
        # machine's directories, and Claude Code's dashed form of it too.
        tmp = os.path.realpath(tempfile.gettempdir()).encode()
        self.temp = []
        if tmp != b"/tmp":
            self.temp = [(tmp, b"/tmp"), (re.sub(rb"[^A-Za-z0-9]", b"-", tmp), b"-tmp")]
        self.uuids = {}
        self.emails = {}

    def _uuid(self, m):
        v = m.group(0)
        if v not in self.uuids:
            self.uuids[v] = b"00000000-0000-4000-8000-%012d" % (len(self.uuids) + 1)
        return self.uuids[v]

    def _email(self, m):
        v = m.group(0)
        if v.endswith(b"@example.com"):
            return v
        if v not in self.emails:
            self.emails[v] = b"user%d@example.com" % (len(self.emails) + 1)
        return self.emails[v]

    def bytes(self, b):
        for old, new in self.temp:  # before the UUIDs inside it are renumbered
            b = b.replace(old, new)
        b = TEMP.sub(lambda m: b"capture-config" if m.group(1) else b"capture-work", b)
        b = UUID.sub(self._uuid, b)
        b = EMAIL.sub(self._email, b)
        b = TOKEN.sub(b"sk-ant-REDACTED", b)
        b = SIGNATURE.sub(rb"\1REDACTED\2", b)
        b = OAUTH.sub(rb"\1REDACTED", b)
        for old, new in self.fixed:
            if len(old) >= 3:  # a name this short would hit ordinary words
                b = re.sub(rb"(?<![A-Za-z0-9])" + re.escape(old) + rb"(?![A-Za-z0-9])", new, b)
        # The interpreter the hook command runs, when it lives in the home
        # directory (a virtual environment): which one says nothing about
        # the CLI, and its path names the machine's own directories.
        b = HOME_PYTHON.sub(rb"/usr/bin/\1", b)
        return b


# Transcript lines that carry the session's context rather than its
# conversation: attachments (instructions, environment, account, tool and
# skill listings, prompt snapshots) and recorded API requests. Their shape is
# kept; every string in them is blanked.
CONTEXT_TYPES = {"attachment", "api-request", "api-request-blob", "api-request-shape"}
KEEP_KEYS = {"type", "uuid", "parentUuid", "timestamp", "sessionId", "version", "isSidechain", "userType"}


def blank(v, key=None):
    if isinstance(v, dict):
        return {k: (x if k in KEEP_KEYS else blank(x, k)) for k, x in v.items()}
    if isinstance(v, list):
        return [blank(x) for x in v]
    if isinstance(v, str):
        return "REDACTED"
    return v


def transcript(data):
    out = []
    for line in data.split(b"\n"):
        try:
            d = json.loads(line)
        except ValueError:
            out.append(line)
            continue
        if isinstance(d, dict) and d.get("type") in CONTEXT_TYPES:
            if isinstance(d.get("attachment"), dict):  # keep which attachment it was
                kind = d["attachment"].get("type")
                d = blank(d)
                d["attachment"]["type"] = kind
            else:
                d = blank(d)
            line = json.dumps(d, ensure_ascii=False, separators=(",", ":")).encode()
        out.append(line)
    return b"\n".join(out)


def again(top):
    for d, _, names in os.walk(top):
        for n in names:
            p = os.path.join(d, n)
            with open(p, "rb") as f:
                data = f.read()
            new = HOME_PYTHON.sub(rb"/usr/bin/\1", data)
            if new != data:
                with open(p, "wb") as f:
                    f.write(new)
                print("rewrote", p, file=sys.stderr)


def main():
    if sys.argv[1] == "--again":
        return again(sys.argv[2])
    raw, out = sys.argv[1], sys.argv[2]
    s = Scrubber()
    if os.path.exists(out):
        # Never write over anything: remove an old capture with git rm.
        sys.exit("scrub.py: %s exists; give a new directory" % out)
    os.makedirs(out)
    shutil.copyfile(os.path.join(raw, "VERSION"), os.path.join(out, "VERSION"))
    for scen in sorted(os.listdir(raw)):
        src = os.path.join(raw, scen)
        if not os.path.isdir(src):
            continue
        dst = os.path.join(out, scen)
        os.makedirs(dst)
        hooks = os.path.join(src, "hooks")
        if os.path.isdir(hooks):
            os.makedirs(os.path.join(dst, "hooks"))
            names = sorted(n for n in os.listdir(hooks) if n.endswith(".json"))
            for i, n in enumerate(names, 1):
                event = n.split("-", 1)[1]
                with open(os.path.join(hooks, n), "rb") as f:
                    data = s.bytes(f.read())
                with open(os.path.join(dst, "hooks", "%02d-%s" % (i, event)), "wb") as f:
                    f.write(data)
        for n in sorted(os.listdir(src)):
            p = os.path.join(src, n)
            if n == "hooks" or not os.path.isfile(p):
                continue
            if n == "terminal.raw" and scen not in ("trust", "login_terminal", "resume_missing"):
                continue
            with open(p, "rb") as f:
                data = f.read()
            if n.endswith(".jsonl"):
                data = transcript(data)
            data = s.bytes(data)
            with open(os.path.join(dst, s.bytes(n.encode()).decode()), "wb") as f:
                f.write(data)


if __name__ == "__main__":
    main()
