import assert from "node:assert/strict";
import { test } from "node:test";
import { entries, merge } from "../src/conversation.ts";

test("an undelivered input remains visible independently of a running turn", () => {
  const event = { pos: 42, time: "2026-01-02T03:04:05Z", kind: "input_failed", data: { principal: "sender", text: "held input", reason: "not delivered" } };
  const events = merge([], [event]);
  assert.deepEqual(entries(events), [{ kind: "input_failed", pos: event.pos, time: event.time, who: "sender", text: "held input", reason: "not delivered" }]);
  assert.equal(merge(events, [event]), events);
});
