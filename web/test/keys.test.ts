// Unit tests of the key table (src/keys.ts): one per entry, each with the
// key written out here rather than read from the table. Run with
// `node --test test/` (npm run check runs them).

import { test } from "node:test";
import assert from "node:assert/strict";
import { eventKey, eventKeys, finals, type KeyPress, modifiers, pasted, sequences, singles, tildes, toKeys, validKey } from "../src/keys.ts";

const ESC = "\x1b";

// Each entry of the table, the bytes xterm.js produces for it, and the key.
const singleCases: [string, string][] = [
  ["\x00", "C-@"], ["\x01", "C-a"], ["\x02", "C-b"], ["\x03", "C-c"], ["\x04", "C-d"], ["\x05", "C-e"],
  ["\x06", "C-f"], ["\x07", "C-g"], ["\x08", "C-h"], ["\x09", "Tab"], ["\x0a", "C-j"], ["\x0b", "C-k"],
  ["\x0c", "C-l"], ["\x0d", "Enter"], ["\x0e", "C-n"], ["\x0f", "C-o"], ["\x10", "C-p"], ["\x11", "C-q"],
  ["\x12", "C-r"], ["\x13", "C-s"], ["\x14", "C-t"], ["\x15", "C-u"], ["\x16", "C-v"], ["\x17", "C-w"],
  ["\x18", "C-x"], ["\x19", "C-y"], ["\x1a", "C-z"], ["\x1b", "Escape"], ["\x1c", "C-\\"], ["\x1d", "C-]"],
  ["\x1e", "C-^"], ["\x1f", "C-_"], ["\x7f", "BSpace"],
];
const finalCases: [string, string][] = [
  ["A", "Up"], ["B", "Down"], ["C", "Right"], ["D", "Left"], ["H", "Home"], ["F", "End"],
  ["P", "F1"], ["Q", "F2"], ["R", "F3"], ["S", "F4"],
];
const tildeCases: [string, string][] = [
  ["1", "Home"], ["2", "IC"], ["3", "DC"], ["4", "End"], ["5", "PPage"], ["6", "NPage"],
  ["15", "F5"], ["17", "F6"], ["18", "F7"], ["19", "F8"], ["20", "F9"], ["21", "F10"], ["23", "F11"], ["24", "F12"],
];
const modifierCases: [string, string][] = [
  ["2", "S-"], ["3", "M-"], ["4", "M-S-"], ["5", "C-"], ["6", "C-S-"], ["7", "C-M-"], ["8", "C-M-S-"],
];
const sequenceCases: [string, string][] = [[`${ESC}[Z`, "BTab"]];
const eventCases: [string, string][] = [
  ["Enter", "Enter"], ["Tab", "Tab"], ["Backspace", "BSpace"], ["Escape", "Escape"],
  ["ArrowUp", "Up"], ["ArrowDown", "Down"], ["ArrowLeft", "Left"], ["ArrowRight", "Right"],
  ["Home", "Home"], ["End", "End"], ["PageUp", "PPage"], ["PageDown", "NPage"], ["Insert", "IC"], ["Delete", "DC"],
  ["F1", "F1"], ["F2", "F2"], ["F3", "F3"], ["F4", "F4"], ["F5", "F5"], ["F6", "F6"],
  ["F7", "F7"], ["F8", "F8"], ["F9", "F9"], ["F10", "F10"], ["F11", "F11"], ["F12", "F12"],
];

// press is a KeyboardEvent of key with the modifiers mods names (C, M, S,
// W for the Meta key).
function press(key: string, mods = ""): KeyPress {
  return { key, ctrlKey: mods.includes("C"), altKey: mods.includes("M"), shiftKey: mods.includes("S"), metaKey: mods.includes("W") };
}

// keysOf is toKeys of data as key names, a text as "text:…".
function keysOf(data: string): string[] {
  return toKeys(data).map((k) => (k.key !== undefined ? k.key : `${k.paste ? "paste" : "text"}:${k.text}`));
}

// Every entry of each table has a case here, and every case an entry.
test("every entry of the table is tested", () => {
  const same = (table: Readonly<Record<string, string>>, cases: [string, string][], name: string) =>
    assert.deepEqual(Object.keys(table).sort(), cases.map(([k]) => k).sort(), `${name}: the table and its tests`);
  same(singles, singleCases, "singles");
  same(finals, finalCases, "finals");
  same(tildes, tildeCases, "tildes");
  same(modifiers, modifierCases, "modifiers");
  same(sequences, sequenceCases, "sequences");
  same(eventKeys, eventCases, "eventKeys");
});

for (const [b, want] of singleCases) {
  test(`single byte 0x${b.charCodeAt(0).toString(16).padStart(2, "0")} is ${want}`, () => {
    assert.deepEqual(keysOf(b), [want]);
    assert.ok(validKey(want));
  });
  // ESC before it is Alt.
  const alt = want.startsWith("C-") ? `C-M-${want.slice(2)}` : `M-${want}`;
  test(`Alt and byte 0x${b.charCodeAt(0).toString(16).padStart(2, "0")} is ${alt}`, () => {
    assert.deepEqual(keysOf(ESC + b), validKey(alt) ? [alt] : []);
  });
}

for (const [f, want] of finalCases) {
  test(`CSI ${f} is ${want}`, () => assert.deepEqual(keysOf(`${ESC}[${f}`), [want]));
  test(`SS3 ${f} is ${want}`, () => assert.deepEqual(keysOf(`${ESC}O${f}`), [want]));
  test(`CSI 1;1 ${f} is ${want}`, () => assert.deepEqual(keysOf(`${ESC}[1;1${f}`), [want]));
}

for (const [n, want] of tildeCases) {
  test(`CSI ${n} ~ is ${want}`, () => assert.deepEqual(keysOf(`${ESC}[${n}~`), [want]));
}

for (const [m, prefix] of modifierCases) {
  test(`modifier ${m} is ${prefix}`, () => {
    assert.deepEqual(keysOf(`${ESC}[1;${m}A`), [`${prefix}Up`]);
    assert.deepEqual(keysOf(`${ESC}[1;${m}P`), [`${prefix}F1`]);
    assert.deepEqual(keysOf(`${ESC}[3;${m}~`), [`${prefix}DC`]);
    assert.deepEqual(keysOf(`${ESC}[24;${m}~`), [`${prefix}F12`]);
  });
}

for (const [s, want] of sequenceCases) {
  test(`${JSON.stringify(s)} is ${want}`, () => assert.deepEqual(keysOf(s), [want]));
}

test("text is one text, wide characters and all", () => {
  assert.deepEqual(keysOf("ls -la"), ["text:ls -la"]);
  assert.deepEqual(keysOf("幅の広い文字 é"), ["text:幅の広い文字 é"]);
  assert.deepEqual(keysOf("echo hi\r"), ["text:echo hi", "Enter"]);
});

test("Alt with a printable ASCII character is M- with it", () => {
  assert.deepEqual(keysOf(`${ESC}a`), ["M-a"]);
  assert.deepEqual(keysOf(`${ESC};`), ["M-;"]);
  assert.deepEqual(keysOf(`${ESC}[`), ["M-["]);
  assert.deepEqual(keysOf(`${ESC}O`), ["M-O"]);
  assert.deepEqual(keysOf(`${ESC}é`), []); // no key name has it
});

test("a paste is one paste", () => {
  assert.deepEqual(pasted("one\ntwo\t\x7f"), [{ text: "one\ntwo\t\x7f", paste: true }]);
  assert.deepEqual(pasted(""), []);
});

test("nothing the table does not know leaves the page", () => {
  for (const s of [
    `${ESC}[I`, `${ESC}[O`, // focus in and out
    `${ESC}[<0;10;5M`, `${ESC}[M !!`, // mouse reports
    `${ESC}[12;40R`, // a cursor position report
    `${ESC}[?1;2c`, `${ESC}[>0;276;0c`, // device attributes
    `${ESC}]11;rgb:0000/0000/0000${ESC}\\`, // a colour report
    `${ESC}[200~`, `${ESC}[201~`, // bracketed-paste marks
    `${ESC}[99~`, `${ESC}[1;9A`, `${ESC}[2;5A`, `${ESC}[1;2;3A`, `${ESC}[ A`, `${ESC}[`, `${ESC}O`,
    "\x85", "\x9b",
  ]) {
    for (const k of toKeys(s)) {
      assert.ok(k.key === undefined || validKey(k.key), `${JSON.stringify(s)} gave the key ${k.key}`);
      assert.ok(k.key !== undefined || !k.text?.includes(ESC), `${JSON.stringify(s)} gave the text ${JSON.stringify(k.text)}`);
    }
    if (!s.startsWith(`${ESC}]`) && s !== `${ESC}[M !!` && s.length > 2) {
      assert.deepEqual(keysOf(s), [], `${JSON.stringify(s)} is not a key`);
    }
  }
});

test("validKey is the server's rule", () => {
  for (const k of ["Enter", "Tab", "BTab", "BSpace", "Escape", "Up", "C-Up", "C-M-S-F12", "S-Home", "M-Enter", "C-Tab", "M-x", "C-a", "C-M-a", "C-@", "C-_", "M-;", "C-?"]) {
    assert.ok(validKey(k), k);
  }
  for (const k of ["a", "S-a", "S-Tab", "C-Enter", "C-BSpace", "C-Escape", "C-{", "C-1", "C-/", "C-C-a", "Space", "C-ab", "", "M-", "M- "]) {
    assert.ok(!validKey(k), k);
  }
});

test("every key the table makes is one the server takes", () => {
  for (const [, k] of [...singleCases, ...finalCases, ...tildeCases, ...sequenceCases]) assert.ok(validKey(k), k);
  for (const [, p] of modifierCases) for (const [, k] of [...finalCases, ...tildeCases]) assert.ok(validKey(p + k), p + k);
});

for (const [k, want] of eventCases) {
  test(`the key ${k} is ${want}`, () => assert.equal(eventKey(press(k)), want));
}

test("a named key's modifiers, as tmux names them", () => {
  const cases: [string, string, string][] = [
    ["PageUp", "S", "S-PPage"], ["PageUp", "M", "M-PPage"], ["PageDown", "C", "C-NPage"], ["Insert", "M", "M-IC"],
    ["ArrowUp", "CMS", "C-M-S-Up"], ["F5", "S", "S-F5"], ["F12", "CM", "C-M-F12"], ["Home", "C", "C-Home"], ["Delete", "MS", "M-S-DC"],
    ["Tab", "C", "C-Tab"], ["Tab", "M", "M-Tab"], ["Tab", "S", "BTab"], ["Tab", "MS", "M-BTab"], ["Tab", "CS", "BTab"],
    ["Enter", "C", "Enter"], ["Enter", "S", "Enter"], ["Enter", "M", "M-Enter"], ["Enter", "CM", "M-Enter"],
    ["Backspace", "C", "C-h"], ["Backspace", "CM", "C-M-h"], ["Backspace", "M", "M-BSpace"], ["Backspace", "S", "BSpace"],
    ["Escape", "C", "Escape"], ["Escape", "M", "M-Escape"],
  ];
  for (const [k, mods, want] of cases) {
    assert.equal(eventKey(press(k, mods)), want, `${mods} ${k}`);
    assert.ok(validKey(want), want);
  }
});

test("other presses are the terminal emulator's", () => {
  for (const k of ["a", "A", " ", "Shift", "Control", "Alt", "Meta", "CapsLock", "ArrowUpp", "F13", "Dead", "Process", "Unidentified"]) {
    assert.equal(eventKey(press(k)), null, k);
    assert.equal(eventKey(press(k, "C")), null, `C ${k}`);
  }
  assert.equal(eventKey(press("ArrowUp", "W")), null, "with the Meta key");
});

test("a terminal's answers to queries are dropped whole", () => {
  for (const s of [`${ESC}]11;rgb:0000/0000/0000${ESC}\\`, `${ESC}]10;rgb:ffff/ffff/ffff\x07`, `${ESC}P1$r0m${ESC}\\`, `${ESC}_x${ESC}\\`]) {
    assert.deepEqual(keysOf(s), [], JSON.stringify(s));
  }
  assert.deepEqual(keysOf(`${ESC}]11;rgb:0000/0000/0000${ESC}\\ok`), ["text:ok"]);
  assert.deepEqual(keysOf(`${ESC}]`), ["M-]"]);
  assert.deepEqual(keysOf(`${ESC}P`), ["M-P"]);
});
