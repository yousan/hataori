#!/bin/sh
# mkrun.sh <run-dir> [permission]: make a run directory with a scratch settings file that
# logs every hook to <run-dir>/hooks.jsonl, and an MCP config that starts channel.py.
set -eu
run=$(mkdir -p "$1" && cd "$1" && pwd); bin=$(cd "$(dirname "$0")" && pwd)
perm=0; [ "${2:-}" = permission ] && perm=1
mkdir -p "$run/work"
hook="HOOK_LOG=$run/hooks.jsonl /usr/bin/python3 $bin/hooklog.py"
/usr/bin/python3 - "$run" "$hook" "$bin" "$perm" <<'PY'
import json, sys
run, hook, bin, perm = sys.argv[1:]
h = [{"type": "command", "command": hook}]
events = ["SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "SessionEnd",
          "SubagentStart", "SubagentStop", "PermissionRequest"]
hooks = {e: [{"hooks": h}] for e in events}
hooks["PreToolUse"] = [{"matcher": "*", "hooks": h}]
hooks["PostToolUse"] = [{"matcher": "*", "hooks": h}]
s = {"hooks": hooks, "enableAllProjectMcpServers": True, "enabledPlugins": {"discord@claude-plugins-official": False}}
if __import__("os").environ.get("ALLOW_REPLY") == "1": s["permissions"] = {"allow": ["mcp__spike__reply"]}
json.dump(s, open(f"{run}/settings.json", "w"), indent=1)
cfg = {"mcpServers": {"spike": {"command": "/usr/bin/python3", "args": [f"{bin}/channel.py"],
    "env": {"CHANNEL_LOG": f"{run}/channel.jsonl", "CHANNEL_SOCK": "/tmp/hspk-" + run.rsplit("/", 1)[1] + ".sock",
            "CHANNEL_PERMISSION": perm, "CHANNEL_ALWAYS_LOAD": __import__("os").environ.get("ALWAYS_LOAD", "0")}}}}
json.dump(cfg, open(f"{run}/mcp.json", "w"), indent=1)
json.dump(cfg, open(f"{run}/work/.mcp.json", "w"), indent=1)
PY
echo "$run"
