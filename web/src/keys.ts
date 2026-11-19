// The one fixed table that turns what the page's terminal emulator
// produces, while the person operates a host tmux pane, into the keys the
// server passes to tmux (design, section 7.2, "How keys reach the pane";
// Appendix D, HostKey). A run of printable characters is text; a key is
// one of tmux's key names, alone or with C-, M- and S-; a paste is sent as
// a paste. Everything else, such as a mouse report or a terminal's answer
// to a query, is dropped: nothing else leaves the page.
//
// It imports nothing, so that its unit tests run under Node by themselves
// (web/test/keys.test.ts).

// HostKey is one input to an operated pane, as the server takes it.
export interface HostKey {
  key?: string;
  text?: string;
  paste?: boolean;
}

// singles are the single bytes that are keys: the C0 controls, Escape and
// DEL. 0x09 is Tab, 0x0d Enter, 0x1b Escape, 0x7f BSpace; every other
// control is C- with its letter or its character (C-@ is 0x00, C-a is
// 0x01, C-[ would be Escape, C-\ 0x1c, C-] 0x1d, C-^ 0x1e, C-_ 0x1f).
export const singles: Readonly<Record<string, string>> = {
  "\x00": "C-@",
  "\x01": "C-a",
  "\x02": "C-b",
  "\x03": "C-c",
  "\x04": "C-d",
  "\x05": "C-e",
  "\x06": "C-f",
  "\x07": "C-g",
  "\x08": "C-h",
  "\x09": "Tab",
  "\x0a": "C-j",
  "\x0b": "C-k",
  "\x0c": "C-l",
  "\x0d": "Enter",
  "\x0e": "C-n",
  "\x0f": "C-o",
  "\x10": "C-p",
  "\x11": "C-q",
  "\x12": "C-r",
  "\x13": "C-s",
  "\x14": "C-t",
  "\x15": "C-u",
  "\x16": "C-v",
  "\x17": "C-w",
  "\x18": "C-x",
  "\x19": "C-y",
  "\x1a": "C-z",
  "\x1b": "Escape",
  "\x1c": "C-\\",
  "\x1d": "C-]",
  "\x1e": "C-^",
  "\x1f": "C-_",
  "\x7f": "BSpace",
};

// finals are the keys of a CSI or SS3 sequence by its final character:
// ESC [ A, or ESC [ 1 ; m A with modifiers, or ESC O A.
export const finals: Readonly<Record<string, string>> = {
  A: "Up",
  B: "Down",
  C: "Right",
  D: "Left",
  H: "Home",
  F: "End",
  P: "F1",
  Q: "F2",
  R: "F3",
  S: "F4",
};

// tildes are the keys of a CSI sequence that ends in ~, by its number:
// ESC [ 3 ~, or ESC [ 3 ; m ~ with modifiers.
export const tildes: Readonly<Record<string, string>> = {
  "1": "Home",
  "2": "IC",
  "3": "DC",
  "4": "End",
  "5": "PPage",
  "6": "NPage",
  "15": "F5",
  "17": "F6",
  "18": "F7",
  "19": "F8",
  "20": "F9",
  "21": "F10",
  "23": "F11",
  "24": "F12",
};

// modifiers are the tmux prefixes of a sequence's modifier parameter m,
// which is 1 plus Shift (1), Alt (2) and Ctrl (4).
export const modifiers: Readonly<Record<string, string>> = {
  "2": "S-",
  "3": "M-",
  "4": "M-S-",
  "5": "C-",
  "6": "C-S-",
  "7": "C-M-",
  "8": "C-M-S-",
};

// The whole sequences that are a key by themselves.
export const sequences: Readonly<Record<string, string>> = {
  "\x1b[Z": "BTab", // Shift+Tab
};

// The key names of Appendix D that take any of C-, M- and S-, and those
// that take M- only (Tab takes C- too), as the server checks them
// (control.ValidHostKey).
const anyMod = new Set(["Up", "Down", "Left", "Right", "Home", "End", "PPage", "NPage", "IC", "DC",
  "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12"]);
const metaOnly = new Set(["Enter", "BSpace", "Escape", "BTab", "Tab"]);

// validKey reports whether name is a key name the server passes to tmux:
// the same rule as control.ValidHostKey.
export function validKey(name: string): boolean {
  const seen = new Set<string>();
  let rest = name;
  while (rest.length > 2 && rest[1] === "-" && "CMS".includes(rest[0])) {
    if (seen.has(rest[0])) return false;
    seen.add(rest[0]);
    rest = rest.slice(2);
  }
  const c = seen.has("C");
  const s = seen.has("S");
  const m = seen.has("M");
  if (anyMod.has(rest)) return true;
  if (rest === "Tab") return !s;
  if (metaOnly.has(rest)) return !c && !s;
  if (rest.length !== 1) return false;
  const code = rest.charCodeAt(0);
  if (code <= 0x20 || code >= 0x7f || s || (!c && !m)) return false;
  if (c) return /[A-Za-z@[\\\]^_?]/.test(rest);
  return true;
}

// meta is the key Alt (ESC first) makes of the key name or character k:
// M- before it, with C- kept first as tmux's other names put it.
function meta(k: string): string {
  return k.startsWith("C-") ? `C-M-${k.slice(2)}` : `M-${k}`;
}

// printable reports whether ch is text: not a C0 control and not DEL.
function printable(ch: string): boolean {
  const c = ch.codePointAt(0)!;
  return c >= 0x20 && c !== 0x7f && !(c >= 0x80 && c < 0xa0);
}

// parseSequence reads one CSI or SS3 sequence at the start of s, which
// starts with ESC [ or ESC O, and returns its key, or "" for a sequence
// the table does not know, and how many characters it took.
function parseSequence(s: string): [string, number] {
  const whole = Object.keys(sequences).find((q) => s.startsWith(q));
  if (whole) return [sequences[whole], whole.length];
  if (s[1] === "O") {
    if (s.length < 3) return ["", s.length];
    return [finals[s[2]] ?? "", 3];
  }
  // ESC [ parameters (digits and ;) intermediate bytes, then a final byte.
  const m = /^\x1b\[([0-9;]*)([ -/]*)([@-~])/.exec(s);
  if (!m) return ["", s.length]; // cut short: nothing of it is sent
  const [all, params, mid, final] = m;
  if (mid !== "") return ["", all.length];
  const ps = params === "" ? [] : params.split(";");
  let base: string | undefined;
  let mod: string | undefined;
  if (final === "~") {
    base = tildes[ps[0] ?? ""];
    mod = ps[1];
    if (ps.length > 2) base = undefined;
  } else {
    base = finals[final];
    if (ps.length === 2 && ps[0] === "1") mod = ps[1];
    else if (ps.length !== 0) base = undefined;
  }
  if (!base) return ["", all.length];
  if (mod === undefined || mod === "1") return [base, all.length];
  const prefix = modifiers[mod];
  return [prefix ? prefix + base : "", all.length];
}

// toKeys turns what the terminal emulator produced for one input into the
// keys sent to the pane, in order. A run of printable characters is one
// text; ESC before a key or a character is Alt (M-); a sequence or byte
// the table does not know is dropped, and so is a key the server would
// refuse.
export function toKeys(data: string): HostKey[] {
  const out: HostKey[] = [];
  let text = "";
  const flush = () => {
    if (text !== "") out.push({ text });
    text = "";
  };
  const key = (k: string) => {
    flush();
    if (k !== "" && validKey(k)) out.push({ key: k });
  };
  const chars = [...data];
  for (let i = 0; i < chars.length; ) {
    const ch = chars[i];
    if (ch === "\x1b" && i + 1 < chars.length) {
      const next = chars[i + 1];
      // A string sequence (OSC, DCS, APC, PM, SOS), such as a terminal's
      // answer to a query: dropped whole, up to its BEL or ESC \. ESC and
      // one of these characters alone is Alt with it, below.
      if ("]P_^X".includes(next) && i + 2 < chars.length) {
        const rest = chars.slice(i + 2).join("");
        const bel = rest.indexOf("\x07");
        const st = rest.indexOf("\x1b\\");
        const cut = [bel, st].filter((n) => n >= 0);
        flush();
        if (cut.length === 0) break; // cut short: the rest is part of it
        const at = Math.min(...cut);
        i += 2 + [...rest.slice(0, at)].length + (at === st && (bel < 0 || st < bel) ? 2 : 1);
        continue;
      }
      if ((next === "[" || next === "O") && i + 2 < chars.length) {
        const [k, n] = parseSequence(chars.slice(i).join(""));
        key(k);
        i += [...chars.slice(i).join("").slice(0, n)].length;
        continue;
      }
      // Alt: ESC, then the key or the character it changes.
      if (singles[next] !== undefined) key(meta(singles[next]));
      else if (printable(next) && next.length === 1 && next.charCodeAt(0) < 0x7f) key(`M-${next}`);
      else flush(); // Alt with a character beyond ASCII: no key name has one
      i += 2;
      continue;
    }
    if (singles[ch] !== undefined) {
      key(singles[ch]);
      i++;
      continue;
    }
    if (printable(ch)) {
      text += ch;
      i++;
      continue;
    }
    flush(); // a C1 control: dropped
    i++;
  }
  flush();
  return out;
}

// eventKeys are the named keys by their KeyboardEvent.key: the page takes
// these from the keyboard itself while operating, before the terminal
// emulator sees them, since the emulator drops some of their modifiers
// (Ctrl+Tab is a plain Tab, Alt+PageUp a plain PageUp, Shift+PageUp
// scrolls its own view).
export const eventKeys: Readonly<Record<string, string>> = {
  Enter: "Enter",
  Tab: "Tab",
  Backspace: "BSpace",
  Escape: "Escape",
  ArrowUp: "Up",
  ArrowDown: "Down",
  ArrowLeft: "Left",
  ArrowRight: "Right",
  Home: "Home",
  End: "End",
  PageUp: "PPage",
  PageDown: "NPage",
  Insert: "IC",
  Delete: "DC",
  F1: "F1",
  F2: "F2",
  F3: "F3",
  F4: "F4",
  F5: "F5",
  F6: "F6",
  F7: "F7",
  F8: "F8",
  F9: "F9",
  F10: "F10",
  F11: "F11",
  F12: "F12",
};

// KeyPress is what eventKey reads of a KeyboardEvent.
export interface KeyPress {
  key: string;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
  metaKey: boolean;
}

// eventKey is the key a named key's press sends, with its modifiers as
// far as tmux names them: Shift+Tab is BTab, Ctrl+Backspace is C-h as a
// terminal sends it, and a modifier tmux has no name for with that key is
// left out (Ctrl+Enter is Enter). null for any other press, which the
// terminal emulator turns into text or a control character, and for one
// with the Meta key, which has no tmux name.
export function eventKey(e: KeyPress): string | null {
  const base = eventKeys[e.key];
  if (base === undefined || e.metaKey) return null;
  if (base === "BSpace" && e.ctrlKey) return e.altKey ? "C-M-h" : "C-h";
  let name = base;
  let shift = e.shiftKey;
  if (base === "Tab" && shift) {
    name = "BTab";
    shift = false;
  }
  const tries: [boolean, boolean, boolean][] = [
    [e.ctrlKey, e.altKey, shift],
    [e.ctrlKey, e.altKey, false],
    [false, e.altKey, shift],
    [false, e.altKey, false],
  ];
  for (const [c, m, s] of tries) {
    const k = `${c ? "C-" : ""}${m ? "M-" : ""}${s ? "S-" : ""}${name}`;
    if (validKey(k)) return k;
  }
  return name;
}

// pasted is what a paste of s sends: one paste, or nothing for an empty
// one.
export function pasted(s: string): HostKey[] {
  return s === "" ? [] : [{ text: s, paste: true }];
}
