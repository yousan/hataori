"""Privacy boundaries checked against real, already-scrubbed wire recordings."""

import copy
from decimal import Decimal
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
    def test_person_steps_and_message_shapes(self):
        docs = recordings("person-2026-11-28")
        self.assertEqual([d["sequence"] for d in docs], list(range(1, 67)))
        steps = [d for d in docs if d.get("source") == "operator-step"]
        self.assertEqual(len(steps), 14)
        self.assertIsNone(docs[-1]["socket_failure"])
        envelopes = {d["body"]["envelope_id"]: d["body"] for d in docs
                     if d.get("source") == "socket-receive" and d["body"].get("envelope_id")}
        matched = [envelopes[d["envelope_id"]] for d in steps]
        first = matched[0]["payload"]["event"]
        self.assertNotIn("subtype", first)
        for key in ("bot_id", "app_id"):
            self.assertNotIn(key, first)
        self.assertIn("<@"+first["user"]+">", first["text"])
        self.assertIn("<https://example.invalid/redacted-url-", first["text"])
        self.assertIn("&amp; &lt; &gt;", first["text"])
        self.assertIn("```code```", first["text"])
        reply, edit, deleted, broadcast, dm = [e["payload"]["event"] for e in matched[1:6]]
        self.assertEqual(edit["message"]["ts"], reply["ts"])
        self.assertEqual(edit["previous_message"]["text"], reply["text"])
        self.assertEqual(deleted["deleted_ts"], reply["ts"])
        self.assertEqual(deleted["previous_message"]["text"], edit["message"]["text"])
        self.assertEqual(broadcast["subtype"], "thread_broadcast")
        self.assertEqual(broadcast["thread_ts"], reply["thread_ts"])
        self.assertEqual(broadcast["root"]["ts"], reply["thread_ts"])
        self.assertEqual(dm["channel_type"], "im")
        self.assertEqual(dm["user"], first["user"])
        bot = next(d["body"] for d in docs if d.get("request", {}).get("method") == "auth.test")
        root = next(d["body"] for d in docs if d.get("request", {}).get("method") == "chat.postMessage")
        r = capture.Recorder.__new__(capture.Recorder)
        r.channel, r.dm, r.operator = root["channel"], dm["channel"], first["user"]
        r.start = float(root["ts"])-1
        r.cfg = {"SLACK_TEAM_ID": bot["team_id"], "SLACK_BOT_USER_ID": bot["user_id"],
                 "SLACK_APP_ID": root["message"]["app_id"], "SLACK_OTHER_APP_ID": "A0000000999"}
        r.expected_webhooks = {"[hataori test] "+s for s in ("other-channel", "other-thread", "own-channel")}
        for d in docs:
            if d.get("source") == "socket-receive":
                self.assertTrue(r.allowed(d["body"]), d["sequence"])
        clicks = [e["payload"]["actions"][0] for e in matched[6:9]]
        self.assertEqual([a["action_id"] for a in clicks], ["capture_twice", "capture_twice", "capture_late"])
        self.assertNotEqual(clicks[0]["action_ts"], clicks[1]["action_ts"])
        self.assertGreaterEqual(Decimal(reply["ts"]), Decimal(reply["thread_ts"]))
        # Each 90-second event observation window finishes before advancing/end.
        for step, end in [(steps[-2], steps[-1]), (steps[-1], docs[-1])]:
            self.assertGreaterEqual(end["elapsed_seconds"]-step["elapsed_seconds"], 90)

    def test_delayed_interaction_redelivery(self):
        docs = recordings("person-2026-11-28")
        late = [d for d in docs if d.get("source") == "socket-receive"
                and d["body"].get("type") == "interactive"
                and d["body"]["payload"]["actions"][0]["action_id"] == "capture_late"]
        self.assertEqual(len(late), 2)
        first, second = late
        self.assertEqual(first["body"]["envelope_id"], second["body"]["envelope_id"])
        self.assertEqual(first["body"]["payload"]["actions"][0]["action_ts"],
                         second["body"]["payload"]["actions"][0]["action_ts"])
        acks = [d for d in docs if d.get("source") == "socket-send"
                and d["envelope_id"] == first["body"]["envelope_id"]]
        start = first["received_seconds"]
        self.assertAlmostEqual(second["received_seconds"]-start, 2.963419, places=5)
        self.assertEqual(len(acks), 2)
        for ack, expected in zip(acks, (5.001512, 7.964642)):
            self.assertAlmostEqual(ack["elapsed_seconds"]-start, expected, places=5)

    def test_scrub_interactive_workspace_domain(self):
        envelope = copy.deepcopy(next(d for d in recordings("person-2026-11-28")
                                     if d.get("source") == "socket-receive"
                                     and d["body"].get("type") == "interactive"))
        # Test-only scalar substitution in a captured interactive payload.
        envelope["body"]["payload"]["team"]["domain"] = "test-workspace-domain"
        with tempfile.TemporaryDirectory() as tmp:
            source, out = Path(tmp)/"source", Path(tmp)/"out"
            source.mkdir()
            (source/"event.json").write_text(json.dumps(envelope))
            scrub.scrub(source, out)
            result = json.loads((out/"event.json").read_text())
            self.assertRegex(result["body"]["payload"]["team"]["domain"], r"^name-[0-9]+$")
            self.assertNotIn("test-workspace-domain", json.dumps(result))

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
