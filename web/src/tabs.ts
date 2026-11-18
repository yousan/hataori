// The tabs of the main area and which panes show them (design, section
// 7.2). "All threads" is pinned first and cannot be closed; the others are
// threads opened from the tree. The panes show the active tab and, when
// split, the tabs viewed before it.

export type TabKey = "all" | `thread:${number}`;

export const allTab: TabKey = "all";

export function threadTab(n: number): TabKey {
  return `thread:${n}`;
}

// threadOf is the thread number of a thread's tab, or null for "All threads".
export function threadOf(k: TabKey): number | null {
  return k === allTab ? null : Number(k.slice("thread:".length));
}

export type Split = 1 | 2 | 3;

export interface Tabs {
  open: readonly TabKey[]; // in the tab bar's order, "All threads" first
  viewed: readonly TabKey[]; // the open tabs, the active one first, then by when last viewed
}

export const initialTabs: Tabs = { open: [allTab], viewed: [allTab] };

export function active(s: Tabs): TabKey {
  return s.viewed[0];
}

// view makes k the active tab, opening it at the end of the bar if needed.
export function view(s: Tabs, k: TabKey): Tabs {
  return {
    open: s.open.includes(k) ? s.open : [...s.open, k],
    viewed: [k, ...s.viewed.filter((x) => x !== k)],
  };
}

// close closes k. "All threads" stays. When k was active, the tab viewed
// before it becomes active.
export function close(s: Tabs, k: TabKey): Tabs {
  if (k === allTab) return s;
  return { open: s.open.filter((x) => x !== k), viewed: s.viewed.filter((x) => x !== k) };
}

// panes is what each of n panes shows: the active tab, then the tabs viewed
// before it; null for a pane with nothing viewed before.
export function panes(s: Tabs, n: Split): (TabKey | null)[] {
  return Array.from({ length: n }, (_, i) => s.viewed[i] ?? null);
}

export function nextSplit(n: Split): Split {
  return n === 3 ? 1 : ((n + 1) as Split);
}
