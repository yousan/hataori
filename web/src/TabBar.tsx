import { useEffect, useRef } from "react";
import type { HostTerminal, ThreadInfo } from "./api";
import { hostLabel } from "./hostTerminals";
import { t } from "./catalog";
import { age, mark, stateName } from "./format";
import type { Layout } from "./layout";
import { allTab, hostOf, nextSplit, type Split, type TabKey, threadOf } from "./tabs";
import type { Theme } from "./theme";

interface Props {
  open: readonly TabKey[];
  active: TabKey;
  thread: (n: number) => ThreadInfo | undefined;
  host: (device: string) => HostTerminal | undefined;
  now: number;
  onView: (k: TabKey) => void;
  onClose: (k: TabKey) => void;
  split: Split;
  onSplit: (n: Split) => void;
  layout: Layout;
  onLayout: (l: Layout) => void;
  theme: Theme;
  onTheme: (t: Theme) => void;
}

// badge is what a thread's tab flags: an open question or a failure.
function badge(x: ThreadInfo | undefined): "question" | "failed" | null {
  if (x?.state === "waiting") return "question";
  if (x?.state === "failed") return "failed";
  return null;
}

// tabID is the element id of k's tab, which its pane names.
export function tabID(k: TabKey): string {
  return `tab-${k.replace(":", "-")}`;
}

// A host terminal's tab has no mark, no elapsed time and no badge:
// hataori judges no state of it.
function Tab(props: {
  k: TabKey;
  x: ThreadInfo | undefined;
  h: HostTerminal | undefined;
  active: boolean;
  now: number;
  onView: (k: TabKey) => void;
  onClose: (k: TabKey) => void;
  onKey: (e: React.KeyboardEvent, k: TabKey) => void;
}) {
  const { k, x, h, active, now, onView, onClose, onKey } = props;
  const n = threadOf(k);
  const device = hostOf(k);
  const name =
    n !== null ? (x?.title || t("thread.number", { n })) : device !== null ? (h ? hostLabel(h) : device) : t("tabs.all");
  const b = badge(x);
  return (
    <div className={active ? "tab active" : "tab"} data-tab={k} role="presentation">
      <button
        type="button"
        role="tab"
        id={tabID(k)}
        className="tab-main"
        aria-selected={active}
        tabIndex={active ? 0 : -1}
        onClick={() => onView(k)}
        onKeyDown={(e) => onKey(e, k)}
      >
        {n !== null && x && (
          <span className={`agent-mark state-${x.state}`} title={stateName(x.state)}>
            {mark(x)}
          </span>
        )}
        <span className="tab-title">{name}</span>
        {x?.state === "running" && <span className="elapsed">{age(x.updated, now)}</span>}
        {b && (
          <span className={`badge badge-${b}`} data-badge={b}>
            {t(b === "question" ? "tabs.question" : "tabs.failed")}
          </span>
        )}
      </button>
      {k !== allTab && (
        <button type="button" className="tab-close" tabIndex={active ? 0 : -1} aria-label={t("tabs.close", { name })} title={t("tabs.close", { name })} onClick={() => onClose(k)}>
          {t("tabs.closeMark")}
        </button>
      )}
    </div>
  );
}

export function TabBar(props: Props) {
  const { open, active, thread, host, now, onView, onClose, split, onSplit, layout, onLayout, theme, onTheme } = props;
  const next: Theme = theme === "dark" ? "light" : "dark";
  const bar = useRef<HTMLDivElement>(null);
  const focusing = useRef(false); // the active tab is to take the focus once it shows

  // After the keyboard moved or closed a tab, the active tab takes the focus.
  useEffect(() => {
    if (!focusing.current) return;
    focusing.current = false;
    bar.current?.querySelector<HTMLElement>(".tab-main[aria-selected=true]")?.focus();
  }, [active, open]);

  // onKey moves along the tabs with the arrow keys, Home and End, as a tab
  // list does, and closes the tab with Delete.
  const onKey = (e: React.KeyboardEvent, k: TabKey) => {
    const i = open.indexOf(k);
    let to: TabKey | undefined;
    switch (e.key) {
      case "ArrowRight":
        to = open[(i + 1) % open.length];
        break;
      case "ArrowLeft":
        to = open[(i - 1 + open.length) % open.length];
        break;
      case "Home":
        to = open[0];
        break;
      case "End":
        to = open[open.length - 1];
        break;
      case "Delete":
        if (k !== allTab) {
          e.preventDefault();
          focusing.current = true;
          onClose(k);
        }
        return;
      default:
        return;
    }
    e.preventDefault();
    focusing.current = true;
    onView(to);
  };
  const closeFocused = (k: TabKey) => {
    focusing.current = true;
    onClose(k);
  };

  return (
    <div className="tabbar">
      <div className="tabs" role="tablist" aria-label={t("tabs.label")} ref={bar}>
        {open.map((k) => {
          const n = threadOf(k);
          const device = hostOf(k);
          return (
            <Tab
              key={k}
              k={k}
              x={n === null ? undefined : thread(n)}
              h={device === null ? undefined : host(device)}
              active={k === active}
              now={now}
              onView={onView}
              onClose={closeFocused}
              onKey={onKey}
            />
          );
        })}
      </div>
      <div className="tools">
        <button
          type="button"
          data-tool="tree"
          aria-pressed={layout.tree}
          title={t(layout.tree ? "tabs.hideTree" : "tabs.showTree")}
          onClick={() => onLayout({ ...layout, tree: !layout.tree })}
        >
          {t("tabs.tree")}
        </button>
        <button type="button" data-tool="split" data-split={split} title={t("tabs.splitNext", { n: nextSplit(split) })} onClick={() => onSplit(nextSplit(split))}>
          {t("tabs.split", { n: split })}
        </button>
        <button
          type="button"
          data-tool="details"
          aria-pressed={layout.details}
          title={t(layout.details ? "tabs.hideDetails" : "tabs.showDetails")}
          onClick={() => onLayout({ ...layout, details: !layout.details })}
        >
          {t("tabs.details")}
        </button>
        <button
          type="button"
          className="theme"
          data-tool="theme"
          title={t(next === "dark" ? "theme.toDark" : "theme.toLight")}
          aria-label={t(next === "dark" ? "theme.toDark" : "theme.toLight")}
          onClick={() => onTheme(next)}
        >
          {t(next === "dark" ? "theme.dark" : "theme.light")}
        </button>
      </div>
    </div>
  );
}
