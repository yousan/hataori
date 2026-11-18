#!/usr/bin/env python3
"""scrub.py <in> <out>: replace values that name an account, a host, a path or an id.

Keys are replaced wherever they appear in JSON; free text is scrubbed with the patterns below.
Rate-limit numbers and reset times are kept: they are what the note is about."""
import json, re, sys
DROP = {"session_id", "sessionId", "transcript_path", "cwd", "scratchpad_dir", "current_dir", "project_dir",
        "codexHome", "serverName", "installationId", "uuid", "email", "accountId", "chatgptAccountId",
        "userAgent", "path", "threadId", "turnId", "rootTurnId", "requestId"}
def walk(v, k=None):
    if isinstance(v, dict): return {a: walk(b, a) for a, b in v.items()}
    if isinstance(v, list): return [walk(x) for x in v]
    if k in DROP and isinstance(v, str): return "<" + k + ">"
    return v
def text(s):
    s = re.sub(r"/(?:home|tmp|Users)/[^\s\"']*", "<path>", s)
    s = re.sub(r"\([A-Za-z]+/[A-Za-z_]+\)", "(<time zone>)", s)
    return s
src = open(sys.argv[1]).read()
try:
    out = json.dumps(walk(json.loads(src)), indent=1)
except ValueError:
    out = "\n".join(json.dumps(walk(json.loads(l))) if l.startswith("{") else l for l in src.splitlines())
open(sys.argv[2], "w").write(text(out).rstrip() + "\n")
