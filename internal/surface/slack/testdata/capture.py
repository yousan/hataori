#!/usr/bin/env python3
"""Narrow Slack wire recorder. Raw output MUST stay private and outside git.

python3 capture.py --env-file PRIVATE_ENV --out PRIVATE_NEW_DIR bot
python3 capture.py --env-file PRIVATE_ENV --out PRIVATE_NEW_DIR person

Requires websocket-client==1.9.0. No Slack SDK; SDK upload parity is still unrun.
The env file is parsed as data, never sourced. Credentials are read from its
designated slack-test.* files, never command arguments or inline environment.
"""

import argparse
import datetime
import importlib.metadata
import json
import os
from pathlib import Path
import queue
import re
import shlex
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

try:
    import websocket
except ImportError:
    websocket = None  # Offline fixture checks need only the standard library.

API = "https://slack.com/api/"
PREFIX = "[hataori test] "
KEYS = {
    "SLACK_BOT_TOKEN_FILE", "SLACK_APP_TOKEN_FILE",
    "SLACK_OWN_WEBHOOK_URL_FILE", "SLACK_OTHER_WEBHOOK_URL_FILE",
    "SLACK_TEAM_ID", "SLACK_APP_ID", "SLACK_OTHER_APP_ID",
    "SLACK_TEST_CHANNEL_ID", "SLACK_NO_BOT_CHANNEL_ID",
    "SLACK_OPERATOR_USER_ID", "SLACK_OWNER_USER_ID", "SLACK_BOT_USER_ID",
    "SLACK_BOT_ID",
}


def config(path):
    values = {}
    for line in Path(path).read_text().splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, val = line.removeprefix("export ").split("=", 1)
        key = key.strip()
        if key not in KEYS or key in values:
            raise ValueError("unexpected or duplicate test configuration key")
        parts = shlex.split(val, comments=True)
        if len(parts) != 1:
            raise ValueError("test configuration requires one literal value")
        values[key] = parts[0]
    for key, value in values.items():
        if key.endswith("_FILE"):
            p = Path(value)
            if not p.name.startswith("slack-test.") or p.is_symlink():
                raise ValueError("credential must be a designated test file")
            if p.stat().st_mode & 0o077:
                raise ValueError("credential permissions must be private")
    return values


class Recorder:
    def __init__(self, cfg, out):
        self.cfg = cfg
        self.channel = cfg["SLACK_TEST_CHANNEL_ID"]
        self.no_bot = cfg["SLACK_NO_BOT_CHANNEL_ID"]
        self.operator = cfg["SLACK_OPERATOR_USER_ID"]
        self.dm = None
        self.start = time.time()
        self.out = Path(out).resolve()
        repo = Path(__file__).resolve().parents[4]
        if self.out.is_relative_to(repo):
            raise ValueError("raw output must be outside the checkout")
        self.out.mkdir(mode=0o700, parents=False, exist_ok=False)
        self.lock = threading.Lock()
        self.send_lock = threading.Lock()
        self.index = 0
        self.events = queue.Queue()
        self.stop = threading.Event()
        self.ready = threading.Event()
        self.failure = None
        self.ws = None
        self.thread = None
        self.timers = []
        self.secrets = {k: Path(v).read_text().strip()
                        for k, v in cfg.items() if k.endswith("_FILE")}
        self.created = set()
        self.ack_modes = {}
        self.ack_choices = {}
        self.expected_webhooks = set()
        self.save("run", {"scenario": "start", "date": datetime.date.today().isoformat(),
                          "slack_go_version": "not used (direct HTTP/WebSocket)",
                          "websocket_client_version": importlib.metadata.version("websocket-client"),
                          "manifest": "manifest.json"})

    def save(self, name, obj):
        # Credentials never reach even private recordings. Socket URLs and
        # response_url values are bearer credentials too.
        def clean(value):
            if isinstance(value, dict):
                return {k: ("REDACTED-CREDENTIAL" if k in {"token", "response_url", "upload_url"}
                            else clean(v)) for k, v in value.items()}
            if isinstance(value, list):
                return [clean(v) for v in value]
            if isinstance(value, str):
                for secret in self.secrets.values():
                    if secret:
                        value = value.replace(secret, "REDACTED-CREDENTIAL")
                value = re.sub(r"(?:xox[bpe]-|xapp-)[A-Za-z0-9-]+", "REDACTED-CREDENTIAL", value)
                if value.startswith("wss://"):
                    return "REDACTED-SOCKET-URL"
            return value
        with self.lock:
            self.index += 1
            record = {"sequence": self.index, "elapsed_seconds": round(time.time()-self.start, 6),
                      **clean(obj)}
            path = self.out / f"{self.index:05d}-{name}.json"
            with path.open("x", encoding="utf-8") as f:
                json.dump(record, f, ensure_ascii=False, indent=2)
                f.write("\n")

    def http(self, label, url, data, headers, request):
        started = time.time()-self.start
        req = urllib.request.Request(url, data=data, headers=headers, method="POST")
        # Do not forward a credential through redirects.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        try:
            response = urllib.request.build_opener(NoRedirect).open(req, timeout=30)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            body = response.read()
            status = response.status
            hdr = {k.lower(): v for k, v in response.headers.items()
                   if k.lower().startswith("x-ratelimit") or k.lower() in {
                       "retry-after", "content-type", "x-slack-req-id", "x-oauth-scopes",
                       "x-accepted-oauth-scopes"}}
        try:
            body = json.loads(body)
        except ValueError:
            body = body.decode("utf-8", errors="replace")
        self.save(label, {"source": "http", "request_started_seconds": round(started, 6),
                          "request": request, "status": status, "headers": hdr, "body": body})
        print(f"{label}: HTTP {status}, " + (str(body.get("error", "ok"))
              if isinstance(body, dict) else "text response"), flush=True)
        return status, body

    def api(self, label, method, **body):
        allowed = {"auth.test", "apps.connections.open", "chat.postMessage", "chat.update",
                   "chat.delete", "reactions.add", "reactions.remove", "conversations.open",
                   "conversations.history", "conversations.replies",
                   "files.getUploadURLExternal", "files.completeUploadExternal"}
        if method not in allowed:
            raise ValueError("API method outside capture boundary")
        if "channel" in body or "channel_id" in body:
            ch = body.get("channel", body.get("channel_id"))
            if ch not in {self.channel, self.dm} or ch is None:
                if not (method == "conversations.history" and ch == self.no_bot):
                    raise ValueError("conversation outside capture boundary")
        if method == "conversations.open" and body != {"users": self.operator}:
            raise ValueError("only the operator's bot DM is permitted")
        if body.get("thread_ts") and (body.get("channel", body.get("channel_id")), body["thread_ts"]) not in self.created:
            raise ValueError("thread must have been created by this run")
        if method in {"conversations.history", "conversations.replies"}:
            if float(body.get("oldest", 0)) < self.start:
                raise ValueError("history must exclude all preexisting messages")
        if method in {"chat.update", "chat.delete", "reactions.add", "reactions.remove"}:
            ts = body.get("ts", body.get("timestamp"))
            if (body.get("channel"), ts) not in self.created:
                raise ValueError("mutations may target only this run's bot messages")
        app = method == "apps.connections.open"
        key = "SLACK_APP_TOKEN_FILE" if app else "SLACK_BOT_TOKEN_FILE"
        encoded = {k: json.dumps(v) if isinstance(v, (dict, list, bool)) else str(v)
                   for k, v in body.items()}
        status, result = self.http(label, API+method, urllib.parse.urlencode(encoded).encode(),
                                  {"Authorization": "Bearer "+self.secrets[key],
                                   "Content-Type": "application/x-www-form-urlencoded"},
                                  {"method": method, "body": body})
        if method == "chat.postMessage" and result.get("ok"):
            self.created.add((result["channel"], result["ts"]))
        return status, result

    def post(self, label, text, **extra):
        time.sleep(1.1)
        _, result = self.api(label, "chat.postMessage", channel=self.channel,
                             text=PREFIX+text, unfurl_links=False, unfurl_media=False, **extra)
        if not result.get("ok"):
            raise RuntimeError("test post failed (see private recording)")
        return result["ts"]

    def connect(self):
        if websocket is None:
            raise RuntimeError("install the pinned websocket-client dependency")
        _, auth = self.api("auth", "auth.test")
        if not auth.get("ok") or auth.get("team_id") != self.cfg["SLACK_TEAM_ID"] or auth.get("user_id") != self.cfg["SLACK_BOT_USER_ID"]:
            raise ValueError("token does not match authorized test identity")
        _, dm = self.api("open-dm", "conversations.open", users=self.operator)
        if not dm.get("ok"):
            raise ValueError("could not open operator DM")
        self.dm = dm["channel"]["id"]
        _, connection = self.api("open-socket", "apps.connections.open")
        if not connection.get("ok"):
            raise ValueError("could not open Socket Mode")
        self.ws = websocket.create_connection(connection["url"], timeout=1,
                                               enable_multithread=True)
        self.thread = threading.Thread(target=self.receive, daemon=True)
        self.thread.start()
        if not self.ready.wait(15):
            raise RuntimeError("no Socket Mode hello")

    def allowed(self, envelope):
        kind = envelope.get("type")
        if kind in {"hello", "disconnect"}:
            return True
        payload = envelope.get("payload", {})
        if payload.get("team_id", payload.get("team", {}).get("id")) != self.cfg["SLACK_TEAM_ID"]:
            return False
        if kind == "events_api":
            event = payload.get("event", {})
            if event.get("type") != "message" or event.get("channel") not in {self.channel, self.dm}:
                return False
            if float(event.get("event_ts", event.get("ts", 0))) < self.start:
                return False
            msg = event.get("message", event.get("previous_message", event))
            # Edits/deletions have a new event_ts but may embed an old message.
            for embedded in (msg, event.get("previous_message", {})):
                if embedded and float(embedded.get("ts", 0)) < self.start:
                    return False
            if msg.get("thread_ts") and float(msg["thread_ts"]) < self.start:
                return False
            user = msg.get("user")
            app = msg.get("app_id", msg.get("bot_profile", {}).get("app_id"))
            if user == self.operator and not msg.get("text", "").startswith(PREFIX):
                return False
            return (user in {self.operator, self.cfg["SLACK_BOT_USER_ID"]}
                    or app in {self.cfg["SLACK_APP_ID"], self.cfg["SLACK_OTHER_APP_ID"]}
                    or (msg.get("subtype") == "bot_message"
                        and msg.get("text") in self.expected_webhooks))
        if kind == "interactive":
            return (payload.get("channel", {}).get("id") in {self.channel, self.dm}
                    and payload.get("user", {}).get("id") == self.operator
                    and float(payload.get("message", {}).get("ts", 0)) >= self.start)
        return False

    def ack(self, envelope_id, record=True):
        if self.stop.is_set():
            return
        with self.send_lock:
            self.ws.send(json.dumps({"envelope_id": envelope_id}))
            if record:
                self.save("ack", {"source": "socket-send", "envelope_id": envelope_id})

    def receive(self):
        try:
            while not self.stop.is_set():
                try:
                    data = self.ws.recv()
                except websocket.WebSocketTimeoutException:
                    continue
                if not data:
                    break
                received = time.time()-self.start
                envelope = json.loads(data)
                if not self.allowed(envelope):
                    # No off-target payload is persisted, including in logs.
                    if envelope.get("envelope_id"):
                        self.ack(envelope["envelope_id"], record=False)
                    continue
                eid = envelope.get("envelope_id")
                policy = "immediate"
                if eid:
                    payload = envelope.get("payload", {})
                    event = payload.get("event", {})
                    text = event.get("text", "")
                    actions = payload.get("actions", [])
                    action = actions[0].get("action_id", "") if actions else ""
                    mode = self.ack_modes.get(text, self.ack_modes.get(action, "immediate"))
                    policy = self.ack_choices.setdefault(eid, mode)
                self.save("envelope", {"source": "socket-receive", "ack_policy": policy,
                                       "received_seconds": round(received, 6),
                                       "body": envelope})
                if eid and policy == "immediate":
                    self.ack(eid)
                self.events.put(envelope)
                if eid and policy == "late":
                    timer = threading.Timer(5, self.ack, args=(eid,))
                    self.timers.append(timer)
                    timer.start()
                if envelope.get("type") == "hello":
                    self.ready.set()
                if envelope.get("type") == "disconnect":
                    break
        except Exception:
            if not self.stop.is_set():
                self.failure = "Socket Mode receive failed; private recording is incomplete"
        finally:
            if not self.stop.is_set() and self.failure is None:
                self.failure = "Socket Mode connection ended; no automatic reconnect"

    def close(self):
        self.stop.set()
        for timer in self.timers:
            timer.cancel()
        if self.ws:
            self.ws.close()
        if self.thread:
            self.thread.join(3)
        self.save("end", {"source": "run-end", "socket_failure": self.failure,
                          "created_bot_messages": [{"channel": c, "ts": t}
                                                   for c, t in sorted(self.created)]})


def pages(r, method, label, **body):
    cursor = None
    for n in range(1, 5):
        args = dict(channel=r.channel, limit=2, oldest=f"{r.start:.6f}",
                    include_all_metadata=True, **body)
        if cursor:
            args["cursor"] = cursor
        status, result = r.api(f"{label}-{n}", method, **args)
        if status == 429 or not result.get("ok"):
            break
        cursor = result.get("response_metadata", {}).get("next_cursor")
        if not cursor:
            break
        time.sleep(1.1)


def bot(r):
    root = r.post("post-text", "channel plain text")
    md = [{"type": "markdown", "text": "**hataori test markdown**"}]
    r.post("post-markdown", "notification fallback", blocks=md)
    r.post("post-metadata", "metadata", metadata={"event_type": "hataori_test",
                                                  "event_payload": {"key": "capture-341"}})
    for n in range(3):
        r.post(f"post-thread-{n}", f"thread reply {n}", thread_ts=root)
    r.api("update", "chat.update", channel=r.channel, ts=root, text=PREFIX+"edited root")
    for method, name in [("reactions.add", "reaction-add"), ("reactions.add", "reaction-duplicate"),
                         ("reactions.remove", "reaction-remove")]:
        r.api(name, method, channel=r.channel, timestamp=root, name="eyes")
    pages(r, "conversations.replies", "replies", ts=root)
    pages(r, "conversations.history", "history")
    r.api("missing-thread", "conversations.replies", channel=r.channel,
          ts=f"{int(r.start)+1}.000001", oldest=f"{r.start:.6f}", limit=2)
    r.api("no-bot-history", "conversations.history", channel=r.no_bot,
          oldest=f"{r.start:.6f}", limit=2)
    doomed = r.post("post-delete-target", "delete this run's bot post")
    for n in (1, 2):
        r.api(f"delete-{n}", "chat.delete", channel=r.channel, ts=doomed)
    # Keep original request and any warning/truncation in the response.
    time.sleep(1.1)
    r.api("overlong-text", "chat.postMessage", channel=r.channel, text=PREFIX+"x"*40001)
    time.sleep(1.1)
    r.api("overlong-markdown", "chat.postMessage", channel=r.channel,
          text=PREFIX+"long markdown fallback", blocks=[{"type": "markdown", "text": "x"*12001}])
    # Deliberately bounded. A missing 429 is an unrun observation, not success.
    hit = False
    for n in range(30):
        status, body = r.api(f"rate-update-{n}", "chat.update", channel=r.channel,
                             ts=root, text=PREFIX+f"rate test {n}")
        if status == 429 or body.get("error") == "ratelimited":
            hit = True
            break
    r.save("rate-result", {"source": "observation", "hit_rate_limit": hit, "max_calls": 30})
    # The HTTP upload flow is recorded without an SDK. Never follow arbitrary URLs.
    content = b"hataori test capture file\n"
    _, upload = r.api("upload-url", "files.getUploadURLExternal", filename="hataori-test.txt",
                      length=len(content))
    if upload.get("ok"):
        url = urllib.parse.urlsplit(upload["upload_url"])
        if url.scheme != "https" or url.hostname != "files.slack.com":
            raise ValueError("unexpected Slack upload host")
        status, _ = r.http("upload-bytes", upload["upload_url"], content,
                           {"Content-Type": "application/octet-stream"},
                           {"method": "external-upload", "content": content.decode()})
        if status == 200:
            r.api("upload-complete", "files.completeUploadExternal", channel_id=r.channel,
                  thread_ts=root, files=[{"id": upload["file_id"], "title": "hataori-test.txt"}])
    time.sleep(8)


def webhooks(r, root, timeout):
    # URLs were bound by the operator to the designated test channel. There is
    # no channel override, lookup, creation or invitation.
    for name, key, thread in [("other-channel", "SLACK_OTHER_WEBHOOK_URL_FILE", False),
                              ("other-thread", "SLACK_OTHER_WEBHOOK_URL_FILE", True),
                              ("own-channel", "SLACK_OWN_WEBHOOK_URL_FILE", False)]:
        body = {"text": PREFIX+name}
        if thread:
            body["thread_ts"] = root
        r.expected_webhooks.add(body["text"])
        url = r.secrets[key]
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != "https" or parsed.hostname != "hooks.slack.com":
            raise ValueError("unexpected webhook host")
        r.http("webhook-"+name, url, json.dumps(body).encode(),
               {"Content-Type": "application/json"}, {"method": "incoming-webhook", "body": body})
        wait_for(r, "observe webhook "+name,
                 lambda e, text=body["text"]: e.get("payload", {}).get("event", {}).get("text") == text,
                 timeout)


def acknowledgments(r):
    for mode in ("late", "never"):
        text = PREFIX+"bot ACK "+mode
        r.ack_modes[text] = mode
        r.post("ack-"+mode, "bot ACK "+mode)
        print("Observing "+mode+" ACK retries for 90 seconds", flush=True)
        time.sleep(90)


def formats(r):
    root = r.post("formats-root", "format scenarios root")
    variants = [
        ("plain", {}),
        ("markdown", {"blocks": [{"type": "markdown", "text": "**hataori test markdown**"}]}),
        ("metadata", {"metadata": {"event_type": "hataori_test", "event_payload": {"key": "capture-341"}}}),
        ("buttons", {"blocks": [{"type": "actions", "elements": [{"type": "button",
          "action_id": "capture_format", "text": {"type": "plain_text", "text": "Test button"},
          "value": "hataori-test"}]}]}),
    ]
    for location in ("channel", "thread"):
        for name, extra in variants:
            args = dict(extra)
            if location == "thread":
                args["thread_ts"] = root
            r.post("format-"+location+"-"+name, location+" "+name+" fallback", **args)
    pages(r, "conversations.replies", "format-replies", ts=root)
    pages(r, "conversations.history", "format-history")
    time.sleep(5)


def wait_for(r, step, predicate, seconds):
    print("STEP "+step, flush=True)
    deadline = time.monotonic()+seconds
    while time.monotonic() < deadline:
        if r.failure:
            raise RuntimeError(r.failure)
        try:
            event = r.events.get(timeout=1)
        except queue.Empty:
            continue
        if predicate(event):
            r.save("step", {"source": "operator-step", "step": step,
                            "envelope_id": event.get("envelope_id")})
            return event
    raise TimeoutError("operator step timed out")


def person(r, timeout):
    actions = [{"type": "actions", "elements": [
        {"type": "button", "action_id": name, "text": {"type": "plain_text", "text": label},
         "value": "hataori-test"} for name, label in [
             ("capture_twice", "Press twice"), ("capture_late", "ACK after five seconds")]]}]
    root = r.post("person-root", "operator scenario root; use its thread", blocks=actions)
    r.ack_modes["capture_late"] = "late"
    def event(e):
        return e.get("payload", {}).get("event", {})
    def human(e):
        return event(e).get("user") == r.operator
    wait_for(r, "1: channel text: [hataori test] URL https://example.com mention yourself & < > ```code```",
             lambda e: human(e) and event(e).get("channel") == r.channel
             and event(e).get("text", "").startswith(PREFIX+"URL "), timeout)
    reply = wait_for(r, "2: reply to the bot's scenario root: [hataori test] reply",
                     lambda e: human(e) and event(e).get("thread_ts") == root
                     and event(e).get("text") == PREFIX+"reply", timeout)
    ts = event(reply)["ts"]
    wait_for(r, "3: edit your reply to [hataori test] reply edited",
             lambda e: event(e).get("subtype") == "message_changed"
             and event(e).get("message", {}).get("ts") == ts, timeout)
    wait_for(r, "4: delete ONLY that generated reply",
             lambda e: event(e).get("subtype") == "message_deleted"
             and event(e).get("deleted_ts") == ts, timeout)
    wait_for(r, "5: reply [hataori test] broadcast with Also send to channel enabled",
             lambda e: human(e) and event(e).get("subtype") == "thread_broadcast"
             and event(e).get("thread_ts") == root, timeout)
    wait_for(r, "6: DM the bot: [hataori test] DM",
             lambda e: human(e) and event(e).get("channel") == r.dm
             and event(e).get("text") == PREFIX+"DM", timeout)
    for n in (1, 2):
        wait_for(r, f"7.{n}: press Press twice once",
                 lambda e: any(a.get("action_id") == "capture_twice"
                               for a in e.get("payload", {}).get("actions", [])), timeout)
    wait_for(r, "8: press ACK after five seconds; note what the Slack UI shows",
             lambda e: any(a.get("action_id") == "capture_late"
                           for a in e.get("payload", {}).get("actions", [])), timeout)
    webhooks(r, root, timeout)
    for mode in ("late", "never"):
        text = PREFIX+"ACK "+mode
        r.ack_modes[text] = mode
        wait_for(r, "9: channel post exactly: "+text,
                 lambda e, text=text: human(e) and event(e).get("text") == text, timeout)
        print("Observing retries for 90 seconds; wait before next step", flush=True)
        time.sleep(90)
    print("Person sequence complete; record the delayed button's UI result separately", flush=True)


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--step-timeout", type=int, default=600)
    parser.add_argument("mode", choices=["bot", "person", "webhooks", "ack", "formats"])
    args = parser.parse_args()
    recorder = None
    try:
        recorder = Recorder(config(args.env_file), args.out)
        recorder.connect()
        if args.mode == "bot":
            bot(recorder)
        elif args.mode == "webhooks":
            root = recorder.post("webhook-root", "webhook scenario root")
            webhooks(recorder, root, args.step_timeout)
            time.sleep(5)
        elif args.mode == "ack":
            acknowledgments(recorder)
        elif args.mode == "formats":
            formats(recorder)
        else:
            person(recorder, args.step_timeout)
        if recorder.failure:
            raise RuntimeError(recorder.failure)
    except (Exception, KeyboardInterrupt) as e:
        # Network exceptions can embed signed URLs; do not print their values.
        print("Capture stopped: "+type(e).__name__+"; inspect private recordings", file=sys.stderr)
        return 1
    finally:
        if recorder:
            recorder.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
