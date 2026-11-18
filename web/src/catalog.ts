// Every string the page shows is here, and only here (design, section 7.2).
// scripts/lint-strings.mjs fails on a user-visible string anywhere else.
//
// Another language is one more catalog of the type Catalog: the type makes
// it name every key that English has, and nothing else. A string may name
// values in braces, such as {n}; t fills them in.

const en = {
  "app.title": "hataori dashboard",
  "app.readOnly": "read-only",
  "app.signedOut": "Not signed in, or the session has ended. Open a new sign-in link.",
  "shell.empty": "Nothing to show yet.",
  "theme.toLight": "Switch to the light theme",
  "theme.toDark": "Switch to the dark theme",
  "theme.light": "Light",
  "theme.dark": "Dark",

  "tree.label": "Threads",
  "tree.filter": "Filter by title, number or terminal",
  "tree.attention": "Needs attention",
  "tree.threads": "Threads",
  "tree.none": "No threads.",
  "tree.noMatch": "No thread matches the filter.",
  "tree.density": "Row density",
  "tree.dense": "Dense",
  "tree.normal": "Normal",
  "tree.counts": "Threads per state",

  "thread.number": "#{n}",
  "thread.context": "{pct}% context",
  "thread.noAgent": "•",
  "thread.question": "Question: {text}",
  "thread.questionHidden": "Waiting on a question",
  "thread.approval": "Waiting on an approval",
  "thread.approvalText": "Approval: {text}",
  "thread.failure": "Failed: {reason}",
  "thread.failureUnknown": "Failed",

  "age.seconds": "{n}s",
  "age.minutes": "{n}m",
  "age.hours": "{n}h",
  "age.days": "{n}d",

  "state.creating": "creating",
  "state.idle": "idle",
  "state.running": "running",
  "state.waiting": "waiting",
  "state.asleep": "asleep",
  "state.closed": "closed",
  "state.removed": "removed",
  "state.failed": "failed",
  "state.count": "{state} {n}",

  "tabs.label": "Open tabs",
  "tabs.all": "All threads",
  "tabs.close": "Close tab {name}",
  "tabs.closeMark": "×",
  "tabs.question": "question",
  "tabs.failed": "failed",
  "tabs.tree": "Tree",
  "tabs.showTree": "Show the tree",
  "tabs.hideTree": "Hide the tree",
  "tabs.split": "Split {n}",
  "tabs.splitNext": "Show {n} panes side by side",
  "tabs.details": "Details",
  "tabs.showDetails": "Show the details",
  "tabs.hideDetails": "Hide the details",

  "pane.thread": "#{n} {title}",
  "pane.empty": "Nothing viewed before.",
  "pane.noProcess": "no process",
  "events.textHidden": "(text)",
  "events.toolStarted": "tool {tool}",
  "events.toolEnded": "tool {tool} done",
  "events.turnStarted": "turn started",
  "events.turnEnded": "turn ended: {outcome}",
  "events.turnEndedWhy": "turn ended: {outcome} ({reason})",
  "pane.later": "This pane's content comes later.",
  "details.label": "Details",
  "details.later": "Details come later.",

  "status.label": "Status",
  "status.noSelection": "No thread selected",
  "status.live": "live · event {pos}",
  "status.liveStarting": "live",
  "status.offline": "offline · last event {pos}",
  "status.offlineNone": "offline",

  "limits.label": "Remaining usage per agent",
  "limits.window": "{window} {used}% used",
  "limits.resets": "· resets {time}",
  "limits.asOf": "as of {time}",
  "limits.hours": "{n}h",
  "limits.days": "{n}d",
} as const;

export type Key = keyof typeof en;
export type Catalog = { readonly [K in Key]: string };

// lang is the language the page is shown in, and catalog its strings.
export const lang = "en";
const catalog: Catalog = en;

// t is the string of key in the page's language, with each {name} replaced
// by vars[name].
export function t(key: Key, vars?: { readonly [name: string]: string | number }): string {
  const s = catalog[key];
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, name: string) => (name in vars ? String(vars[name]) : m));
}
