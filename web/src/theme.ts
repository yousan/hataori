// The page has a light and a dark theme. It follows the system until the
// person picks one; the pick is kept in this browser.

export type Theme = "light" | "dark";

const storageKey = "hataori.theme";

export function initialTheme(): Theme {
  try {
    const saved = localStorage.getItem(storageKey);
    if (saved === "light" || saved === "dark") {
      return saved;
    }
  } catch {
    // Storage may be unavailable; follow the system.
  }
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function applyTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(storageKey, theme);
  } catch {
    // Not kept; the page still switches.
  }
}
