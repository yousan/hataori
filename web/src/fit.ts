// Fitting a read-only terminal to its pane. The session's terminal keeps
// its own columns and rows: only the font the page draws them in changes,
// so the grid fills the pane's width or its height, whichever it reaches
// first. Nothing is sent to the session.

import type { Terminal } from "@xterm/xterm";

// The font sizes a fitted terminal may take, in CSS pixels.
const minFont = 4;
const maxFont = 96;
// steps is how many halvings the search makes: the font size it finds is
// within (maxFont - minFont) / 2^steps of the largest that fits.
const steps = 16;

// fit sets term's font size to the largest at which its grid fits in box.
// When even the smallest does not fit, it keeps the smallest.
export function fit(term: Terminal, box: HTMLElement): void {
  const screen = term.element?.querySelector<HTMLElement>(".xterm-screen");
  const w = box.clientWidth;
  const h = box.clientHeight;
  if (!screen || w <= 0 || h <= 0) return;
  // Setting the font size measures the font and sizes the grid at once,
  // so the grid's size can be read right after.
  const fits = (size: number) => {
    term.options.fontSize = size;
    const r = screen.getBoundingClientRect();
    return r.width <= w && r.height <= h;
  };
  let lo = minFont;
  let hi = maxFont;
  if (fits(hi)) return;
  if (!fits(lo)) return;
  for (let i = 0; i < steps; i++) {
    const mid = (lo + hi) / 2;
    if (fits(mid)) lo = mid;
    else hi = mid;
  }
  term.options.fontSize = lo;
}

// fitted keeps term fitted to box while box changes size, until the
// returned function is called. refit fits it again at once, for a change
// of the terminal's own columns or rows.
export function fitted(term: Terminal, box: HTMLElement): { refit: () => void; stop: () => void } {
  const refit = () => fit(term, box);
  const observer = new ResizeObserver(refit);
  observer.observe(box);
  refit();
  return { refit, stop: () => observer.disconnect() };
}

// sized notes term's columns and rows on its element, for tests to read.
export function sized(term: Terminal): void {
  const el = term.element;
  if (!el) return;
  el.dataset.cols = String(term.cols);
  el.dataset.rows = String(term.rows);
}
