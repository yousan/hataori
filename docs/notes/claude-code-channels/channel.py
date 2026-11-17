#!/usr/bin/env python3
"""A minimal two-way Claude Code channel: an MCP server over stdio with no dependencies.

Inbound: connect to the Unix socket $CHANNEL_SOCK and send one JSON object per line:
  {"type": "message", "content": "...", "meta": {"chat_id": "1"}}
  {"type": "verdict", "request_id": "abcde", "behavior": "allow"}
Outbound: every reply-tool call, permission request and protocol message is appended
to $CHANNEL_LOG as one JSON object per line, with a time stamp.
"""
import json, os, socket, sys, threading, time

LOG = os.environ["CHANNEL_LOG"]
SOCK = os.environ["CHANNEL_SOCK"]
out_lock = threading.Lock()
log_lock = threading.Lock()


def log(kind, **kw):
    with log_lock, open(LOG, "a") as f:
        f.write(json.dumps({"ts": time.time(), "kind": kind, **kw}) + "\n")


def send(msg):
    with out_lock:
        if os.environ.get("CHANNEL_TRACE") == "1":
            log("send", msg=msg)
        sys.stdout.write(json.dumps(msg) + "\n")
        sys.stdout.flush()


INSTRUCTIONS = (
    'Messages arrive as <channel source="spike" chat_id="...">. '
    "Always answer each one by calling the reply tool with the chat_id from the tag and your full answer as text."
)
REPLY_TOOL = {
    "name": "reply",
    "description": "Send a message back over this channel",
    "inputSchema": {
        "type": "object",
        "properties": {
            "chat_id": {"type": "string", "description": "The conversation to reply in"},
            "text": {"type": "string", "description": "The message to send"},
        },
        "required": ["chat_id", "text"],
    },
}


def handle(msg):
    method, mid = msg.get("method"), msg.get("id")
    if method == "initialize":
        caps = {"experimental": {"claude/channel": {}}, "tools": {}}
        if os.environ.get("CHANNEL_PERMISSION") == "1":
            caps["experimental"]["claude/channel/permission"] = {}
        send({"jsonrpc": "2.0", "id": mid, "result": {
            "protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"),
            "capabilities": caps,
            "serverInfo": {"name": "spike", "version": "0.0.1"},
            "instructions": INSTRUCTIONS}})
    elif method == "tools/list":
        tool = dict(REPLY_TOOL)
        if os.environ.get("CHANNEL_ALWAYS_LOAD") == "1":
            tool["_meta"] = {"anthropic/alwaysLoad": True}
        send({"jsonrpc": "2.0", "id": mid, "result": {"tools": [tool]}})
    elif method == "tools/call":
        p = msg["params"]
        log("reply_tool", name=p.get("name"), arguments=p.get("arguments"))
        send({"jsonrpc": "2.0", "id": mid, "result": {"content": [{"type": "text", "text": "sent"}]}})
    elif method == "notifications/claude/channel/permission_request":
        log("permission_request", params=msg.get("params"))
    elif mid is not None:
        if method == "ping":
            send({"jsonrpc": "2.0", "id": mid, "result": {}})
        else:
            send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "method not found"}})


def inbound():
    if os.path.exists(SOCK):
        os.unlink(SOCK)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(SOCK)
    srv.listen(8)
    while True:
        conn, _ = srv.accept()
        with conn, conn.makefile() as f:
            for line in f:
                ev = json.loads(line)
                if ev["type"] == "message":
                    send({"jsonrpc": "2.0", "method": "notifications/claude/channel",
                          "params": {"content": ev["content"], "meta": ev.get("meta", {})}})
                    log("pushed", content=ev["content"], meta=ev.get("meta", {}))
                elif ev["type"] == "verdict":
                    send({"jsonrpc": "2.0", "method": "notifications/claude/channel/permission",
                          "params": {"request_id": ev["request_id"], "behavior": ev["behavior"]}})
                    log("verdict", request_id=ev["request_id"], behavior=ev["behavior"])


def main():
    log("start", pid=os.getpid())
    threading.Thread(target=inbound, daemon=True).start()
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        msg = json.loads(line)
        log("recv", method=msg.get("method"), has_id="id" in msg,
            msg=msg if os.environ.get("CHANNEL_TRACE") == "1" else None)
        handle(msg)
    log("eof")


if __name__ == "__main__":
    main()
