#!/usr/bin/env python3
"""Select and scrub only the disposable real-supervisor test's exported outputs.

Usage: scrub_actual.py RAW_EXPORT NEW_CAPTURE_DIRECTORY
Never reads authentication, configuration, a database or other CLI logs.
"""
import json
from pathlib import Path
import re
import sys

raw, out = map(Path, sys.argv[1:])
root = (raw / "fixture-root.txt").read_text()
out.mkdir(parents=True, exist_ok=False)
uuids, opaque_ids = {}, {}


def scrub(value):
    text = json.dumps(value, ensure_ascii=False).replace(root, "/fixture/instance")
    text = re.sub(r"/tmp/TestRealCodexSupervisor\d+/\d+", "/fixture/repository", text)
    text = re.sub(r"/tmp/go-build\d+/b\d+/hataori.test", "/fixture/hataori", text)
    text = re.sub(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}",
                  lambda m: uuids.setdefault(m[0], "00000000-0000-4000-8000-%012d" % (len(uuids)+1)), text)
    text = re.sub(r"(?<![A-Za-z0-9])[A-Za-z]+_[0-9a-f]{20,}",
                  lambda m: opaque_ids.setdefault(m[0], "opaque_%06d" % (len(opaque_ids)+1)), text)
    text = re.sub(r"(?<![A-Za-z0-9])[0-9a-f]{32,}", "HEX_REDACTED", text)
    text = re.sub(r"rollout-[^\"/]+\.jsonl", "rollout-fixture.jsonl", text)
    text = re.sub(r"eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)?", "CREDENTIAL_REDACTED", text)
    text = re.sub(r"sk-[A-Za-z0-9_-]{20,}", "CREDENTIAL_REDACTED", text)
    return json.loads(text)


methods = {"thread/started", "turn/started", "turn/completed", "item/started",
           "item/completed", "mcpServer/startupStatus/updated", "error"}
events = []
ack = None
for line in (raw / "adapter-events.raw.jsonl").read_text().splitlines():
    row = json.loads(line)
    if row.get("id") == 4 and row.get("result") == {}:
        ack = row
    if row.get("method") not in methods and not ("id" in row and row.get("result") == {}):
        continue
    if row.get("params", {}).get("item", {}).get("type") == "reasoning":
        continue
    events.append(scrub(row))
if ack is None:
    raise SystemExit("No actual initial instruction-injection acknowledgement")
(out / "instruction-ack.json").write_text(json.dumps(scrub(ack))+"\n")
(out / "adapter-events.jsonl").write_text("".join(json.dumps(row)+"\n" for row in events))

records = []
for line in (raw / "response-items.raw.jsonl").read_text().splitlines():
    row = json.loads(line)
    if row.get("type") != "response_item":
        continue
    payload = row["payload"]
    if payload.get("type") == "reasoning":
        continue
    keep = {k: v for k, v in payload.items() if k in {
        "type", "id", "call_id", "name", "namespace", "arguments", "tools", "execution", "input", "output", "role", "content", "phase", "status"}}
    records.append(scrub({"type": "response_item", "payload": keep}))
(out / "response-items.jsonl").write_text("".join(json.dumps(row)+"\n" for row in records))
print(json.dumps({"events": len(events), "response_items": len(records)}))
