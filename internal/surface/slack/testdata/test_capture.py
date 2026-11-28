"""Privacy boundaries checked against real, already-scrubbed wire recordings."""

import copy
import json
from pathlib import Path
import tempfile
import unittest

import capture
import scrub

ROOT = Path(__file__).parent


def recordings(scenario):
    return [json.loads(p.read_text()) for p in sorted((ROOT/"captures"/scenario).glob("*.json"))]


class CaptureTests(unittest.TestCase):
    def test_event_boundary(self):
        docs = recordings("bot-2026-11-28")
        envelope = next(d["body"] for d in docs if d.get("source") == "socket-receive"
                        and d["body"].get("type") == "events_api")
        event = envelope["payload"]["event"]
        r = capture.Recorder.__new__(capture.Recorder)
        r.channel, r.dm = event["channel"], "D0000000999"
        r.start = float(event["ts"])-1
        r.operator = "U0000000998"
        r.cfg = {"SLACK_TEAM_ID": envelope["payload"]["team_id"],
                 "SLACK_BOT_USER_ID": event["user"], "SLACK_APP_ID": event["app_id"],
                 "SLACK_OTHER_APP_ID": "A0000000999"}
        r.expected_webhooks = set()
        self.assertTrue(r.allowed(envelope))
        for field, replacement in [("channel", "C0000000999"), ("event_ts", "1.000000"), ("ts", "1.000000")]:
            changed = copy.deepcopy(envelope)
            changed["payload"]["event"][field] = replacement
            self.assertFalse(r.allowed(changed))
        changed = copy.deepcopy(envelope)
        changed["payload"]["team_id"] = "T0000000999"
        self.assertFalse(r.allowed(changed))

    def test_webhook_without_app_or_user(self):
        docs = recordings("webhooks-2026-11-28")
        envelope = next(d["body"] for d in docs if d.get("source") == "socket-receive"
                        and d["body"].get("payload", {}).get("event", {}).get("subtype") == "bot_message")
        event = envelope["payload"]["event"]
        r = capture.Recorder.__new__(capture.Recorder)
        r.channel, r.dm = event["channel"], None
        r.start = float(event["ts"])-1
        r.operator = "U0000000998"
        r.cfg = {"SLACK_TEAM_ID": envelope["payload"]["team_id"],
                 "SLACK_BOT_USER_ID": "U0000000999", "SLACK_APP_ID": "A0000000998",
                 "SLACK_OTHER_APP_ID": "A0000000999"}
        r.expected_webhooks = set()
        self.assertFalse(r.allowed(envelope))
        r.expected_webhooks.add(event["text"])
        self.assertTrue(r.allowed(envelope))
        changed = copy.deepcopy(envelope)
        changed["payload"]["event"]["channel"] = "C0000000999"
        self.assertFalse(r.allowed(changed))

    def test_preexisting_embedded_edit(self):
        docs = recordings("bot-2026-11-28")
        envelope = copy.deepcopy(next(d["body"] for d in docs if d.get("source") == "socket-receive"
                             and d["body"].get("payload", {}).get("event", {}).get("subtype") == "message_changed"))
        e = envelope["payload"]["event"]
        r = capture.Recorder.__new__(capture.Recorder)
        r.channel, r.dm = e["channel"], "D0000000999"
        r.start = float(e["message"]["ts"])-1
        r.operator = e["message"]["user"]
        r.cfg = {"SLACK_TEAM_ID": envelope["payload"]["team_id"],
                 "SLACK_BOT_USER_ID": "U0000000999", "SLACK_APP_ID": e["message"]["app_id"],
                 "SLACK_OTHER_APP_ID": "A0000000999"}
        r.expected_webhooks = set()
        self.assertTrue(r.allowed(envelope))
        e["previous_message"]["ts"] = "1.000000"
        self.assertFalse(r.allowed(envelope))
        e["previous_message"]["ts"] = e["message"]["ts"]
        e["message"]["ts"] = "1.000000"
        self.assertFalse(r.allowed(envelope))

    def test_api_boundary_before_network(self):
        docs = recordings("bot-2026-11-28")
        request = next(d["request"] for d in docs if d.get("source") == "http"
                       and d["request"]["method"] == "chat.delete")
        r = capture.Recorder.__new__(capture.Recorder)
        r.channel = request["body"]["channel"]
        r.no_bot, r.dm, r.operator = "C0000000999", None, "U0000000999"
        r.created = set()
        r.start = 1000000000
        with self.assertRaises(ValueError):
            r.api("refused", request["method"], **request["body"])
        with self.assertRaises(ValueError):
            r.api("refused", "users.list")
        with self.assertRaises(ValueError):
            r.api("refused", "conversations.history", channel=r.channel, oldest="0")
        with self.assertRaises(ValueError):
            r.api("refused", "conversations.open", users="U0000000998")

    def test_scrub_preserves_order_and_input(self):
        # Real recordings are the parser input, with an artificial time shift
        # and identity substitution to exercise the scrubber's transformation.
        docs = recordings("bot-2026-11-28")[:15]
        before = json.dumps(docs)
        shifted = scrub.TIMESTAMP.sub(
            lambda m: f"{scrub.decimal.Decimal(m.group())+700000000:.6f}", before)
        docs = json.loads(shifted)
        original_user = next(d["body"]["user_id"] for d in docs
                             if d.get("request", {}).get("method") == "auth.test")
        with tempfile.TemporaryDirectory() as tmp:
            raw, out = Path(tmp)/"raw", Path(tmp)/"scrubbed"
            raw.mkdir()
            for i, doc in enumerate(docs):
                (raw/f"{i:03d}.json").write_text(json.dumps(doc).replace(original_user, "UABC1234567"))
            raw_before = [p.read_bytes() for p in sorted(raw.glob("*"))]
            scrub.scrub(raw, out)
            cleaned = [json.loads(p.read_text()) for p in sorted(out.glob("*"))]
            self.assertEqual(raw_before, [p.read_bytes() for p in sorted(raw.glob("*"))])
            self.assertNotIn("UABC1234567", json.dumps(cleaned))
            old_ts = sorted(set(scrub.TIMESTAMP.findall(shifted)))
            new_ts = sorted(set(scrub.TIMESTAMP.findall(json.dumps(cleaned))))
            self.assertTrue(new_ts[0].startswith("1000000000."))
            self.assertEqual(len(old_ts), len(new_ts))
            self.assertEqual([scrub.decimal.Decimal(t)-scrub.decimal.Decimal(old_ts[0]) for t in old_ts],
                             [scrub.decimal.Decimal(t)-scrub.decimal.Decimal(new_ts[0]) for t in new_ts])
            with self.assertRaises(FileExistsError):
                scrub.scrub(raw, out)

    def test_cursor_round_trip(self):
        docs = recordings("bot-2026-11-28")
        page1 = next(d for d in docs if d.get("source") == "http" and d["sequence"] == 30)
        page2 = next(d for d in docs if d.get("source") == "http" and d["sequence"] == 31)
        self.assertEqual(page1["body"]["response_metadata"]["next_cursor"], page2["request"]["body"]["cursor"])
        with tempfile.TemporaryDirectory() as tmp:
            raw, out = Path(tmp)/"raw", Path(tmp)/"out"
            raw.mkdir()
            for i, page in enumerate((page1, page2)):
                # Substitute a test-only encoded cursor into real payloads.
                cursor = page1["body"]["response_metadata"]["next_cursor"]
                (raw/f"{i}.json").write_text(json.dumps(page).replace(cursor, "test-encoded-cursor"))
            scrub.scrub(raw, out)
            first, second = [json.loads(p.read_text()) for p in sorted(out.glob("*"))]
            cursor = first["body"]["response_metadata"]["next_cursor"]
            self.assertEqual(cursor, second["request"]["body"]["cursor"])
            self.assertNotEqual(cursor, "test-encoded-cursor")


if __name__ == "__main__":
    unittest.main()
