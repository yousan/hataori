// Fitting a read-only terminal to its pane. The session's terminal keeps
// its own columns and rows: only the font the page draws them in changes,
// so the grid fills the pane's width or its height, whichever it reaches
// first. Nothing is sent to the session.
//
// xterm.js draws each row a whole number of device pixels tall, so when
// the height is reached first, the grid's bottom may stop short of the
// pane's by up to one device pixel per row. Scaling the drawing to close
// that gap would put the mouse on other cells than the ones shown.

import type { Terminal } from "@xterm/xterm";

// The font sizes a fitted terminal may take, in CSS pixels. Below the
// smallest, a grid that still does not fit scrolls in its box.
const minFont = 4;
const maxFont = 512;

// fit sets term's font size to the largest at which its grid fits in box,
// to within a fraction of a cell.
export function fit(term: Terminal, box: HTMLElement): void {
  const screen = term.element?.querySelector<HTMLElement>(".xterm-screen");
  const w = box.clientWidth;
  const h = box.clientHeight;
  if (!screen || w <= 0 || h <= 0 || term.cols <= 0 || term.rows <= 0) return;
  // Setting the font size measures the font and sizes the grid at once,
  // so the grid's size can be read right after.
  const fits = (size: number) => {
    term.options.fontSize = size;
    const r = screen.getBoundingClientRect();
    return r.width <= w && r.height <= h;
  };
  const clamp = (size: number) => Math.min(maxFont, Math.max(minFont, size));
  // The grid grows about in proportion to the font: start the search near
  // the size that proportion gives.
  const now = screen.getBoundingClientRect();
  const from = term.options.fontSize ?? 15;
  const guess = now.width > 0 && now.height > 0 ? clamp(from * Math.min(w / now.width, h / now.height)) : from;
  let lo = clamp(guess * 0.95);
  let hi = clamp(guess * 1.05);
  while (lo > minFont && !fits(lo)) [hi, lo] = [lo, clamp(lo * 0.8)];
  if (!fits(lo)) return; // even the smallest font does not fit: it scrolls
  while (hi < maxFont && fits(hi)) [lo, hi] = [hi, clamp(hi * 1.25)];
  if (fits(hi)) return; // the largest font fits
  // Halve until a step changes the grid by less than half a cell.
  const enough = 0.5 / Math.max(term.cols, term.rows);
  while ((hi - lo) / lo > enough) {
    const mid = (lo + hi) / 2;
    if (fits(mid)) lo = mid;
    else hi = mid;
  }
  term.options.fontSize = lo;
}

// fitted keeps term fitted to box while box changes size or the screen's
// pixel density changes, until stop is called. refit fits it again at
// once, for a change of the terminal's own columns or rows.
export function fitted(term: Terminal, box: HTMLElement): { refit: () => void; stop: () => void } {
  const refit = () => fit(term, box);
  const observer = new ResizeObserver(refit);
  observer.observe(box);
  // A window moved to a screen of another density keeps its CSS size, so
  // the observer does not see it. xterm.js sizes its cells for the new
  // density on the same change, so the fit waits for a frame.
  let density: MediaQueryList | null = null;
  let frame = 0;
  const densityChanged = () => {
    watchDensity();
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(refit);
  };
  const watchDensity = () => {
    density?.removeEventListener("change", densityChanged);
    density = matchMedia(`(resolution: ${devicePixelRatio}dppx)`);
    density.addEventListener("change", densityChanged);
  };
  watchDensity();
  refit();
  return {
    refit,
    stop: () => {
      observer.disconnect();
      density?.removeEventListener("change", densityChanged);
      cancelAnimationFrame(frame);
    },
  };
}

// sized notes term's columns and rows on its element, for tests to read.
export function sized(term: Terminal): void {
  const el = term.element;
  if (!el) return;
  el.dataset.cols = String(term.cols);
  el.dataset.rows = String(term.rows);
}
