// Unit tests of what the page's terminal never sends to a session's
// terminal (src/reports.ts). Run with `node --test test/` (npm run check
// runs them).

import { test } from "node:test";
import assert from "node:assert/strict";
import { mouseModes, personal } from "../src/reports.ts";

const ESC = "\x1b";

test("mouse reports are not the person's", () => {
  for (const r of [`${ESC}[<0;12;5M`, `${ESC}[<0;12;5m`, `${ESC}[<64;1;1M`, `${ESC}[M !!`, `${ESC}[32;12;5M`]) {
    assert.equal(personal(r), false, JSON.stringify(r));
  }
});

test("focus reports are not the person's", () => {
  assert.equal(personal(`${ESC}[I`), false);
  assert.equal(personal(`${ESC}[O`), false);
});

test("keys and pastes are the person's", () => {
  for (const k of ["a", "reply typed here", "\r", `${ESC}OA`, `${ESC}[A`, `${ESC}[200~pasted${ESC}[201~`, "\x03", `${ESC}`, `${ESC}[1;5C`]) {
    assert.equal(personal(k), true, JSON.stringify(k));
  }
  assert.equal(personal(""), false);
});

test("the modes that report the mouse or the focus", () => {
  for (const m of [9, 1000, 1001, 1002, 1003, 1004, 1005, 1006, 1015, 1016]) assert.ok(mouseModes.has(m), String(m));
  for (const m of [1, 25, 47, 1047, 1049, 2004]) assert.ok(!mouseModes.has(m), String(m));
});
