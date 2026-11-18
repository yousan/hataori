#!/usr/bin/env python3
"""log.py <kind>: append {ts, raw} to <kind>.jsonl next to this script.

Used as a hook command (kind "hooks") and as the status line command (kind "statusline");
as the status line it prints one word so the line is visible."""
import json, os, sys, time
kind = sys.argv[1]
raw = json.loads(sys.stdin.read())
with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), kind + ".jsonl"), "a") as f:
    f.write(json.dumps({"ts": time.time(), "raw": raw}) + "\n")
if kind == "statusline":
    print("status-logged")
