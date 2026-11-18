// The page has a light and a dark theme. It follows the system until the
// person picks one; only a pick is kept, in this browser.

import { useEffect, useState } from "react";

export type Theme = "light" | "dark";

const storageKey = "hataori.theme";
const darkQuery = "(prefers-color-scheme: dark)";

function picked(): Theme | null {
  try {
    const saved = localStorage.getItem(storageKey);
    if (saved === "light" || saved === "dark") {
      return saved;
    }
  } catch {
    // Storage may be unavailable; follow the system.
  }
  return null;
}

function system(): Theme {
  return matchMedia(darkQuery).matches ? "dark" : "light";
}

// useTheme returns the theme shown and a function that picks one. Until a
// pick, the theme follows the system as it changes.
export function useTheme(): [Theme, (t: Theme) => void] {
  const [pick, setPick] = useState<Theme | null>(picked);
  const [sys, setSys] = useState<Theme>(system);

  useEffect(() => {
    const q = matchMedia(darkQuery);
    const changed = () => setSys(q.matches ? "dark" : "light");
    q.addEventListener("change", changed);
    return () => q.removeEventListener("change", changed);
  }, []);

  useEffect(() => {
    const root = document.documentElement;
    if (pick) {
      root.dataset.theme = pick;
    } else {
      delete root.dataset.theme; // the style sheet follows the system
    }
  }, [pick]);

  const choose = (t: Theme) => {
    setPick(t);
    try {
      localStorage.setItem(storageKey, t);
    } catch {
      // Not kept; the page still switches.
    }
  };
  return [pick ?? sys, choose];
}
