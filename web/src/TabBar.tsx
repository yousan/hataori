import { threadStateName, visibleState } from "./machine";
import { useEffect, useRef, useState } from "react";
import type { HostTerminal, ThreadInfo } from "./api";
import { hostLabel } from "./hostTerminals";
import { clock, left, type OpState } from "./operate";
import type { AttachState } from "./attach";
import { t } from "./catalog";
import { age, displayTitle, fullTitle, mark } from "./format";
import type { Layout } from "./layout";
import { type Show, ShowTool } from "./show";
import { ToolIcon } from "./ToolIcon";
import { allTab, hostOf, nextSplit, type Split, type TabKey, threadOf } from "./tabs";
import type { Theme } from "./theme";

interface Props {
  open: readonly TabKey[];
  active: TabKey;
  thread: (n: number) => ThreadInfo | undefined;
  host: (device: string) => HostTerminal | undefined;
  // op is the operation of a host pane's tab, if any.
  op: (k: TabKey) => OpState | undefined;
  att: (k: TabKey) => AttachState | undefined; // a thread's terminal attached
  now: number;
  onView: (k: TabKey) => void;
  onClose: (k: TabKey) => void;
  split: Split;
  onSplit: (n: Split) => void;
  layout: Layout;
  onLayout: (l: Layout) => void;
  theme: Theme;
  onTheme: (t: Theme) => void;
  // The choice between the active threads and all of them, for the tree and
  // the All threads table, how many threads it hides, and how many idle
  // shell panes, counted apart.
  show: Show;
  onShow: (s: Show) => void;
  hidden: number;
  hiddenHosts: number;
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

function tabName(k: TabKey, x: ThreadInfo | undefined, h: HostTerminal | undefined): string {
  const n = threadOf(k);
  const device = hostOf(k);
  return n !== null ? ((x && displayTitle(x)) || t("thread.number", { n })) : device !== null ? (h ? hostLabel(h) : device) : t("tabs.all");
}

// A host terminal's tab has no mark, no elapsed time and no badge:
// hataori judges no state of it.
function Tab(props: {
  k: TabKey;
  x: ThreadInfo | undefined;
  h: HostTerminal | undefined;
  op: OpState | undefined;
  att: AttachState | undefined;
  active: boolean;
  now: number;
  onView: (k: TabKey) => void;
  onClose: (k: TabKey) => void;
  onKey: (e: React.KeyboardEvent, k: TabKey) => void;
}) {
  const { k, x, h, op, att, active, now, onView, onClose, onKey } = props;
  const n = threadOf(k);
  const name = tabName(k, x, h);
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
          <span className={`agent-mark state-${visibleState(x)}`} title={threadStateName(x)}>
            {mark(x)}
          </span>
        )}
        <span className="tab-title" title={(x && fullTitle(x)) || undefined}>
          {name}
        </span>
        {x && visibleState(x) === "running" && <span className="elapsed">{age(x.updated, now)}</span>}
        {b && (
          <span className={`badge badge-${b}`} data-badge={b}>
            {t(b === "question" ? "tabs.question" : "tabs.failed")}
          </span>
        )}
        {att?.phase === "on" && (
          <span className="tab-operating" data-tab-attached={att.thread} title={t("attach.tabTitle")}>
            {t("attach.tab")}
          </span>
        )}
        {op?.phase === "on" && (
          <span className="tab-operating" data-tab-operating={op.pane} title={t("operate.tabTitle", { pane: op.pane })}>
            {t("operate.tab", { left: clock(left(op, now)) })}
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
  const { open, active, thread, host, op, att, now, onView, onClose, split, onSplit, layout, onLayout, theme, onTheme, show, onShow, hidden, hiddenHosts } = props;
  const next: Theme = theme === "dark" ? "light" : "dark";
  const bar = useRef<HTMLDivElement>(null);
  const focusing = useRef(false); // the active tab is to take the focus once it shows
  const [listed, setListed] = useState(false);
  const picker = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const listButton = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!listed) return;
    list.current?.querySelector<HTMLElement>('[aria-checked="true"]')?.focus();
    const outside = (e: PointerEvent) => {
      if (e.target instanceof Node && !picker.current?.contains(e.target)) setListed(false);
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [listed]);

  const listKey = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      setListed(false);
      listButton.current?.focus();
      return;
    }
    const items = [...(list.current?.querySelectorAll<HTMLButtonElement>('[role="menuitemradio"]') ?? [])];
    const at = items.indexOf(document.activeElement as HTMLButtonElement);
    const to = e.key === "Home" ? 0 : e.key === "End" ? items.length - 1 : e.key === "ArrowDown" ? (at + 1) % items.length : e.key === "ArrowUp" ? (at - 1 + items.length) % items.length : null;
    if (to === null) return;
    e.preventDefault();
    items[to]?.focus();
  };

  // After the keyboard moved or closed a tab, the active tab takes the focus.
  useEffect(() => {
    if (!focusing.current) return;
    focusing.current = false;
    bar.current?.querySelector<HTMLElement>(".tab-main[aria-selected=true]")?.focus();
  }, [active, open]);

  // The active tab is kept in view in a bar too narrow for every tab: when
  // another one becomes active, and when the bar or any tab changes its
  // size, unless the person scrolled it away. The rest are reached by
  // scrolling the bar, or with the keys.
  useEffect(() => {
    const el = bar.current;
    const tab = el?.querySelector<HTMLElement>(".tab.active");
    if (!el || !tab) return;
    // Whether the person left the tab in view, by where it and the bar were
    // when the observer last told: a scroll's event comes later, when a tab
    // may have grown already that the observer has yet to tell of.
    let shown = true;
    let seen = { left: 0, width: 0, room: 0 };
    const reveal = () => {
      const [b, r] = [el.getBoundingClientRect(), tab.getBoundingClientRect()];
      seen = { left: r.left - b.left + el.scrollLeft, width: r.width, room: b.width };
      if (!shown) return;
      if (r.left < b.left) el.scrollLeft -= b.left - r.left;
      else if (r.right > b.right) el.scrollLeft += Math.min(r.right - b.right, r.left - b.left);
    };
    const onScroll = () => {
      const left = seen.left - el.scrollLeft;
      shown = Math.min(left + seen.width, seen.room) - Math.max(left, 0) >= Math.min(seen.width, seen.room) - 1;
    };
    reveal();
    const watch = new ResizeObserver(reveal);
    watch.observe(el);
    for (const e of el.querySelectorAll(".tab")) watch.observe(e);
    el.addEventListener("scroll", onScroll);
    return () => {
      watch.disconnect();
      el.removeEventListener("scroll", onScroll);
    };
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
              op={op(k)}
              att={att(k)}
              active={k === active}
              now={now}
              onView={onView}
              onClose={closeFocused}
              onKey={onKey}
            />
          );
        })}
      </div>
      <div className="tab-picker" ref={picker} onBlur={(e) => { if (!e.currentTarget.contains(e.relatedTarget)) setListed(false); }}>
        <button type="button" className="icon-tool" ref={listButton} data-tool="tab-list" title={t("tabs.list")} aria-label={t("tabs.list")} aria-haspopup="menu" aria-expanded={listed} aria-controls="open-tab-menu" onClick={() => setListed(!listed)} onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") { e.preventDefault(); setListed(true); }
        }}><ToolIcon name="list" /></button>
        {listed && <div className="tab-menu" id="open-tab-menu" role="menu" aria-label={t("tabs.label")} ref={list} onKeyDown={listKey}>
          {open.map((k) => {
            const n = threadOf(k);
            const device = hostOf(k);
            const x = n === null ? undefined : thread(n);
            const name = tabName(k, x, device === null ? undefined : host(device));
            return <button key={k} type="button" role="menuitemradio" aria-checked={k === active} tabIndex={-1} title={name} onClick={() => {
              setListed(false);
              focusing.current = k !== active;
              onView(k);
              // Choosing the already active tab does not change active.
              if (k === active) bar.current?.querySelector<HTMLElement>('.tab-main[aria-selected="true"]')?.focus();
            }}>{name}</button>;
          })}
        </div>}
      </div>
      <div className="tools">
        <ShowTool show={show} onShow={onShow} hidden={hidden} terminals={hiddenHosts} />
        <button
          type="button"
          data-tool="tree"
          className="icon-tool"
          aria-pressed={layout.tree}
          aria-label={t(layout.tree ? "tabs.hideTree" : "tabs.showTree")}
          title={t(layout.tree ? "tabs.hideTree" : "tabs.showTree")}
          onClick={() => onLayout({ ...layout, tree: !layout.tree })}
        >
          <ToolIcon name="tree" />
        </button>
        <button type="button" className="icon-tool split-tool" data-tool="split" data-split={split} title={t("tabs.splitNext", { n: nextSplit(split) })} aria-label={t("tabs.splitNext", { n: nextSplit(split) })} onClick={() => onSplit(nextSplit(split))}>
          <ToolIcon name="split" /><span className="split-count" aria-hidden="true">{t("show.count", { n: split })}</span>
        </button>
        <button
          type="button"
          data-tool="details"
          className="icon-tool"
          aria-pressed={layout.details}
          aria-label={t(layout.details ? "tabs.hideDetails" : "tabs.showDetails")}
          title={t(layout.details ? "tabs.hideDetails" : "tabs.showDetails")}
          onClick={() => onLayout({ ...layout, details: !layout.details })}
        >
          <ToolIcon name="details" />
        </button>
        <button
          type="button"
          className="icon-tool"
          data-tool="theme"
          title={t(next === "dark" ? "theme.toDark" : "theme.toLight")}
          aria-label={t(next === "dark" ? "theme.toDark" : "theme.toLight")}
          onClick={() => onTheme(next)}
        >
          <ToolIcon name={next === "dark" ? "moon" : "sun"} />
        </button>
      </div>
    </div>
  );
}
