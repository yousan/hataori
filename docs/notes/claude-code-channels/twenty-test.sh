#!/bin/sh
# twenty-test.sh <run-dir> [n]: push n neutral questions one at a time, each after the previous
# turn's Stop hook, then report how many turns were answered through the reply tool.
set -eu
R=$1; N=${2:-20}; B=$(dirname "$0"); S=/tmp/hspk-$(basename "$R").sock
stops() { grep -c '"event": "Stop"' "$R/hooks.jsonl" 2>/dev/null || true; }
T0=$(date +%s.%N)
i=1
while [ "$i" -le "$N" ]; do
  before=$(stops)
  /usr/bin/python3 "$B/push.py" "$S" message "q$i" "Question $i: what is $i times 7? Answer in one short sentence."
  w=0
  while [ "$(stops)" -le "$before" ] && [ "$w" -lt 120 ]; do sleep 1; w=$((w + 1)); done
  sleep 2
  i=$((i + 1))
done
/usr/bin/python3 - "$R" "$T0" "$N" <<'PY'
import json, sys
R, T0, N = sys.argv[1], float(sys.argv[2]), int(sys.argv[3])
replies = {}
for l in open(R + "/channel.jsonl"):
    d = json.loads(l)
    if d["ts"] >= T0 and d["kind"] == "reply_tool":
        replies.setdefault(d["arguments"]["chat_id"], []).append(d["arguments"]["text"])
stops, prompts = [], []
for l in open(R + "/hooks.jsonl"):
    d = json.loads(l)
    if d["ts"] < T0: continue
    if d["event"] == "Stop": stops.append(d["payload"].get("last_assistant_message", ""))
    if d["event"] == "UserPromptSubmit": prompts.append(d["payload"].get("prompt", ""))
via = sum(1 for i in range(1, N + 1) if f"q{i}" in replies)
print(f"turns: {N}  UserPromptSubmit: {len(prompts)}  Stop: {len(stops)}  answered through the reply tool: {via}")
for i in range(1, N + 1):
    r = replies.get(f"q{i}")
    s = stops[i - 1] if i - 1 < len(stops) else None
    print(f"q{i:<3} reply={r!r:<45} stop={s!r}"[:200])
PY
