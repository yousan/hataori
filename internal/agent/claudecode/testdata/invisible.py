#!/usr/bin/env python3
"""Record the CLI's invisible-character handling without a real model (#916).

Usage: python3 -B invisible.py RAW_DIR [ordinary|confined|extra_ordinary|extra_confined]

Uses image_paths.py's fresh HOME/config/dummy directories and mention.py's
loopback API stand-in. No user configuration or credentials are loaded.
Every case records the first Enter, any extra confirmation, and a following
input. Raw terminal snapshots are private until scrub.py is run.
"""

import json
import os
import subprocess
import sys
import tempfile
import time
import uuid

sys.dont_write_bytecode = True
from capture import Session
from image_paths import World, rewritten
from mention import API, environ


CHARS = [
    ("nul", "\x00"), ("bell", "\x07"), ("backspace", "\x08"),
    ("vertical_tab", "\x0b"), ("form_feed", "\x0c"), ("delete", "\x7f"),
    ("next_line", "\x85"), ("soft_hyphen", "\u00ad"),
    ("arabic_mark", "\u061c"), ("zero_width_space", "\u200b"),
    ("non_joiner", "\u200c"), ("joiner", "\u200d"),
    ("left_mark", "\u200e"), ("right_mark", "\u200f"),
    ("left_embedding", "\u202a"), ("right_embedding", "\u202b"),
    ("pop_direction", "\u202c"), ("left_override", "\u202d"),
    ("right_override", "\u202e"), ("word_joiner", "\u2060"),
    ("left_isolate", "\u2066"), ("right_isolate", "\u2067"),
    ("first_isolate", "\u2068"), ("pop_isolate", "\u2069"),
    ("bom", "\ufeff"), ("text_selector", "\ufe0e"),
    ("emoji_selector", "\ufe0f"), ("color_sequence", "\x1b[31m"),
    ("tab", "\t"), ("crlf", "\r\n"),
    ("emoji_joiner", "👩\u200d💻"), ("emoji_heart", "❤\ufe0f"),
]

EXTRA = [
    ("keycap", "1\ufe0f\u20e3"), ("emoji_modifier", "👩🏽\u200d💻"),
    ("regional_flag", "🇯🇵"),
    ("tag_flag", "🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"),
    ("arabic_joiner", "ب\u200dت"), ("cjk_selector", "漢\ufe00"),
    ("hangul_filler", "\u3164"), ("mongolian_selector", "\u180b"),
    ("grapheme_joiner", "\u034f"), ("function_application", "\u2061"),
    ("line_separator", "\u2028"), ("paragraph_separator", "\u2029"),
]


def cases(phase, extra):
    for name, char in EXTRA if extra else CHARS:
        yield name, "Say %s %s a%sb ok" % (phase, name, char)
    if extra:
        return
    # Repeated equivalent prompts establish their actual order.
    for name, char in (("same_plain", ""), ("same_invisible", "\u200b"), ("same_again", "")):
        yield name, "Say same %s%s please" % (phase, char)
    # Ordinary shell mode only: harmless printf, no filesystem/tool access.
    # Confined cases use the existing safe message rewrite.
    for name in ("zero_width_space", "vertical_tab", "emoji_selector", "bom"):
        char = dict(CHARS)[name]
        yield "leading_" + name, char + "!printf 'invisible-%s\\n'" % phase


def snapshot(s, path):
    s.term.flush()
    with open(os.path.join(s.dir, "terminal.raw"), "rb") as f:
        data = f.read()
    with open(path, "wb") as f:
        f.write(data)


def scenario(out, confined, extra=False):
    w, api, sid = World(), API(), str(uuid.uuid4())
    result = []
    try:
        for phase in ("start", "resume"):
            name = ("extra_" if extra else "") + ("confined_" if confined else "ordinary_") + phase
            args = ["claude", "--settings", w.settings, "--setting-sources", "user", "--model", "haiku",
                    "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--permission-mode", "default"]
            if confined:
                args += ["--restricted", "--tools", "Read,Glob,Grep"]
            args += ["--resume", sid] if phase == "resume" else ["--session-id", sid]
            s = Session(out, name, args, w.cwd, environ(w, api, confined))
            try:
                s.wait("SessionStart", timeout=60)
                time.sleep(3)
                for case, text in cases(phase, extra):
                    pasted = rewritten(text, True) if confined else text
                    before = len(s.calls("UserPromptSubmit"))
                    stops = len(s.calls("Stop"))
                    s.term.flush()
                    last_size = os.path.getsize(os.path.join(s.dir, "terminal.raw"))
                    s.pasted(pasted)
                    time.sleep(2)
                    first = [s.payload(n)["prompt"] for n in s.calls("UserPromptSubmit")[before:]]
                    # A shell command has no UserPromptSubmit. The warning
                    # text, not lack of a hook, determines confirmation.
                    s.term.flush()
                    with open(os.path.join(s.dir, "terminal.raw"), "rb") as f:
                        tail = f.read()
                    # Use the current case's terminal delta, not old warnings.
                    warning = b"review and press Enter to send" in tail[last_size:]
                    if case == "right_override":
                        snapshot(s, os.path.join(s.dir, "right_override-first.raw"))
                    if warning:
                        s.write(b"\r")
                        time.sleep(2)
                    prompts = [s.payload(n)["prompt"] for n in s.calls("UserPromptSubmit")[before:]]
                    if prompts:
                        s.wait("Stop", stops + 1, timeout=30)
                    after = "Say after %s %s please" % (phase, case)
                    n, stops = len(s.calls("UserPromptSubmit")), len(s.calls("Stop"))
                    s.pasted(after)
                    s.wait("Stop", stops + 1, timeout=30)
                    follow = [s.payload(h)["prompt"] for h in s.calls("UserPromptSubmit")[n:]]
                    result.append({"session": phase, "case": case, "text": text, "pasted": pasted,
                                   "first_prompts": first, "confirmed": warning, "prompts": prompts,
                                   "reports": [s.payload(h) for h in s.calls("UserPromptSubmit")[before:n]],
                                   "after": after, "following_prompts": follow,
                                   "following_reports": [s.payload(h) for h in s.calls("UserPromptSubmit")[n:]]})
                    print(name, case, "confirm=" + str(warning), repr(prompts), repr(follow), flush=True)
            finally:
                s.exit()
            with open(os.path.join(out, name, "outcome.json"), "w") as f:
                json.dump([c for c in result if c["session"] == phase], f, ensure_ascii=False, indent=1)
                f.write("\n")
    finally:
        api.server.shutdown()
        # Keep private temporary state for inspection; never delete user data.
        print("Private disposable state:", w.top, file=sys.stderr)


def consecutive(out, confined):
    """Baseline: a new paste while the invisible warning holds the editor."""
    w, api, sid = World(), API(), str(uuid.uuid4())
    try:
        for phase in ("start", "resume"):
            name = "consecutive_" + ("confined_" if confined else "ordinary_") + phase
            args = ["claude", "--settings", w.settings, "--setting-sources", "user", "--model", "haiku",
                    "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--permission-mode", "default"]
            if confined:
                args += ["--restricted", "--tools", "Read,Glob,Grep"]
            args += ["--resume", sid] if phase == "resume" else ["--session-id", sid]
            s = Session(out, name, args, w.cwd, environ(w, api, confined))
            try:
                s.wait("SessionStart", timeout=60)
                time.sleep(3)
                text = "Say %s right_override a\u202eb ok" % phase
                pasted = rewritten(text, True) if confined else text
                s.pasted(pasted)
                time.sleep(2)
                first = [s.payload(n) for n in s.calls("UserPromptSubmit")]
                snapshot(s, os.path.join(s.dir, "first.raw"))
                after = "Say following %s please" % phase
                s.pasted(after)
                s.wait("Stop", timeout=30)
                reports = [s.payload(n) for n in s.calls("UserPromptSubmit")]
                with open(os.path.join(s.dir, "outcome.json"), "w") as f:
                    json.dump({"session": phase, "text": text, "pasted": pasted,
                               "first_reports": first, "after": after, "reports": reports}, f,
                              ensure_ascii=False, indent=1)
                    f.write("\n")
                print(name, "first", len(first), "combined", [p["prompt"] for p in reports], flush=True)
            finally:
                s.exit()
    finally:
        api.server.shutdown()
        print("Private disposable state:", w.top, file=sys.stderr)


if __name__ == "__main__":
    out = os.path.abspath(sys.argv[1])
    os.makedirs(out, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="capture-config-") as config:
        env = {k: os.environ[k] for k in ("PATH", "LANG", "LC_ALL") if k in os.environ}
        env.update({"HOME": config, "CLAUDE_CONFIG_DIR": config, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"})
        version = subprocess.run(["claude", "--version"], cwd=config, env=env,
                                 capture_output=True, text=True, check=True).stdout.strip()
    with open(os.path.join(out, "VERSION"), "w") as f:
        f.write(version + "\n" + time.strftime("%Y-%m-%d") + "\n")
    for mode in sys.argv[2:] or ["ordinary", "confined", "extra_ordinary", "extra_confined",
                               "consecutive_ordinary", "consecutive_confined"]:
        if mode in ("consecutive_ordinary", "consecutive_confined"):
            consecutive(out, mode.endswith("confined"))
            continue
        if mode not in ("ordinary", "confined", "extra_ordinary", "extra_confined"):
            raise ValueError("unknown mode: " + mode)
        scenario(out, mode.endswith("confined"), mode.startswith("extra_"))
