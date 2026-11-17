#!/usr/bin/env python3
"""timeline.py <run-dir> [since-epoch]: hooks and channel events of one run, in time order."""
import json, sys
R = sys.argv[1]; T0 = float(sys.argv[2]) if len(sys.argv) > 2 else 0
ev = []
try:
    for l in open(R + "/hooks.jsonl"):
        d = json.loads(l); p = d["payload"]
        if d["ts"] < T0: continue
        x = p.get("prompt") or p.get("last_assistant_message") or p.get("message") or p.get("tool_input") or ""
        ev.append((d["ts"], "hook", d["event"], p.get("tool_name", ""), x))
except FileNotFoundError:
    pass
for l in (open(R + "/channel.jsonl") if __import__("os").path.exists(R + "/channel.jsonl") else []):
    d = json.loads(l)
    if d["ts"] < T0 or d["kind"] in ("recv", "send", "start"): continue
    ev.append((d["ts"], "chan", d["kind"], "", d.get("arguments") or d.get("content") or d.get("params") or d.get("request_id")))
base = T0 or (ev[0][0] if ev else 0)
for t, s, e, tn, x in sorted(ev, key=lambda e: e[0]):
    x = x if isinstance(x, str) else json.dumps(x)
    print(f"{t - base:7.1f}  {s}  {e:<18} {tn:<10} {x[:140]!r}")
