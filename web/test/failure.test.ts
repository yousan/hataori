// Unit tests of the failure reason (src/failure.ts). Run with
// `node --test test/` (npm run check runs them).

import { test } from "node:test";
import assert from "node:assert/strict";
import { failureReason } from "../src/failure.ts";

const trust =
  "the folder-trust dialog says trusting the folder loads .claude/settings.json; " +
  "this scope does not trust .claude/settings.json; to trust them, add them to the scope's trust_files";

test("a readable detail follows the reason", () => {
  assert.equal(failureReason({ reason: "blocked at start", detail: trust }), "blocked at start: " + trust);
});

test("a screen stays out", () => {
  assert.equal(failureReason({ reason: "blocked at start", detail: "\x1b[2JAccessing workspace:\r\n/w" }), "blocked at start");
  assert.equal(failureReason({ reason: "blocked at start", detail: "two\r\nlines" }), "blocked at start");
  assert.equal(failureReason({ reason: "blocked at start", detail: "x".repeat(2001) }), "blocked at start");
});

test("a detail of several lines goes below the reason", () => {
  const d = "hataori did not answer the dialog for you.\nClaude Code showed:\n> Trusting this folder loads .claude/settings.json.\n> ❯ No, exit";
  assert.equal(failureReason({ reason: "blocked at start", detail: d }), "blocked at start.\n" + d);
});

test("no reason, no text", () => {
  assert.equal(failureReason({ detail: trust }), undefined);
  assert.equal(failureReason(undefined), undefined);
  assert.equal(failureReason({ reason: "crashed" }), "crashed");
});
