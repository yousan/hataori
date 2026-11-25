#!/usr/bin/env python3
"""Record raw Discord payloads for the driver's tests. Standard library only.

usage:
  capture.py bot    RAW_DIR      the captures a bot can take alone
  capture.py person RAW_DIR MIN  wait up to MIN minutes for the person's steps (README)
  capture.py second RAW_DIR      more REST captures for posting (no gateway connection)
  capture.py pages RAW_DIR       consecutive pages of history (no gateway connection)
  capture.py lookups RAW_DIR     the channel objects the driver looks up (no gateway connection)
  capture.py rethread RAW_DIR    a thread opened from a message, then opened again from it (no gateway connection)
  capture.py commands RAW_DIR MIN  register test commands, wait up to MIN minutes for the person to use
                                 them in the channel and in a thread (README), then restore the commands
  capture.py choices RAW_DIR MIN  post a message with three buttons and one with a menu of three options,
                                 wait up to MIN minutes for a press and a selection of two (README),
                                 then remove the buttons and the menu by editing
  capture.py lamps RAW_DIR       reactions set, read and removed on a bot message, a rejected one, and posts
                                 that mention the bot itself, @everyone and a role (no gateway connection)
  capture.py calls RAW_DIR MIN   what is a call and what is not: say one step after another on the terminal,
                                 wait up to MIN minutes for the person to do them all (README)
  capture.py cleanup RAW_DIR     delete what the bot posted in the runs above

Environment:
  HATAORI_DISCORD_TOKEN_FILE  file holding the bot token (read at run time, never printed)
  HATAORI_DISCORD_CHANNEL     the test channel's id
  HATAORI_DISCORD_OWNER       the owner's user id (person, lookups and calls runs only)

RAW_DIR receives one JSON file per capture:
  rest-<name>.json   {"source": "rest", "request", "status", "headers", "body"}
  gw-<NN>-<TYPE>.json {"source": "gateway", "op", "t", "s", "d"}
and created.json, the ids to delete in the cleanup run. Raw captures hold real ids and
must go through scrub.py before they are committed.

The gateway client opens exactly one connection, with intents GUILDS, GUILD_MESSAGES,
DIRECT_MESSAGES and MESSAGE_CONTENT, and closes it with code 1000 when the run ends.
"""

import base64, json, os, random, socket, ssl, struct, sys, threading, time
import urllib.error, urllib.request

API = "https://discord.com/api/v10"
UA = "DiscordBot (https://github.com/yousan/hataori, 0.1)"
INTENTS = (1 << 0) | (1 << 9) | (1 << 12) | (1 << 15)


def token():
    return open(os.environ["HATAORI_DISCORD_TOKEN_FILE"]).read().strip()


class Raw:
    def __init__(self, path):
        self.path = path
        os.makedirs(path, exist_ok=True)
        self.lock = threading.Lock()
        cf = os.path.join(path, "created.json")
        self.created = json.load(open(cf)) if os.path.exists(cf) else {"messages": [], "threads": []}

    def save(self, name, obj):
        with self.lock, open(os.path.join(self.path, name + ".json"), "w") as f:
            json.dump(obj, f, indent=1, ensure_ascii=False)

    def note(self, kind, value):
        with self.lock:
            self.created[kind].append(value)
            json.dump(self.created, open(os.path.join(self.path, "created.json"), "w"), indent=1)


def rest(raw, name, method, path, body=None, files=None):
    """One REST call. Saves it as rest-<name> when name is set. Returns (status, body)."""
    data, ctype = None, None
    if files:
        boundary = "hataori%016x" % random.getrandbits(64)
        parts = [b"--" + boundary.encode(), b'Content-Disposition: form-data; name="payload_json"',
                 b"Content-Type: application/json", b"", json.dumps(body).encode()]
        for i, (fname, content) in enumerate(files):
            parts += [b"--" + boundary.encode(),
                      ('Content-Disposition: form-data; name="files[%d]"; filename="%s"' % (i, fname)).encode(),
                      b"Content-Type: text/plain", b"", content]
        parts += [b"--" + boundary.encode() + b"--", b""]
        data, ctype = b"\r\n".join(parts), "multipart/form-data; boundary=" + boundary
    elif body is not None:
        data, ctype = json.dumps(body).encode(), "application/json"
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Authorization", "Bot " + token())
    req.add_header("User-Agent", UA)
    if ctype:
        req.add_header("Content-Type", ctype)
    try:
        r = urllib.request.urlopen(req)
        status, hdr, out = r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        status, hdr, out = e.code, e.headers, e.read()
    keep = {k.lower(): v for k, v in hdr.items()
            if k.lower().startswith("x-ratelimit") or k.lower() in ("retry-after", "content-type", "via")}
    parsed = json.loads(out) if out else None
    if name:
        raw.save("rest-" + name, {"source": "rest", "date": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                                  "request": {"method": method, "path": path, "body": body,
                                              "files": [f[0] for f in files or []]},
                                  "status": status, "headers": keep, "body": parsed})
    print(f"{name or '-'}: {method} {status}", file=sys.stderr)
    return status, parsed


class Gateway:
    """A minimal gateway client: JSON encoding, no compression, one connection."""

    def __init__(self, raw, on_dispatch=None, keep=True):
        self.raw, self.on_dispatch, self.keep = raw, on_dispatch, keep
        self.seq, self.n, self.ready = None, 0, threading.Event()
        self.closed = threading.Event()
        self.send_lock = threading.Lock()

    def connect(self):
        _, gw = rest(None, None, "GET", "/gateway/bot")
        host = gw["url"].split("://", 1)[1].rstrip("/")
        ctx = ssl.create_default_context()
        s = ctx.wrap_socket(socket.create_connection((host, 443)), server_hostname=host)
        key = base64.b64encode(os.urandom(16)).decode()
        s.sendall((f"GET /?v=10&encoding=json HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\n"
                   f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
                   f"User-Agent: {UA}\r\n\r\n").encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            resp += s.recv(1)
        if b" 101 " not in resp.split(b"\r\n", 1)[0]:
            raise RuntimeError("gateway upgrade failed: " + resp.split(b"\r\n", 1)[0].decode())
        self.sock, self.buf = s, b""
        threading.Thread(target=self.read_loop, daemon=True).start()

    def recv_exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def read_frame(self):
        msg, op = b"", None
        while True:
            b1, b2 = self.recv_exact(2)
            fin, opcode, n = b1 & 0x80, b1 & 0x0F, b2 & 0x7F
            if n == 126:
                n = struct.unpack(">H", self.recv_exact(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self.recv_exact(8))[0]
            payload = self.recv_exact(n)
            if opcode == 9:
                self.send_frame(10, payload)
                continue
            if opcode == 8:
                return 8, payload
            if opcode != 0:
                op = opcode
            msg += payload
            if fin:
                return op, msg

    def send_frame(self, opcode, payload):
        mask = os.urandom(4)
        n = len(payload)
        head = bytes([0x80 | opcode])
        head += bytes([0x80 | n]) if n < 126 else (bytes([0x80 | 126]) + struct.pack(">H", n) if n < 65536
                                                     else bytes([0x80 | 127]) + struct.pack(">Q", n))
        body = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        with self.send_lock:
            self.sock.sendall(head + mask + body)

    def send(self, obj):
        self.send_frame(1, json.dumps(obj).encode())

    def heartbeat(self, interval):
        time.sleep(interval * random.random())
        while not self.closed.is_set():
            try:
                self.send({"op": 1, "d": self.seq})
            except OSError:
                return
            self.closed.wait(interval)

    def read_loop(self):
        try:
            while True:
                op, data = self.read_frame()
                if op == 8:
                    code = struct.unpack(">H", data[:2])[0] if len(data) >= 2 else None
                    print(f"gateway closed by the service: {code}", file=sys.stderr)
                    break
                ev = json.loads(data)
                if ev.get("s") is not None:
                    self.seq = ev["s"]
                if ev["op"] == 10:
                    threading.Thread(target=self.heartbeat, args=(ev["d"]["heartbeat_interval"] / 1000,),
                                     daemon=True).start()
                    self.send({"op": 2, "d": {"token": token(), "intents": INTENTS,
                                              "properties": {"os": "linux", "browser": "hataori-capture",
                                                             "device": "hataori-capture"}}})
                elif ev["op"] == 0:
                    self.n += 1
                    if self.keep:
                        self.raw.save(f"gw-{self.n:02d}-{ev['t']}", {"source": "gateway", **ev})
                    print(f"gateway: {ev['t']}", file=sys.stderr)
                    if ev["t"] == "READY":
                        self.ready.set()
                    if self.on_dispatch:
                        self.on_dispatch(ev)
        except (EOFError, OSError):
            pass
        self.closed.set()

    def close(self):
        if not self.closed.is_set():
            try:
                self.send_frame(8, struct.pack(">H", 1000))
            except OSError:
                pass
            self.closed.wait(5)
            self.closed.set()
        self.sock.close()
        print("gateway connection closed", file=sys.stderr)


def bot_run(raw):
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    gw = Gateway(raw)
    gw.connect()
    try:
        if not gw.ready.wait(30):
            raise RuntimeError("no READY")
        rest(raw, "users-me", "GET", "/users/@me")
        st, m = rest(raw, "post-channel", "POST", f"/channels/{ch}/messages",
                     {"content": "[hataori test] a message from the bot account"})
        raw.note("messages", [ch, m["id"]])
        st, th = rest(raw, "thread-from-message", "POST", f"/channels/{ch}/messages/{m['id']}/threads",
                      {"name": "[hataori test] thread", "auto_archive_duration": 60})
        t = th["id"]
        raw.note("threads", t)
        ids = []
        for i in range(1, 5):
            st, p = rest(raw, f"post-thread-{i}", "POST", f"/channels/{t}/messages",
                         {"content": f"[hataori test] message {i} in the thread"})
            ids.append(p["id"])
            raw.note("messages", [t, p["id"]])
        rest(raw, "history-starter", "GET", f"/channels/{t}/messages?limit=10")
        pages(raw, t, "history-page")
        rest(raw, "history-bad-cursor", "GET", f"/channels/{t}/messages?after=not-a-snowflake&limit=2")
        rest(raw, "history-unknown-channel", "GET", "/channels/1/messages?limit=2")
        st, a = rest(raw, "post-attachment", "POST", f"/channels/{t}/messages",
                     {"content": "[hataori test] a message with an attachment",
                      "attachments": [{"id": 0, "filename": "note.txt"}]},
                     files=[("note.txt", b"hello from hataori\n")])
        if st == 200:
            raw.note("messages", [t, a["id"]])
        rest(raw, "post-too-long", "POST", f"/channels/{t}/messages", {"content": "x" * 2001})
        nonce = "hataori-%d" % random.getrandbits(40)
        for i in (1, 2):
            st, p = rest(raw, f"post-nonce-{i}", "POST", f"/channels/{t}/messages",
                         {"content": "[hataori test] posted twice with one nonce", "nonce": nonce,
                          "enforce_nonce": True})
            if i == 1:
                raw.note("messages", [t, p["id"]])
        st, e = rest(raw, "edit", "PATCH", f"/channels/{t}/messages/{ids[0]}",
                     {"content": "[hataori test] message 1, edited"})
        rest(raw, "delete", "DELETE", f"/channels/{t}/messages/{ids[3]}")
        rest(raw, "delete-again", "DELETE", f"/channels/{t}/messages/{ids[3]}")
        rest(raw, "set-thread-title", "PATCH", f"/channels/{t}", {"name": "[hataori test] thread, renamed"})
        # A burst of posts into the thread until the route answers 429.
        for i in range(1, 12):
            st, p = rest(raw, f"burst-{i}", "POST", f"/channels/{t}/messages",
                         {"content": f"[hataori test] burst {i}"})
            if st == 200:
                raw.note("messages", [t, p["id"]])
                os.remove(os.path.join(raw.path, f"rest-burst-{i}.json"))
            else:
                os.rename(os.path.join(raw.path, f"rest-burst-{i}.json"),
                          os.path.join(raw.path, "rest-rate-limited.json"))
                break
        # Another route right after the 429: the thread's own messages are limited, the channel is not.
        rest(raw, "other-route-after-429", "GET", f"/channels/{ch}/messages?limit=1")
        time.sleep(3)
    finally:
        gw.close()


def person_run(raw, minutes):
    owner = os.environ["HATAORI_DISCORD_OWNER"]
    want = {"button press", "direct message", "person in channel", "webhook post"}
    seen, done = set(), threading.Event()

    def on_dispatch(ev):
        d = ev["d"]
        if ev["t"] == "INTERACTION_CREATE":
            rest(raw, "interaction-ack", "POST", f"/interactions/{d['id']}/{d['token']}/callback", {"type": 6})
            seen.add("button press")
        elif ev["t"] == "MESSAGE_CREATE":
            kind = None
            if d.get("webhook_id"):
                kind = "webhook post"
            elif d["author"]["id"] == owner:
                kind = "person in channel" if d.get("guild_id") else "direct message"
            if kind and kind not in seen:
                # The same message through REST, at once, so the person may delete it right away.
                rest(raw, "get-" + kind.replace(" ", "-"), "GET", f"/channels/{d['channel_id']}/messages/{d['id']}")
                seen.add(kind)
        print("seen: " + ", ".join(sorted(seen)), file=sys.stderr)
        if want <= seen:
            done.set()

    gw = Gateway(raw, on_dispatch)
    gw.connect()
    try:
        if not gw.ready.wait(30):
            raise RuntimeError("no READY")
        st, dm = rest(raw, "open-direct", "POST", "/users/@me/channels", {"recipient_id": owner})
        st, m = rest(raw, "post-button", "POST", f"/channels/{dm['id']}/messages",
                     {"content": "[hataori test] Please press the button, then send me one direct message.",
                      "components": [{"type": 1, "components": [
                          {"type": 2, "style": 1, "label": "Press me", "custom_id": "hataori-test-choice"}]}]})
        raw.note("messages", [dm["id"], m["id"]])
        rest(raw, "edit-direct", "PATCH", f"/channels/{dm['id']}/messages/{m['id']}",
             {"content": "[hataori test] Please press the button below, then send me one direct message.",
              "components": m["components"]})
        print("waiting for the person's steps (README)", file=sys.stderr)
        end = time.time() + 60 * minutes
        try:
            while time.time() < end and not gw.closed.is_set() and not done.is_set():
                time.sleep(1)
        except KeyboardInterrupt:
            pass
        time.sleep(3)
        rest(raw, "history-direct", "GET", f"/channels/{dm['id']}/messages?limit=10")
        rest(raw, "history-channel", "GET", f"/channels/{os.environ['HATAORI_DISCORD_CHANNEL']}/messages?limit=10")
    finally:
        gw.close()


def pages(raw, conv, name):
    """Consecutive pages of two: each after the newest message of the one before."""
    cursor, n = conv, 1
    while True:
        st, page = rest(raw, f"{name}-{n}", "GET", f"/channels/{conv}/messages?after={cursor}&limit=2")
        if not page:
            return
        cursor = max(page, key=lambda m: int(m["id"]))["id"]
        n += 1


def pages_run(raw):
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    st, th = rest(raw, "thread-for-pages", "POST", f"/channels/{ch}/threads",
                  {"name": "[hataori test] pages", "type": 11, "auto_archive_duration": 60})
    t = th["id"]
    raw.note("threads", t)
    for i in range(1, 6):
        st, p = rest(None, None, "POST", f"/channels/{t}/messages", {"content": f"[hataori test] message {i}"})
        raw.note("messages", [t, p["id"]])
    pages(raw, t, "history-page")
    st, notices = rest(None, None, "GET", f"/channels/{ch}/messages?limit=10")
    for m in notices:
        if m["type"] == 18 and (m.get("message_reference") or {}).get("channel_id") == t:
            raw.note("messages", [ch, m["id"]])


def second_run(raw):
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    st, th = rest(raw, "thread-without-message", "POST", f"/channels/{ch}/threads",
                  {"name": "[hataori test] thread without a message", "type": 11, "auto_archive_duration": 60})
    t = th["id"]
    raw.note("threads", t)
    st, p = rest(raw, "post-thread", "POST", f"/channels/{t}/messages", {"content": "[hataori test] in the thread"})
    raw.note("messages", [t, p["id"]])
    rest(raw, "set-thread-archived", "PATCH", f"/channels/{t}", {"archived": True})
    st, p = rest(raw, "post-archived-thread", "POST", f"/channels/{t}/messages",
                 {"content": "[hataori test] into the archived thread"})
    if st == 200:
        raw.note("messages", [t, p["id"]])
    rest(raw, "delete-thread-refused", "DELETE", f"/channels/{t}")
    for i in range(1, 12):
        st, p = rest(raw, f"burst-{i}", "POST", f"/channels/{t}/messages", {"content": f"[hataori test] burst {i}"})
        if st == 200:
            raw.note("messages", [t, p["id"]])
            os.remove(os.path.join(raw.path, f"rest-burst-{i}.json"))
        else:
            os.rename(os.path.join(raw.path, f"rest-burst-{i}.json"), os.path.join(raw.path, "rest-rate-limited.json"))
            break
    rest(raw, "other-route-after-429", "GET", f"/channels/{ch}/messages?limit=1")


def rethread_run(raw):
    """Open a thread from a message twice. Only the message is noted for the cleanup run:
    the bot may not delete the thread, and it is left as it is."""
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    st, m = rest(raw, "rethread-post", "POST", f"/channels/{ch}/messages",
                 {"content": "[hataori test] a message a thread is opened from twice"})
    raw.note("messages", [ch, m["id"]])
    path = f"/channels/{ch}/messages/{m['id']}/threads"
    st, th = rest(raw, "rethread-first", "POST", path, {"name": "[hataori test] thread from a message", "auto_archive_duration": 60})
    if st in (200, 201):
        print("thread left for deletion:", th["id"], file=sys.stderr)
    rest(raw, "rethread-again", "POST", path, {"name": "[hataori test] the same message again", "auto_archive_duration": 60})
    rest(raw, "rethread-get-message", "GET", f"/channels/{ch}/messages/{m['id']}")


# The fields of a channel object the driver reads. lookups keeps only these, so that
# ids of things the tests never see (the channel's category, its role overrides, its
# last message) do not enter the captures.
CHANNEL_FIELDS = ("id", "type", "guild_id", "name", "flags", "recipients")


def lookups(raw):
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    st, dm = rest(None, None, "POST", "/users/@me/channels", {"recipient_id": os.environ["HATAORI_DISCORD_OWNER"]})
    for name, cid in (("get-channel", ch), ("get-direct-channel", dm["id"])):
        rest(raw, name, "GET", f"/channels/{cid}")
        f = os.path.join(raw.path, f"rest-{name}.json")
        d = json.load(open(f))
        d["body"] = {k: v for k, v in d["body"].items() if k in CHANNEL_FIELDS}
        d["trimmed"] = list(CHANNEL_FIELDS)
        raw.save(f"rest-{name}", d)


# The test command. Its name and its options' names start with "hataori-test", which
# scrub.py keeps.
TEST_COMMANDS = [{
    "name": "hataori-test", "type": 1, "description": "[hataori test] a command for captures",
    "contexts": [0], "integration_types": [0],
    "options": [
        {"type": 3, "name": "hataori-test-text", "description": "[hataori test] required text", "required": True},
        {"type": 3, "name": "hataori-test-note", "description": "[hataori test] optional text"},
    ],
}]


def commands_run(raw, minutes):
    """Register the test command, wait for one use in the channel and one in a thread, answer
    each, and put the application's commands back as they were."""
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    st, app = rest(None, None, "GET", "/applications/@me")
    raw.save("rest-application", {"source": "rest", "request": {"method": "GET", "path": "/applications/@me"},
                                  "status": st, "body": {"id": app["id"]}, "trimmed": ["id"]})
    appid = app["id"]
    st, before = rest(raw, "commands-before", "GET", f"/applications/{appid}/commands")
    rest(raw, "commands-set", "PUT", f"/applications/{appid}/commands", TEST_COMMANDS)
    rest(raw, "commands-after", "GET", f"/applications/{appid}/commands")
    st, th = rest(raw, "thread-for-commands", "POST", f"/channels/{ch}/threads",
                  {"name": "[hataori test] commands", "type": 11, "auto_archive_duration": 60})
    raw.note("threads", th["id"])
    seen, done = set(), threading.Event()

    def on_dispatch(ev):
        d = ev["d"]
        if ev["t"] != "INTERACTION_CREATE" or d.get("type") != 2:
            return
        where = "thread" if d["channel_id"] == th["id"] else "channel"
        # In the channel the answer is public, in the thread private (flag 64, ephemeral).
        flags = 64 if where == "thread" else 0
        rest(raw, f"command-ack-{where}", "POST", f"/interactions/{d['id']}/{d['token']}/callback",
             {"type": 5, "data": {"flags": flags}})
        st, m = rest(raw, f"command-answer-{where}", "PATCH",
                     f"/webhooks/{d['application_id']}/{d['token']}/messages/@original",
                     {"content": f"[hataori test] answered in the {where}", "allowed_mentions": {"parse": []}})
        if st == 200 and not flags:
            raw.note("messages", [d["channel_id"], m["id"]])
        seen.add(where)
        print("seen: " + ", ".join(sorted(seen)), file=sys.stderr)
        if {"channel", "thread"} <= seen:
            done.set()

    gw = Gateway(raw, on_dispatch)
    gw.connect()
    try:
        if not gw.ready.wait(30):
            raise RuntimeError("no READY")
        print("waiting for the commands (README)", file=sys.stderr)
        end = time.time() + 60 * minutes
        try:
            while time.time() < end and not gw.closed.is_set() and not done.is_set():
                time.sleep(1)
        except KeyboardInterrupt:
            pass
        time.sleep(2)
    finally:
        gw.close()
        keep = ("type", "name", "description", "options", "contexts", "integration_types",
                "default_member_permissions", "nsfw", "name_localizations", "description_localizations")
        rest(None, None, "PUT", f"/applications/{appid}/commands",
             [{k: c[k] for k in keep if k in c} for c in before or []])


def choices_run(raw, minutes):
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    buttons = [{"type": 1, "components": [
        {"type": 2, "style": 2, "label": f"[hataori test] button {i}", "custom_id": f"hataori-test-button-{i}"}
        for i in (1, 2, 3)]}]
    menu = [{"type": 1, "components": [{
        "type": 3, "custom_id": "hataori-test-menu", "placeholder": "[hataori test] pick two",
        "min_values": 1, "max_values": 3,
        "options": [{"label": f"[hataori test] option {i}", "value": f"hataori-test-option-{i}"} for i in (1, 2, 3)]}]}]
    st, b = rest(raw, "post-buttons", "POST", f"/channels/{ch}/messages",
                 {"content": "[hataori test] Please press button 2.", "components": buttons,
                  "allowed_mentions": {"parse": []}})
    raw.note("messages", [ch, b["id"]])
    st, m = rest(raw, "post-menu", "POST", f"/channels/{ch}/messages",
                 {"content": "[hataori test] Please pick options 1 and 3.", "components": menu,
                  "allowed_mentions": {"parse": []}})
    raw.note("messages", [ch, m["id"]])
    seen, done = set(), threading.Event()

    def on_dispatch(ev):
        d = ev["d"]
        if ev["t"] != "INTERACTION_CREATE" or d.get("type") != 3:
            return
        kind = "menu" if d["data"].get("component_type") == 3 else "button"
        rest(raw, f"{kind}-ack", "POST", f"/interactions/{d['id']}/{d['token']}/callback", {"type": 6})
        seen.add(kind)
        print("seen: " + ", ".join(sorted(seen)), file=sys.stderr)
        if {"button", "menu"} <= seen:
            done.set()

    gw = Gateway(raw, on_dispatch)
    gw.connect()
    try:
        if not gw.ready.wait(30):
            raise RuntimeError("no READY")
        print("waiting for the press and the selection (README)", file=sys.stderr)
        end = time.time() + 60 * minutes
        try:
            while time.time() < end and not gw.closed.is_set() and not done.is_set():
                time.sleep(1)
        except KeyboardInterrupt:
            pass
        time.sleep(2)
    finally:
        gw.close()
    rest(raw, "edit-remove-buttons", "PATCH", f"/channels/{ch}/messages/{b['id']}",
         {"content": "[hataori test] answered", "components": []})
    rest(raw, "edit-remove-menu", "PATCH", f"/channels/{ch}/messages/{m['id']}",
         {"content": "[hataori test] answered", "components": []})


LAMPS = {"working": "\u23f3", "done": "\u2705", "error": "\u274c"}


def lamps_run(raw):
    """Lamps as the bot's reactions, and mentions. The posts that mention someone mention
    only the bot itself, so that no person is notified."""
    from urllib.parse import quote
    ch = os.environ["HATAORI_DISCORD_CHANNEL"]
    st, me = rest(None, None, "GET", "/users/@me")
    st, m = rest(raw, "lamp-post", "POST", f"/channels/{ch}/messages",
                 {"content": "[hataori test] a message that gets lamps", "allowed_mentions": {"parse": []}})
    raw.note("messages", [ch, m["id"]])
    base = f"/channels/{ch}/messages/{m['id']}/reactions"
    rest(raw, "lamp-add-working", "PUT", f"{base}/{quote(LAMPS['working'])}/@me")
    rest(raw, "lamp-get-working", "GET", f"/channels/{ch}/messages/{m['id']}")
    rest(raw, "lamp-add-done", "PUT", f"{base}/{quote(LAMPS['done'])}/@me")
    rest(raw, "lamp-remove-working", "DELETE", f"{base}/{quote(LAMPS['working'])}/@me")
    rest(raw, "lamp-get-done", "GET", f"/channels/{ch}/messages/{m['id']}")
    rest(raw, "lamp-remove-absent", "DELETE", f"{base}/{quote(LAMPS['error'])}/@me")
    rest(raw, "lamp-add-again", "PUT", f"{base}/{quote(LAMPS['done'])}/@me")
    rest(raw, "lamp-add-unknown-message", "PUT",
         f"/channels/{ch}/messages/{int(m['id']) - 1}/reactions/{quote(LAMPS['working'])}/@me")
    st, p = rest(raw, "post-notify", "POST", f"/channels/{ch}/messages",
                 {"content": f"<@{me['id']}> [hataori test] a ping of the bot itself",
                  "allowed_mentions": {"parse": [], "users": [me["id"]]}})
    raw.note("messages", [ch, p["id"]])
    guild = m.get("guild_id") or rest(None, None, "GET", f"/channels/{ch}")[1]["guild_id"]
    st, p = rest(raw, "post-everyone", "POST", f"/channels/{ch}/messages",
                 {"content": f"@everyone <@&{guild}> [hataori test] mentions that notify nobody",
                  "allowed_mentions": {"parse": []}})
    raw.note("messages", [ch, p["id"]])
    st, p = rest(raw, "post-quiet", "POST", f"/channels/{ch}/messages",
                 {"content": "[hataori test] a post without a notification", "flags": 4096,
                  "allowed_mentions": {"parse": []}})
    raw.note("messages", [ch, p["id"]])


# What the calls run keeps of a server: what the driver reads to learn the role that is
# managed for the bot. The names of the roles and the members are not among them.
ROLE_FIELDS = ("id", "managed", "tags")


def calls_run(raw, minutes):
    """Say the steps one after another, and keep each message as the gateway sends it and as
    REST reads it. No other dispatch is kept."""
    ch, owner = os.environ["HATAORI_DISCORD_CHANNEL"], os.environ["HATAORI_DISCORD_OWNER"]
    st, me = rest(None, None, "GET", "/users/@me")
    raw.save("rest-me", {"source": "rest", "request": {"method": "GET", "path": "/users/@me"}, "status": st,
                         "body": {"id": me["id"]}, "trimmed": ["id"]})
    st, here = rest(None, None, "GET", f"/channels/{ch}")
    guild, got = {}, []
    changed, lock = threading.Event(), threading.Lock()

    def on_dispatch(ev):
        d = ev["d"]
        if ev["t"] == "GUILD_CREATE" and d["id"] == here["guild_id"]:
            guild.update(ev)
        elif ev["t"] in ("MESSAGE_CREATE", "MESSAGE_UPDATE") and d.get("channel_id") == ch and (
                d.get("webhook_id") or d.get("author", {}).get("id") == owner):
            with lock:
                got.append(ev)
        changed.set()

    end = time.time() + 60 * minutes

    def wait(what, ok):
        """The next dispatch that ok takes; those before it are another step's, and left out."""
        while time.time() < end and not gw.closed.is_set():
            with lock:
                while got:
                    ev = got.pop(0)
                    if ok(ev):
                        return ev
            changed.wait(1)
            changed.clear()
        raise RuntimeError(f"{what}: not done")

    def keep(name, ev):
        raw.save("gw-" + name, {"source": "gateway", **ev})
        read(name, ev["d"])

    def read(name, m):
        rest(raw, name, "GET", f"/channels/{ch}/messages/{m['id']}")

    def post(name, text, **more):
        st, m = rest(raw, name, "POST", f"/channels/{ch}/messages",
                     {"content": "[hataori test] " + text, "allowed_mentions": {"parse": []}, **more})
        raw.note("messages", [ch, m["id"]])
        return m

    gw = Gateway(raw, on_dispatch, keep=False)
    gw.connect()
    try:
        while not guild and time.time() < end and not gw.closed.is_set():
            changed.wait(1)
            changed.clear()
        roles = guild["d"]["roles"]
        own = [r["id"] for r in roles if r.get("tags", {}).get("bot_id") == me["id"]]
        others = [r["id"] for r in roles if not r["managed"] and r["id"] != here["guild_id"]]
        if not own or not others:
            raise RuntimeError("the server needs the bot's own role (invite it with a permission) and one more role")
        fields = ("id", "guild_id", "type", "position", "name", "flags")
        raw.save("gw-guild", {"source": "gateway", **{k: guild[k] for k in ("op", "t", "s")}, "trimmed": True, "d": {
            "id": here["guild_id"], "threads": [],
            "channels": [{k: here[k] for k in fields if k in here}],
            "roles": [{k: r[k] for k in ROLE_FIELDS if k in r} for r in roles if r["id"] in (own[0], others[0])]}})

        at, role, other = f"<@{me['id']}>", f"<@&{own[0]}>", f"<@&{others[0]}>"
        target = post("target", "reply to this one")
        # A message of a bot that mentions another account. It pings the person.
        post("post-by-bot", f"<@{owner}> a post of a bot", allowed_mentions={"users": [owner]})

        def created(ev):
            return ev["t"] == "MESSAGE_CREATE" and not ev["d"].get("webhook_id")

        def step(name, do, ok=created):
            print(f"\n== {name}: {do}", file=sys.stderr)
            return wait(name, ok)

        def says(text):
            return "post exactly this:\n" + text

        for name, do in (
                ("mention", says(f"{at} [hataori test] hello")),
                ("mention-role", says(f"{role} [hataori test] hello")),
                ("reply", "reply to the bot's post \"reply to this one\", with the mention on: [hataori test] yes"),
                ("reply-silent", "reply to the same post with the mention off: [hataori test] yes"),
                ("quoted-and-not", says(f"> {at} [hataori test] a\n{at} b")),
                ("quoted", says(f"> {at} [hataori test] a\nb")),
                ("quoted-lines", says(f"[hataori test] a\n>>> b\n{at} c")),
                ("in-code", says(f"[hataori test] `{at}`")),
                ("in-block", says(f"[hataori test]\n```go\n{at}\n```")),
                ("in-open-block", says(f"[hataori test]\n```\n{at}")),
                ("everyone", says("@everyone [hataori test] hello")),
                ("here", says("@here [hataori test] hello")),
                ("other-role", says(f"{other} [hataori test] hello")),
                ("other", says(f"<@{owner}> [hataori test] hello")),
                ("reply-other", "reply to your own message of the step before: [hataori test] yes"),
                ("forward", "forward your message of the step \"mention\" to this channel")):
            keep(name, step(name, do))
        keep("webhook", step("webhook", f"post with a webhook of this channel: {at} [hataori test] hello",
                             lambda ev: ev["t"] == "MESSAGE_CREATE" and ev["d"].get("webhook_id")))
        ev = step("edited", "post: [hataori test] hello")
        mid = ev["d"]["id"]
        ev = step("edited", f"edit that message to exactly this:\n{at} [hataori test] hello",
                  lambda ev: ev["t"] == "MESSAGE_UPDATE" and ev["d"]["id"] == mid and ev["d"].get("edited_timestamp"))
        raw.save("gw-edited-update", {"source": "gateway", **ev})
        read("edited", ev["d"])
        ev = step("reply-deleted", "reply to the bot's post \"reply to this one\" once more: [hataori test] yes")
        rest(None, None, "DELETE", f"/channels/{ch}/messages/{target['id']}")
        time.sleep(2)
        read("reply-deleted", ev["d"])
    finally:
        gw.close()


def cleanup(raw):
    for conv, mid in raw.created["messages"]:
        st, _ = rest(None, None, "DELETE", f"/channels/{conv}/messages/{mid}")
        while st == 429:
            time.sleep(2)
            st, _ = rest(None, None, "DELETE", f"/channels/{conv}/messages/{mid}")
    for t in raw.created["threads"]:
        st, _ = rest(None, None, "DELETE", f"/channels/{t}")
        if st != 200:
            rest(None, None, "PATCH", f"/channels/{t}", {"archived": True})


if __name__ == "__main__":
    cmd, path = sys.argv[1], sys.argv[2]
    raw = Raw(path)
    {"bot": lambda: bot_run(raw), "person": lambda: person_run(raw, float(sys.argv[3])),
     "second": lambda: second_run(raw), "lookups": lambda: lookups(raw), "pages": lambda: pages_run(raw), "rethread": lambda: rethread_run(raw),
     "commands": lambda: commands_run(raw, float(sys.argv[3])),
     "choices": lambda: choices_run(raw, float(sys.argv[3])), "lamps": lambda: lamps_run(raw),
     "calls": lambda: calls_run(raw, float(sys.argv[3])), "cleanup": lambda: cleanup(raw)}[cmd]()
