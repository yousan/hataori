// Unit tests of the last choice of a start (src/lastChoice.ts).

import { test } from "node:test";
import assert from "node:assert/strict";
import { chosenOf } from "../src/lastChoice.ts";

const ev = (pos: number, kind: string, choice?: string) => ({ pos, kind, time: "", data: choice ? { choice } : {} });

test("a choice is known until the second turn after it", () => {
  const c = chosenOf([ev(1, "turn_started"), ev(5, "start_chosen", "trusted")], null);
  assert.equal(c?.data.choice, "trusted");
  const first = chosenOf([ev(5, "start_chosen", "trusted"), ev(7, "turn_started")], c);
  assert.equal(first?.turn, 7);
  assert.equal(chosenOf([ev(5, "start_chosen", "trusted"), ev(7, "turn_started"), ev(9, "turn_started")], first), null);
  assert.equal(chosenOf([ev(5, "start_chosen", "trusted"), ev(7, "turn_started"), ev(9, "turn_started")], null), null);
});

test("also when the events read no longer reach back to it, or to the first turn", () => {
  const c = chosenOf([ev(5, "start_chosen", "trusted"), ev(7, "turn_started")], null);
  assert.equal(chosenOf([ev(90, "state")], c), c);
  assert.equal(chosenOf([ev(90, "state"), ev(91, "turn_started")], c), null);
});

test("a later choice takes its place", () => {
  const c = chosenOf([ev(5, "start_chosen", "without project settings")], null);
  assert.equal(chosenOf([ev(5, "start_chosen", "without project settings"), ev(8, "start_chosen", "trusted")], c)?.data.choice, "trusted");
  const old = chosenOf([ev(5, "start_chosen", "trusted"), ev(7, "turn_started"), ev(9, "turn_started"), ev(12, "start_chosen", "closed")], null);
  assert.equal(old?.data.choice, "closed");
});
