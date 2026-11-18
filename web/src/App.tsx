import { useState } from "react";
import { t } from "./catalog";
import { useLayout } from "./layout";
import { useDashboard, useNow } from "./live";
import { StatusBar } from "./StatusBar";
import { TabBar, tabID } from "./TabBar";
import { ThreadPane } from "./ThreadPane";
import { active, close, initialTabs, panes, type Split, type TabKey, type Tabs, threadOf, threadTab, view } from "./tabs";
import { useTheme } from "./theme";
import { type Density, Tree } from "./Tree";

const densityKey = "hataori.density";

function savedDensity(): Density {
  try {
    return localStorage.getItem(densityKey) === "normal" ? "normal" : "dense";
  } catch {
    return "dense";
  }
}

// App is the page: the tree on the left, the tab bar over the panes, the
// details panel on the right, the status bar at the bottom. What a pane and
// the details panel show comes with their own issues.
export function App() {
  const [theme, setTheme] = useTheme();
  const d = useDashboard();
  const now = useNow(1000);
  const [filter, setFilter] = useState("");
  const [density, setDensity] = useState<Density>(savedDensity);
  const [tabs, setTabs] = useState<Tabs>(initialTabs);
  const [split, setSplit] = useState<Split>(1);
  const [layout, setLayout] = useLayout();

  const pickDensity = (x: Density) => {
    setDensity(x);
    try {
      localStorage.setItem(densityKey, x);
    } catch {
      // Not kept; the page still switches.
    }
  };
  const thread = (n: number) => d.threads?.find((x) => x.number === n);
  const current = threadOf(active(tabs));

  const paneThread = (k: TabKey) => {
    const n = threadOf(k);
    return n === null ? undefined : thread(n);
  };
  const paneName = (k: TabKey) => {
    const n = threadOf(k);
    if (n === null) return t("tabs.all");
    const x = thread(n);
    return x ? t("pane.thread", { n, title: x.title }) : t("thread.number", { n });
  };

  return (
    <div className="shell">
      {d.signedOut && (
        <p className="signed-out" role="alert">
          {t("app.signedOut")}
        </p>
      )}
      <div className="body">
        {layout.tree && (
          <Tree
            threads={d.threads}
            attention={d.attention}
            now={now}
            filter={filter}
            onFilter={setFilter}
            density={density}
            onDensity={pickDensity}
            selected={current}
            onSelect={(n) => setTabs((s) => view(s, threadTab(n)))}
          />
        )}
        <div className="center">
          <TabBar
            open={tabs.open}
            active={active(tabs)}
            thread={thread}
            now={now}
            onView={(k) => setTabs((s) => view(s, k))}
            onClose={(k) => setTabs((s) => close(s, k))}
            split={split}
            onSplit={setSplit}
            layout={layout}
            onLayout={setLayout}
            theme={theme}
            onTheme={setTheme}
          />
          <main className="panes" data-split={split}>
            {panes(tabs, split).map((k, i) => (
              <section
                key={i}
                className={i === 0 ? "pane active" : "pane"}
                data-pane={i}
                data-tab={k ?? ""}
                role={i === 0 ? "tabpanel" : undefined}
                aria-labelledby={i === 0 && k ? tabID(k) : undefined}
              >
                {k ? (
                  <>
                    <h2 className="pane-name">{paneName(k)}</h2>
                    {paneThread(k) ? (
                      <ThreadPane key={k} x={paneThread(k)!} subscribe={d.subscribe} />
                    ) : (
                      <p className="empty">{t("pane.later")}</p>
                    )}
                  </>
                ) : (
                  <p className="empty">{t("pane.empty")}</p>
                )}
              </section>
            ))}
          </main>
        </div>
        {layout.details && (
          <aside className="details" aria-label={t("details.label")}>
            <p className="empty">{t("details.later")}</p>
          </aside>
        )}
      </div>
      <StatusBar selected={current === null ? undefined : thread(current)} live={d.live} />
    </div>
  );
}

