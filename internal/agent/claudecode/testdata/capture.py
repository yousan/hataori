#!/usr/bin/env python3
"""Capture real Claude Code hook calls, transcripts and terminal output.

usage: capture.py RAW_DIR [SCENARIO ...]

Runs the installed `claude` interactively on a pseudo-terminal, one session
per scenario, each in a new empty directory under the temporary directory.
Hooks are registered with --settings only; nothing is written to the
user's settings or to the session directories. Each hook call's payload is
stored byte for byte. The output in RAW_DIR is not scrubbed: run scrub.py
on it, and never commit RAW_DIR.

The login scenario runs `claude auth login` with an empty, throwaway
CLAUDE_CONFIG_DIR and answers it with a wrong code only.
"""

import json
import os
import pty
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import uuid

MODEL = "haiku"  # the cheapest model that produces every payload
ROWS, COLS = 50, 160

# The recorder every hook runs. It stores the payload unchanged, and how
# many complete lines the session's transcript held at the call, and answers
# the two hooks that decide something: a permission request is allowed, and
# a question gets the first option of each of its questions.
HOOK = r'''
import json, os, sys, time
raw = sys.stdin.buffer.read()
ev = json.loads(raw).get("hook_event_name", "Unknown")
d = os.environ["CAPTURE_HOOK_DIR"]
name = "%d-%s.json" % (time.time_ns(), ev)
p = json.loads(raw)
# How many complete lines the transcript holds when the hook is called:
# what a reader of both could have read of it by then.
lines = -1
try:
    with open(p.get("transcript_path") or "", "rb") as f:
        lines = f.read().count(b"\n")
except OSError:
    pass
with open(os.path.join(d, name[:-len(".json")] + ".lines"), "w") as f:
    f.write("%d\n" % lines)
with open(os.path.join(d, name + ".tmp"), "wb") as f:
    f.write(raw)
os.rename(os.path.join(d, name + ".tmp"), os.path.join(d, name))
if ev == "PermissionRequest":
    print(json.dumps({"hookSpecificOutput": {"hookEventName": "PermissionRequest",
                                             "decision": {"behavior": "allow"}}}))
elif ev == "PreToolUse" and p.get("tool_name") == "AskUserQuestion":
    inp = dict(p.get("tool_input") or {})
    inp["answers"] = {q["question"]: q["options"][0]["label"] for q in inp.get("questions", [])}
    print(json.dumps({"hookSpecificOutput": {"hookEventName": "PreToolUse",
                                             "permissionDecision": "allow",
                                             "updatedInput": inp}}))
'''

EVENTS = ["SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure",
          "PermissionRequest", "Notification", "SubagentStart", "SubagentStop", "Stop",
          "StopFailure", "PreCompact", "PostCompact", "SessionEnd"]


def clean_env(extra=None):
    """The environment of a session: only what a login shell has, never
    variables of a Claude Code session that runs this script."""
    keep = ["PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "SHELL", "TMPDIR"]
    env = {k: os.environ[k] for k in keep if k in os.environ}
    env["TERM"] = "xterm-256color"
    env["BROWSER"] = "true"  # never open a browser
    # Keep the user's own context out of the captures: no CLAUDE.md, no
    # auto-memory, no claude.ai connectors.
    env["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] = "1"
    env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
    env["ENABLE_CLAUDEAI_MCP_SERVERS"] = "false"
    env.update(extra or {})
    return env


class Session:
    """One interactive claude on a pseudo-terminal."""

    def __init__(self, out, name, args, cwd, env):
        self.dir = os.path.join(out, name)
        self.hooks = os.path.join(self.dir, "hooks")
        os.makedirs(self.hooks)
        self.term = open(os.path.join(self.dir, "terminal.raw"), "wb")
        env = dict(env, CAPTURE_HOOK_DIR=self.hooks)
        pid, fd = pty.fork()
        if pid == 0:
            os.chdir(cwd)
            os.execvpe(args[0], args, env)
        self.pid, self.fd = pid, fd
        import fcntl, struct, termios
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
        self.alive = True
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        while True:
            try:
                r, _, _ = select.select([self.fd], [], [], 0.5)
                if r:
                    b = os.read(self.fd, 65536)
                    if not b:
                        break
                    self.term.write(b)
                    self.term.flush()
            except OSError:
                break
        self.alive = False

    def calls(self, event=None):
        names = sorted(n for n in os.listdir(self.hooks) if n.endswith(".json"))
        return [n for n in names if event is None or n.split("-", 1)[1] == event + ".json"]

    def payload(self, name):
        with open(os.path.join(self.hooks, name), "rb") as f:
            return json.loads(f.read())

    def wait(self, event, count=1, timeout=120, where=None):
        deadline = time.time() + timeout
        while time.time() < deadline:
            got = [n for n in self.calls(event) if where is None or where(self.payload(n))]
            if len(got) >= count:
                return got
            if not self.alive:
                raise RuntimeError("claude exited while waiting for %s" % event)
            time.sleep(0.2)
        raise TimeoutError("no %s within %ds" % (event, timeout))

    def write(self, b):
        os.write(self.fd, b)

    def typed(self, text):
        for ch in text:
            self.write(ch.encode())
            time.sleep(0.02)
        time.sleep(0.3)
        self.write(b"\r")

    def pasted(self, text):
        self.write(b"\x1b[200~" + text.encode() + b"\x1b[201~")
        time.sleep(1.0)
        self.write(b"\r")

    def turn(self, text, paste=False, stops=1, timeout=180):
        n = len(self.calls("Stop"))
        (self.pasted if paste else self.typed)(text)
        return self.wait("Stop", n + stops, timeout)

    def transcript(self):
        for n in self.calls("SessionStart"):
            return self.payload(n).get("transcript_path")

    def save_transcripts(self):
        """Copy the session's transcript and any subagent transcript."""
        paths = set()
        for n in self.calls():
            p = self.payload(n)
            for k in ("transcript_path", "agent_transcript_path"):
                if p.get(k):
                    paths.add(p[k])
        started = [self.payload(n).get("transcript_path") for n in self.calls("SessionStart")]
        for p in sorted(paths):
            if os.path.exists(p):
                if p == self.transcript():
                    name = "transcript.jsonl"
                elif p in started:  # a new session inside this one, as after /clear
                    name = "transcript-%d.jsonl" % (started.index(p) + 1)
                else:
                    name = "subagent-" + os.path.basename(p)
                shutil.copyfile(p, os.path.join(self.dir, name))

    def exit(self):
        if self.alive:
            n = len(self.calls("SessionEnd"))
            self.typed("/exit")
            try:
                self.wait("SessionEnd", n + 1, timeout=30)
            except (TimeoutError, RuntimeError):
                pass
        self.close()

    def close(self):
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                if os.waitpid(self.pid, os.WNOHANG)[0]:
                    break
            except ChildProcessError:  # already reaped, by stop_group
                break
            time.sleep(0.2)
        else:
            os.kill(self.pid, signal.SIGKILL)
            os.waitpid(self.pid, 0)
        self.reader.join(2)
        self.term.close()
        self.save_transcripts()


def user_plugins():
    """The plugins the user's settings enable, read at run time, so that the
    capture can switch them off."""
    try:
        with open(os.path.expanduser("~/.claude/settings.json")) as f:
            return list((json.load(f).get("enabledPlugins") or {}).keys())
    except (OSError, ValueError):
        return []


def settings_file(work):
    hook = os.path.join(work, "hook.py")
    with open(hook, "w") as f:
        f.write(HOOK)
    cmd = "%s %s" % (sys.executable, hook)
    s = {
        "hooks": {ev: [{"hooks": [{"type": "command", "command": cmd}]}] for ev in EVENTS},
        # Keep the capture to itself.
        "remoteControlAtStartup": False,
        "enabledPlugins": {name: False for name in user_plugins()},
    }
    path = os.path.join(work, "settings.json")
    with open(path, "w") as f:
        json.dump(s, f)
    return path


# The user's settings (their hooks, plugins, permissions) are not loaded.
SOURCES = ["--setting-sources", "project,local"]


def claude(settings, *extra):
    return ["claude", "--settings", settings, *SOURCES, "--model", MODEL, "--permission-mode", "default", *extra]


def new_dir(work, name):
    d = os.path.join(work, "sessions", name)
    os.makedirs(d)
    return d


def run(out, work, name, steps, args=None, cwd=None):
    """Start a session, run steps(session), and end it whatever happens."""
    settings = settings_file(work)
    sid = str(uuid.uuid4())
    cwd = cwd or new_dir(work, name)
    s = Session(out, name, args or claude(settings, "--session-id", sid), cwd, clean_env())
    try:
        s.wait("SessionStart", timeout=60)
        steps(s)
    finally:
        s.exit()
    return sid, cwd


# Scenarios. Each is small: one or two tiny turns.

def plain(out, work):
    return run(out, work, "plain", lambda s: s.turn("Reply with just the word: ok"))


def resume(out, work, sid, cwd):
    settings = settings_file(work)
    run(out, work, "resume", lambda s: s.turn("Reply with just the word: again"),
        args=claude(settings, "--resume", sid), cwd=cwd)


def tools(out, work):
    run(out, work, "tools", lambda s: s.turn(
        "Use the Write tool to create a file named a.txt containing the word hi. Then reply with just: done"))


def question(out, work):
    run(out, work, "question", lambda s: s.turn(
        "Use the AskUserQuestion tool once to ask me: 'Which colour?' with the two options red and blue. "
        "Then reply with just the colour I chose."))


def background(out, work):
    def steps(s):
        # Stop with the task still running, then the task notification and
        # the turn the agent starts by itself.
        s.turn("Run the shell command `sleep 8` with the Bash tool, with run_in_background set to true. "
               "Do not wait for it. Reply with just: started")
        n = len(s.calls("Stop"))
        s.wait("Stop", n + 1, timeout=120)
    run(out, work, "background", steps)


def held_notification(out, work):
    def steps(s):
        # A paste that is in the input box when the task notification
        # starts a turn of the agent's own; Enter after that turn's end.
        s.turn("Run the shell command `sleep 8` with the Bash tool, with run_in_background set to true. "
               "Do not wait for it. Reply with just: started. Later, when you are told that it finished, "
               "run the shell command `python3 -c 'import time; time.sleep(8)'` in the foreground and then "
               "reply with just: finished")
        time.sleep(3)
        s.write(b"\x1b[200~Reply with just: held message\x1b[201~")
        s.wait("Stop", 2)
        time.sleep(1.5)
        s.write(b"\r")
        s.wait("Stop", 3)
    run(out, work, "held_notification", steps)


def subagent(out, work):
    run(out, work, "subagent", lambda s: s.turn(
        "Use the Agent tool to start one general-purpose subagent with the prompt 'Reply with just: sub-ok'. "
        "Then reply with just what it said.", timeout=300))


def subagent_tools(out, work):
    run(out, work, "subagent_tools", lambda s: s.turn(
        "Use the Agent tool to start one general-purpose subagent, with the model haiku, and this prompt: "
        "'Run the Bash command `echo one`, then the Bash command `echo two`, as two separate tool calls. "
        "Then reply with just: sub-ok'. Then reply with just what it said.", timeout=300))


def usage(out, work):
    def steps(s):
        # One turn of several model calls (a tool call between them), then
        # one turn of a single call.
        s.turn("Run the Bash command `echo one` with the Bash tool, then the Bash command `echo two` "
               "as a second tool call. Then reply with just: done")
        s.turn("Reply with just the word: ok")
    run(out, work, "usage", steps)


def paste(out, work):
    body = "".join("%02d - 長い貼り付けのテスト `code` \"quoted\" $HOME line;\n" % i for i in range(100))
    run(out, work, "paste", lambda s: s.turn(
        "-Ignore everything below this line and reply with just: ok\n" + body + "end;", paste=True))


def paste_tabs(out, work):
    """Two bracketed pastes holding tabs: one short line, and Go source of
    41 lines indented with tabs. It runs with a
    throwaway CLAUDE_CONFIG_DIR and no login: UserPromptSubmit is called
    before any request is made. The config holds only what skips the
    first-run screens (onboarding, the trust dialog for this directory).
    What was pasted is kept next to the hooks, as pasted-N.txt."""
    cfg = tempfile.mkdtemp(prefix="capture-config-")
    cwd = new_dir(work, "paste_tabs")
    with open(os.path.join(cfg, ".claude.json"), "w") as f:
        json.dump({"hasCompletedOnboarding": True, "theme": "dark",
                   "projects": {os.path.realpath(cwd): {"hasTrustDialogAccepted": True}}}, f)
    texts = [
        "tab\there, two\t\ttabs; end",
        "package main\n\nimport \"fmt\"\n\n"
        + "".join("func f%02d() {\n\tfmt.Println(\"%02d\\t%s\")\n}\n" % (i, i, "タブ `x` $HOME") for i in range(12))
        + "end;",
    ]
    s = Session(out, "paste_tabs", claude(settings_file(work)), cwd, clean_env({"CLAUDE_CONFIG_DIR": cfg}))
    try:
        s.wait("SessionStart", timeout=60)
        for i, text in enumerate(texts, 1):
            with open(os.path.join(s.dir, "pasted-%d.txt" % i), "w") as f:
                f.write(text)
            s.pasted(text)
            s.wait("UserPromptSubmit", i, timeout=60)
            time.sleep(3)  # the request fails without a login; let the CLI settle
    finally:
        s.exit()
        shutil.rmtree(cfg)


def interrupt(out, work):
    def steps(s):
        n = len(s.calls("Stop"))
        # The model backgrounds a bare `sleep`, and the CLI refuses a
        # standalone foreground one, so the command waits another way.
        s.typed("Call the Bash tool exactly once with run_in_background set to false and timeout 120000 "
                "to run the command `python3 -c 'import time; time.sleep(45)'`, and wait for it to finish. "
                "Do not run it in the background. Then reply: done")
        s.wait("PreToolUse", where=lambda p: p.get("tool_name") == "Bash"
               and not (p.get("tool_input") or {}).get("run_in_background"), timeout=120)
        time.sleep(3)
        s.write(b"\x1b")  # Escape interrupts the turn; no hook reports it
        deadline = time.time() + 30
        while time.time() < deadline:
            t = s.transcript()
            if t and os.path.exists(t) and b"Request interrupted" in open(t, "rb").read():
                break
            time.sleep(0.3)
        else:
            raise TimeoutError("no interrupt line in the transcript")
        if len(s.calls("Stop")) != n:
            print("note: a Stop followed the interrupt", file=sys.stderr)
    run(out, work, "interrupt", steps)


def compact(out, work):
    def steps(s):
        s.turn("Reply with just the word: ok")
        n = len(s.calls("PostCompact"))
        s.typed("/compact")
        s.wait("PostCompact", n + 1, timeout=300)
    run(out, work, "compact", steps)


def failure(out, work):
    # An API error without spending anything: a model that does not exist.
    settings = settings_file(work)
    args = ["claude", "--settings", settings, *SOURCES, "--model", "claude-does-not-exist-0", "--permission-mode", "default",
            "--session-id", str(uuid.uuid4())]
    def steps(s):
        s.typed("Reply with just the word: ok")
        s.wait("StopFailure", timeout=120)
    run(out, work, "failure", steps, args=args)


def resume_missing(out, work):
    """`claude --resume` of a conversation the CLI does not have: a new
    random id in a new directory. No request is sent."""
    settings = settings_file(work)
    s = Session(out, "resume_missing", claude(settings, "--resume", str(uuid.uuid4())),
                new_dir(work, "resume_missing"), clean_env())
    deadline = time.time() + 20
    while s.alive and time.time() < deadline:
        time.sleep(0.2)
    s.close()


def clear(out, work):
    """One turn, /clear at the terminal, and one more turn."""
    def steps(s):
        s.turn("Reply with just the word: ok")
        n = len(s.calls("SessionStart"))
        s.typed("/clear")
        s.wait("SessionStart", n + 1, timeout=60)
        s.turn("Reply with just the word: again")
    run(out, work, "clear", steps)


def stop_group(s, interrupt=1.0, terminate=2.0):
    """End the session's process group as the session host stops a session:
    SIGINT, then SIGTERM after interrupt, then SIGKILL after terminate."""
    for sig, wait in ((signal.SIGINT, interrupt), (signal.SIGTERM, terminate), (signal.SIGKILL, 0)):
        try:
            os.killpg(s.pid, sig)
        except ProcessLookupError:
            return
        deadline = time.time() + wait
        while time.time() < deadline:
            if os.waitpid(s.pid, os.WNOHANG)[0]:
                return
            time.sleep(0.1)


def sleep(out, work):
    """A session stopped as for sleep, then resumed: does it still have its
    conversation? The first turn asks to remember a word, the resumed turn
    asks for it."""
    settings = settings_file(work)
    sid = str(uuid.uuid4())
    cwd = new_dir(work, "sleep")
    s = Session(out, "sleep", claude(settings, "--session-id", sid), cwd, clean_env())
    try:
        s.wait("SessionStart", timeout=60)
        s.turn("Remember the word heron. Reply with just the word: ok")
        time.sleep(1)
        stop_group(s)
    finally:
        s.close()
    s = Session(out, "sleep_resumed", claude(settings, "--resume", sid), cwd, clean_env())
    try:
        s.wait("SessionStart", timeout=60)
        s.turn("Which word did I ask you to remember? Reply with just that word.")
    finally:
        s.exit()


def trust(out, work):
    """The folder-trust dialog of a repository never opened before. It is
    recorded and left without being answered."""
    cwd = new_dir(work, "trust")
    subprocess.run(["git", "init", "-q", cwd], check=True)
    s = Session(out, "trust", claude(settings_file(work)), cwd, clean_env())
    time.sleep(12)  # the dialog stays; no hook runs
    os.kill(s.pid, signal.SIGKILL)
    s.close()


def login(out, work):
    """`claude auth login` on pipes with a throwaway config directory, up to
    its request for a code, then a wrong code."""
    d = os.path.join(out, "login")
    os.makedirs(d)
    cfg = tempfile.mkdtemp(prefix="capture-config-")
    env = clean_env({"CLAUDE_CONFIG_DIR": cfg})
    status = subprocess.run(["claude", "auth", "status"], env=env, capture_output=True, timeout=60)
    with open(os.path.join(d, "status-before.json"), "wb") as f:
        f.write(status.stdout)
    p = subprocess.Popen(["claude", "auth", "login"], env=env, stdin=subprocess.PIPE,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=cfg)
    outs = {"stdout": bytearray(), "stderr": bytearray()}

    def pump(name, stream):
        while True:
            b = stream.read1(4096)
            if not b:
                break
            outs[name] += b
    ts = [threading.Thread(target=pump, args=(k, getattr(p, k)), daemon=True) for k in outs]
    for t in ts:
        t.start()
    try:
        deadline = time.time() + 60
        while b"Paste code" not in outs["stdout"] and time.time() < deadline:
            time.sleep(0.2)
        with open(os.path.join(d, "stdout-before-code.txt"), "wb") as f:
            f.write(bytes(outs["stdout"]))
        p.stdin.write(b"not-a-valid-code\n")
        p.stdin.flush()
        deadline = time.time() + 60
        while b"Invalid code" not in outs["stderr"] + outs["stdout"] and time.time() < deadline:
            time.sleep(0.2)
        time.sleep(1)
    finally:
        p.kill()
        p.wait()
        for t in ts:
            t.join(2)
    with open(os.path.join(d, "stdout.txt"), "wb") as f:
        f.write(bytes(outs["stdout"]))
    with open(os.path.join(d, "stderr.txt"), "wb") as f:
        f.write(bytes(outs["stderr"]))
    shutil.rmtree(cfg)  # the throwaway directory, created above


def login_terminal(out, work):
    """`claude auth login` on a terminal, with a throwaway CLAUDE_CONFIG_DIR:
    the link (wrapped in a terminal hyperlink), then a wrong code."""
    cfg = tempfile.mkdtemp(prefix="capture-config-")
    s = Session(out, "login_terminal", ["claude", "auth", "login"], cfg, clean_env({"CLAUDE_CONFIG_DIR": cfg}))
    try:
        def seen(b, timeout=60):
            deadline = time.time() + timeout
            while time.time() < deadline:
                if b in open(s.term.name, "rb").read():
                    return True
                time.sleep(0.2)
            return False
        if seen(b"Paste code"):
            s.typed("not-a-valid-code")
            seen(b"Invalid code")
            time.sleep(1)
    finally:
        os.kill(s.pid, signal.SIGKILL)
        s.close()
        shutil.rmtree(cfg)


def main():
    out = os.path.abspath(sys.argv[1])
    want = set(sys.argv[2:])
    os.makedirs(out, exist_ok=True)
    work = tempfile.mkdtemp(prefix="capture-")
    ver = subprocess.run(["claude", "--version"], capture_output=True, text=True).stdout.strip()
    with open(os.path.join(out, "VERSION"), "w") as f:
        f.write("%s\n%s\n" % (ver, time.strftime("%Y-%m-%d")))
    order = ["plain", "tools", "question", "background", "subagent", "paste", "interrupt", "compact",
             "failure", "trust", "login", "login_terminal", "resume_missing", "clear", "sleep", "subagent_tools", "usage", "paste_tabs",
             "held_notification"]
    for name in order:
        if want and name not in want and not (name == "plain" and "resume" in want):
            continue
        print("scenario", name, file=sys.stderr)
        try:
            if name == "plain":
                sid, cwd = plain(out, work)
                if not want or "resume" in want:
                    print("scenario resume", file=sys.stderr)
                    resume(out, work, sid, cwd)
            else:
                globals()[name](out, work)
        except Exception as e:  # keep going; the README lists what is missing
            print("scenario %s failed: %s" % (name, e), file=sys.stderr)
    print("raw captures in", out, file=sys.stderr)


if __name__ == "__main__":
    main()
