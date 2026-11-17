#!/usr/bin/env python3
"""agent-states.py <agents.jsonl> <t0> <cwd-substring>: the state changes of background sessions under cwd."""
import json, sys
path, t0, sub = sys.argv[1], float(sys.argv[2]), sys.argv[3]
last = {}
text, i, dec, recs = open(path).read(), 0, json.JSONDecoder(), []
while i < len(text):
    while i < len(text) and text[i].isspace():
        i += 1
    if i >= len(text):
        break
    d, i = dec.raw_decode(text, i)
    recs.append(d)
for d in recs:
    for a in d["agents"] or []:
        if sub not in a.get("cwd", "") or a.get("kind") != "background":
            continue
        key = a.get("id")
        v = (a.get("state"), a.get("status"), a.get("waitingFor"), a.get("pid"), a.get("sessionId"))
        if last.get(key) != v:
            print(f"{d['ts'] - t0:7.1f}  id={key} state={v[0]} status={v[1]} waitingFor={v[2]} pid={v[3]} sessionId={v[4]}")
            last[key] = v
