// Fitting a terminal to its pane. While a person only views it, the
// session's terminal keeps its own columns and rows: only the font the page
// draws them in changes, so the grid fills the pane's width or its height,
// whichever it reaches first, and nothing is sent to the session. While the
// owner types into it, the page gives the session the pane's size in whole
// cells at the page's own font size (paneCells), and the grid that comes
// back is fitted the same way, so it fills the pane.
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

// pageFont is the page's own font size for a terminal, in CSS pixels:
// xterm.js's default, and the narrow layout's.
export const pageFont = 15;

// The sizes the page may give a session's terminal, as the server takes
// them (internal/dashboard, attach.go); a pane outside them is drawn
// scaled, as when viewing.
export const minCols = 40;
export const maxCols = 500;
export const minRows = 10;
export const maxRows = 200;

// paneCells is how many whole cells of term's font at pageFont fit in box:
// the size the session's terminal takes while the owner types. It is null
// when the terminal is not drawn yet, or the size is out of bounds.
export function paneCells(term: Terminal, box: HTMLElement): { cols: number; rows: number } | null {
  const screen = term.element?.querySelector<HTMLElement>(".xterm-screen");
  const overflow = box.style.overflow;
  box.style.overflow = "hidden"; // the box's size without scrollbars, as fit measures it
  const w = box.clientWidth;
  const h = box.clientHeight;
  box.style.overflow = overflow;
  if (!screen || w <= 0 || h <= 0 || term.cols <= 0 || term.rows <= 0) return null;
  // Setting the font size measures the font and sizes the grid at once.
  const now = term.options.fontSize;
  term.options.fontSize = pageFont;
  const r = screen.getBoundingClientRect();
  term.options.fontSize = now;
  if (r.width <= 0 || r.height <= 0) return null;
  const cols = Math.floor(w / (r.width / term.cols));
  const rows = Math.floor(h / (r.height / term.rows));
  if (cols < minCols || cols > maxCols || rows < minRows || rows > maxRows) return null;
  return { cols, rows };
}

// fit sets term's font size to the largest at which its grid fits in box,
// to within a fraction of a cell.
export function fit(term: Terminal, box: HTMLElement): void {
  // Measured without scrollbars: a grid that just took more columns or
  // rows overflows the box until it is fitted, and the scrollbars that
  // gives would make the box seem smaller. They come and go within this
  // call, so no change of the box's size would fit it again.
  const overflow = box.style.overflow;
  box.style.overflow = "hidden";
  try {
    fitWithin(term, box);
  } finally {
    box.style.overflow = overflow;
  }
}

function fitWithin(term: Terminal, box: HTMLElement): void {
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
// once, for a change of the terminal's own columns or rows. onChange, when
// given, is called after each change of box's size.
export function fitted(term: Terminal, box: HTMLElement, onChange?: () => void): { refit: () => void; stop: () => void } {
  const refit = () => fit(term, box);
  const observer = new ResizeObserver(() => {
    refit();
    onChange?.();
  });
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
