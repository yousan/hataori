// What the person chose to show: the tree and the details panel. Kept in
// this browser across reloads.

import { useState } from "react";

export interface Layout {
  tree: boolean;
  details: boolean;
}

const storageKey = "hataori.layout";
const shown: Layout = { tree: true, details: true };

function saved(): Layout {
  try {
    const v = JSON.parse(localStorage.getItem(storageKey) ?? "null");
    if (v && typeof v.tree === "boolean" && typeof v.details === "boolean") return { tree: v.tree, details: v.details };
  } catch {
    // Storage may be unavailable, or hold something else: show both.
  }
  return shown;
}

export function useLayout(): [Layout, (l: Layout) => void] {
  const [layout, setLayout] = useState<Layout>(saved);
  const choose = (l: Layout) => {
    setLayout(l);
    try {
      localStorage.setItem(storageKey, JSON.stringify(l));
    } catch {
      // Not kept; the page still changes.
    }
  };
  return [layout, choose];
}
