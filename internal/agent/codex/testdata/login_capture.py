#!/usr/bin/env python3
"""Capture a device login prompt without completing the login."""
import hashlib
import json
import os
import pwd
import queue
import re
import signal
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlsplit, urlunsplit

sys.dont_write_bytecode = True
from confine import clean_env, own_config_dir

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "claudecode", "testdata"))
from scrub import Scrubber

ANSI = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-_])")
CODE = re.compile(r"[A-Z0-9]{4,}-[A-Z0-9]{4,}")
END = ("not reached: stopped once the link and the code had printed. From Codex's source "
       "(cli/src/login.rs, finish_login): success prints 'Successfully logged in' on standard error "
       "and exits 0; a failure prints 'Error logging in: <error>' on standard error and exits 1, "
       "for example 'device auth timed out after 15 minutes' (login/src/device_code_auth.rs).")


def user_config_dirs():
    return {os.path.realpath(os.path.join(home, ".codex")) for home in
            (os.path.expanduser("~"), pwd.getpwuid(os.getuid()).pw_dir)}


def overlaps(home, user):
    common = os.path.commonpath((home, user))
    return common == home or common == user


def hashes():
    result = {}
    for directory in sorted(user_config_dirs()):
        for name in ("config.toml", "auth.json"):
            path = os.path.join(directory, name)
            try:
                with open(path, "rb") as f:
                    result[path] = hashlib.sha256(f.read()).digest()
            except FileNotFoundError:
                result[path] = None
    return result


def code_placeholder(value):
    return "".join("X" if c.isalpha() else "0" if c.isdigit() else c for c in value)


def replace_visible(line, replacement):
    """Replace visible characters, retaining escape sequences in their positions."""
    escapes = list(ANSI.finditer(line))
    result, pos, visible = [], 0, 0
    for match in escapes:
        count = len(line[pos:match.start()])
        result.append(replacement[visible:visible + count])
        result.append(match.group())
        visible += count
        pos = match.end()
    result.append(replacement[visible:])
    return "".join(result)


def stop_group(p):
    try:
        os.killpg(p.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        p.wait(timeout=5)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        p.wait()
    # The leader can exit before its children.
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: login_capture.py OUT_DIR")
    home = own_config_dir()
    if home is None or any(overlaps(home, user) for user in user_config_dirs()):
        print("login_capture.py: CODEX_HOME must be a separate directory", file=sys.stderr)
        return 2
    if os.path.lexists(os.path.join(home, "auth.json")):
        print("login_capture.py: CODEX_HOME already holds auth.json", file=sys.stderr)
        return 2
    out = os.path.abspath(sys.argv[1])
    if os.path.lexists(out):
        print("login_capture.py: OUT_DIR already exists", file=sys.stderr)
        return 2

    before = hashes()
    env = dict(clean_env(), CODEX_HOME=home)
    argv = ["codex", "login", "--device-auth"]
    version = subprocess.run(["codex", "--version"], env=env, stdin=subprocess.DEVNULL,
                             capture_output=True, text=True, errors="replace").stdout.strip()
    records = []
    q = queue.Queue()
    link = code = None
    link_next = code_next = False
    start = time.monotonic()
    with tempfile.TemporaryDirectory(prefix="capture-") as work:
        p = subprocess.Popen(argv, env=env, cwd=work, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             start_new_session=True, text=True, encoding="utf-8", errors="replace")

        def read(stream, name):
            for line in stream:
                q.put((time.monotonic() - start, name, line.removesuffix("\n").removesuffix("\r")))
            q.put(None)

        threads = [threading.Thread(target=read, args=(p.stdout, "stdout")),
                   threading.Thread(target=read, args=(p.stderr, "stderr"))]
        for thread in threads:
            thread.start()
        ended = 0
        stopped_by = "timeout"
        while time.monotonic() - start < 60 and ended < 2:
            try:
                item = q.get(timeout=max(0.001, 60 - (time.monotonic() - start)))
            except queue.Empty:
                break
            if item is None:
                ended += 1
                continue
            records.append(item)
            plain = ANSI.sub("", item[2]).strip()
            if link_next and plain.startswith("https://"):
                link = plain
                link_next = False
            if code_next and plain:
                code = plain
                code_next = False
            if plain.startswith("1. Open this link in your browser and sign in to your account"):
                link_next = True
            if plain.startswith("2. Enter this one-time code"):
                code_next = True
            if link and code:
                stopped_by = "link and code printed"
                break
        stopped_after = time.monotonic() - start
        stop_group(p)
        for thread in threads:
            thread.join(timeout=5)
        while not q.empty():
            item = q.get_nowait()
            if item is not None:
                records.append(item)
        try:
            os.killpg(p.pid, 0)
            left = True
        except ProcessLookupError:
            left = False
    if hashes() != before:
        print("login_capture.py: user's config.toml or auth.json changed", file=sys.stderr)
        return 2

    # Include lines delivered while the process group was stopping in the
    # sensitive-value scan before any output is written.
    link_next = code_next = False
    for _, _, line in sorted(records, key=lambda r: r[0]):
        plain = ANSI.sub("", line).strip()
        if link_next and plain.startswith("https://"):
            link, link_next = plain, False
        if code_next and plain:
            code, code_next = plain, False
        if plain.startswith("1. Open this link in your browser and sign in to your account"):
            link_next = True
        if plain.startswith("2. Enter this one-time code"):
            code_next = True
    if not link or not code:
        print("login_capture.py: link and code were not both found; no capture written", file=sys.stderr)
        return 2
    parts = urlsplit(link) if link else None
    link_shape = urlunsplit((parts.scheme, parts.netloc, parts.path, "REDACTED", "")) if parts and (parts.query or parts.fragment) else link
    code_shape = code_placeholder(code)
    scrubber = Scrubber()
    def scrub(value):
        return scrubber.bytes(value.encode()).decode("utf-8", errors="replace")
    clean = []
    for t, stream, line in sorted(records, key=lambda r: r[0]):
        visible = ANSI.sub("", line)
        if link and link in visible:
            line = replace_visible(line, visible.replace(link, link_shape))
        if code and code in ANSI.sub("", line):
            visible = ANSI.sub("", line)
            line = replace_visible(line, visible.replace(code, code_shape))
        visible = ANSI.sub("", line)
        line = replace_visible(line, CODE.sub(lambda m: code_placeholder(m.group()), visible))
        clean.append({"t": t, "stream": stream, "line": scrub(line)})
    notes = {"argv": argv, "version": scrub(version), "stopped_after": stopped_after,
             "stopped_by": stopped_by, "exit_code": p.returncode, "left_after_end": left,
             "link_shape": scrub(link_shape) if link_shape else None,
             "code_shape": scrub(code_shape) if code_shape else None, "end": END}
    os.makedirs(out)
    with open(os.path.join(out, "output.jsonl"), "w") as f:
        for item in clean:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")
    with open(os.path.join(out, "notes.json"), "w") as f:
        json.dump(notes, f, indent=1, ensure_ascii=False)
        f.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
