#!/usr/bin/env python3
"""codex-drive.py <log> <workdir> [turn]: start `codex app-server`, read the rate limits, log every line.

With POLLS=N in the environment, read them again every 60 s, N times. With a third argument, then run
one turn and read them once more. The log holds the account (from `account/read`), host and paths:
the stored captures are made from it by scrub.py, never copied."""
import json, subprocess, sys, time, threading
log, work = sys.argv[1], sys.argv[2]
turn = len(sys.argv) > 3
p = subprocess.Popen(["codex", "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, cwd=work)
out = open(log, "w"); t0 = time.time(); lines = []
def reader():
    for l in p.stdout:
        d = json.loads(l); lines.append(d)
        out.write(json.dumps({"t": round(time.time() - t0, 2), "dir": "in", "msg": d}) + "\n"); out.flush()
threading.Thread(target=reader, daemon=True).start()
n = 0
def send(method, params=None, notify=False):
    global n
    m = {"method": method}
    if params is not None: m["params"] = params
    if not notify: n += 1; m["id"] = n
    out.write(json.dumps({"t": round(time.time() - t0, 2), "dir": "out", "msg": m}) + "\n"); out.flush()
    p.stdin.write(json.dumps(m) + "\n"); p.stdin.flush()
    return m.get("id")
def wait(pred, timeout=120):
    end = time.time() + timeout
    while time.time() < end:
        for d in list(lines):
            if pred(d): return d
        time.sleep(0.1)
def resp(i): return wait(lambda d: d.get("id") == i and ("result" in d or "error" in d))
resp(send("initialize", {"clientInfo": {"name": "usage-research", "version": "0"}}))
send("initialized", notify=True)
resp(send("account/rateLimits/read"))
resp(send("account/read", {}))
import os
for _ in range(int(os.environ.get("POLLS", "0"))):
    time.sleep(60); resp(send("account/rateLimits/read"))
if turn:
    r = resp(send("thread/start", {"cwd": work}))
    tid = r["result"]["thread"]["id"]
    resp(send("turn/start", {"threadId": tid, "input": [{"type": "text", "text": "Reply with the single word: pong"}]}))
    wait(lambda d: d.get("method") == "turn/completed", 180)
    time.sleep(3)
    resp(send("account/rateLimits/read"))
time.sleep(2)
p.stdin.close(); p.wait(timeout=20)
