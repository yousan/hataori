#!/usr/bin/env python3
"""make-settings.py <dir>: write <dir>/settings.json, for `claude --settings`, that sends every
hook and the status line input to log.py in <dir>. Copy log.py into <dir> first."""
import json, os, sys
d = os.path.abspath(sys.argv[1])
h = [{"type": "command", "command": f"python3 {d}/log.py hooks"}]
events = ["SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "SessionEnd",
          "PreCompact", "SubagentStop"]
hooks = {e: [{"hooks": h}] for e in events}
hooks["PreToolUse"] = [{"matcher": "*", "hooks": h}]
hooks["PostToolUse"] = [{"matcher": "*", "hooks": h}]
s = {"hooks": hooks,
     "statusLine": {"type": "command", "command": f"python3 {d}/log.py statusline", "refreshInterval": 5}}
json.dump(s, open(os.path.join(d, "settings.json"), "w"), indent=1)
