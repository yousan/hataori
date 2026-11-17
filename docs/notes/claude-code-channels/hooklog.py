#!/usr/bin/env python3
"""Hook command: append {ts, event, payload} to $HOOK_LOG and allow nothing by itself."""
import json, os, sys, time
payload = json.load(sys.stdin)
with open(os.environ["HOOK_LOG"], "a") as f:
    f.write(json.dumps({"ts": time.time(), "event": payload.get("hook_event_name"), "payload": payload}) + "\n")
