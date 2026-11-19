import type { LimitWindow, State, ThreadInfo } from "./api";
import { type Key, lang, t } from "./catalog";

// stateName is a state as the page shows it.
export function stateName(s: State): string {
  return t(`state.${s}` as Key);
}

// age is how long ago iso was, short: 45s, 12m, 3h, 2d.
export function age(iso: string, now: number): string {
  const s = Math.max(0, Math.floor((now - Date.parse(iso)) / 1000));
  if (s < 60) return t("age.seconds", { n: s });
  if (s < 3600) return t("age.minutes", { n: Math.floor(s / 60) });
  if (s < 86400) return t("age.hours", { n: Math.floor(s / 3600) });
  return t("age.days", { n: Math.floor(s / 86400) });
}

// contextUse is the context use in percent, or "" when not reported.
export function contextUse(x: ThreadInfo): string {
  if (!x.context || x.context.size <= 0) return "";
  return t("thread.context", { pct: Math.round((100 * x.context.used) / x.context.size) });
}

// mark is the agent's mark: the first letter of its name.
export function mark(x: ThreadInfo): string {
  return x.agent ? x.agent.slice(0, 1).toUpperCase() : t("thread.noAgent");
}

// fullTitle is a thread's title as the page names it: the chat thread's
// own name when the chat service reports one, else the first line of its
// first prompt; "" for a caller that may read neither.
export function fullTitle(x: ThreadInfo): string {
  return x.name || x.title || "";
}

// issueURL matches the address of an issue or a pull request: .../<repo>/
// issues/<n> or .../<repo>/pull/<n>, also with "-/" before them and as a
// merge request, with whatever follows the number up to a space.
const issueURL = /\bhttps?:\/\/[^\s/]+\/(?:[^\s/]+\/)*?([^\s/]+)\/(?:-\/)?(?:issues|pulls?|merge_requests)\/(\d+)(?:[/?#]\S*)?/g;

// shortTitle is s with each address of an issue or a pull request written
// short, as repo#N, so that the part that tells threads apart shows in a
// narrow row.
export function shortTitle(s: string): string {
  return s.replace(issueURL, (_m, repo: string, n: string) => t("thread.ref", { repo, n }));
}

// displayTitle is a thread's title as a row, a tab or a cell shows it.
export function displayTitle(x: ThreadInfo): string {
  return shortTitle(fullTitle(x));
}

// matches reports whether x matches the filter q: its title (its chat
// thread's name, its first line, or either written short), its number
// (with or without #) or its terminal. An empty filter matches every thread.
export function matches(x: ThreadInfo, q: string): boolean {
  const f = q.trim().toLowerCase();
  if (f === "") return true;
  for (const s of [x.name ?? "", x.title ?? "", displayTitle(x)]) if (s.toLowerCase().includes(f)) return true;
  if (String(x.id) === f.replace(/^#/, "")) return true;
  return !!x.terminal && x.terminal.toLowerCase().includes(f);
}

// windowName is a usage window as the page shows it: its length when the
// agent gives one (5h, 7d), else the agent's name for it.
export function windowName(w: LimitWindow): string {
  const m = w.minutes ?? 0;
  if (m > 0 && m % 1440 === 0) return t("limits.days", { n: m / 1440 });
  if (m > 0 && m % 60 === 0) return t("limits.hours", { n: m / 60 });
  return w.name;
}

// when is a time as the status bar shows it: the day and the time of day,
// in the browser's time zone.
export function when(iso: string): string {
  return new Date(iso).toLocaleString(lang, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
