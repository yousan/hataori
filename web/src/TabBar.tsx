import type { ThreadInfo } from "./api";
import { t } from "./catalog";
import { age, mark, stateName } from "./format";
import type { Layout } from "./layout";
import { allTab, nextSplit, type Split, type TabKey, threadOf } from "./tabs";
import type { Theme } from "./theme";

interface Props {
  open: readonly TabKey[];
  active: TabKey;
  thread: (n: number) => ThreadInfo | undefined;
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

function Tab(props: {
  k: TabKey;
  x: ThreadInfo | undefined;
  active: boolean;
  now: number;
  onView: (k: TabKey) => void;
  onClose: (k: TabKey) => void;
}) {
  const { k, x, active, now, onView, onClose } = props;
  const n = threadOf(k);
  const name = n === null ? t("tabs.all") : x ? x.title : t("thread.number", { n });
  const b = badge(x);
  return (
    <div className={active ? "tab active" : "tab"} data-tab={k} role="presentation">
      <button type="button" role="tab" className="tab-main" aria-selected={active} onClick={() => onView(k)}>
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
        <button type="button" className="tab-close" aria-label={t("tabs.close", { name })} title={t("tabs.close", { name })} onClick={() => onClose(k)}>
          {t("tabs.closeMark")}
        </button>
      )}
    </div>
  );
}

export function TabBar(props: Props) {
  const { open, active, thread, now, onView, onClose, split, onSplit, layout, onLayout, theme, onTheme } = props;
  const next: Theme = theme === "dark" ? "light" : "dark";
  return (
    <div className="tabbar">
      <div className="tabs" role="tablist" aria-label={t("tabs.label")}>
        {open.map((k) => {
          const n = threadOf(k);
          return <Tab key={k} k={k} x={n === null ? undefined : thread(n)} active={k === active} now={now} onView={onView} onClose={onClose} />;
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
