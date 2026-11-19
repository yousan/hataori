// Which nodes of the tree are folded, kept in this browser across reloads:
// a scope's node, by "scope:<key>", and the host terminals' node, "host".
// A scope starts unfolded; the host terminals start folded.

import { useState } from "react";

const storageKey = "hataori.tree.folds";

export const hostNode = "host";

export function scopeNode(scope: string): string {
  return `scope:${scope}`;
}

function saved(): Record<string, boolean> {
  try {
    const v = JSON.parse(localStorage.getItem(storageKey) ?? "null");
    if (v && typeof v === "object" && !Array.isArray(v)) {
      return Object.fromEntries(Object.entries(v).filter(([, f]) => typeof f === "boolean")) as Record<string, boolean>;
    }
  } catch {
    // Storage may be unavailable, or hold something else: the defaults.
  }
  return {};
}

export function useFolds(): [(node: string) => boolean, (node: string, folded: boolean) => void] {
  const [folds, setFolds] = useState<Record<string, boolean>>(saved);
  const folded = (node: string) => folds[node] ?? node === hostNode;
  const setFolded = (node: string, f: boolean) => {
    setFolds((have) => {
      const next = { ...have, [node]: f };
      try {
        localStorage.setItem(storageKey, JSON.stringify(next));
      } catch {
        // Not kept; the tree still folds.
      }
      return next;
    });
  };
  return [folded, setFolded];
}
