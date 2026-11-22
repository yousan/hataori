// What the page's terminal makes that is not the person's: the reports a
// terminal sends for the mouse and for the focus, which the page never
// sends to a session's terminal (attach.ts). It imports nothing, so that
// its unit tests run under Node by themselves (web/test/reports.test.ts).

// mouseModes are the private modes that make a terminal report the mouse
// or the focus: X10, normal, highlight, button and any event tracking,
// focus events, and the UTF-8, SGR, urxvt and SGR-pixels encodings.
export const mouseModes: ReadonlySet<number> = new Set([9, 1000, 1001, 1002, 1003, 1004, 1005, 1006, 1015, 1016]);

// personal reports whether data, what the terminal emulator made of one
// input, came from the person: a key or a paste, and not a mouse report or
// a focus report, which the page never sends.
export function personal(data: string): boolean {
  if (/^\x1b\[<\d+;\d+;\d+[Mm]$/.test(data)) return false; // SGR mouse
  if (/^\x1b\[M[\s\S]{3}$/.test(data)) return false; // X10 mouse
  if (/^\x1b\[\d+;\d+;\d+M$/.test(data)) return false; // urxvt mouse
  if (data === "\x1b[I" || data === "\x1b[O") return false; // focus
  return data !== "";
}
