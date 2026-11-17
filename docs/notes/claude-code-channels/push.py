#!/usr/bin/env python3
"""Push one event into channel.py: push.py SOCK message <chat_id> <text> | push.py SOCK verdict <id> allow|deny"""
import json, socket, sys
sock, kind = sys.argv[1], sys.argv[2]
ev = ({"type": "message", "content": sys.argv[4], "meta": {"chat_id": sys.argv[3]}} if kind == "message"
      else {"type": "verdict", "request_id": sys.argv[3], "behavior": sys.argv[4]})
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock)
s.sendall((json.dumps(ev) + "\n").encode())
s.close()
