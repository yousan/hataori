import { useState } from "react";
import { t } from "./catalog";
import { useDashboard, useNow } from "./live";
import { StatusBar } from "./StatusBar";
import { type Theme, useTheme } from "./theme";
import { type Density, Tree } from "./Tree";

const densityKey = "hataori.density";

function savedDensity(): Density {
  try {
    return localStorage.getItem(densityKey) === "normal" ? "normal" : "dense";
  } catch {
    return "dense";
  }
}

// App is the page: the tree on the left, the main area, the status bar.
// The tabs, panes and details panel come with their own issues.
export function App() {
  const [theme, setTheme] = useTheme();
  const next: Theme = theme === "dark" ? "light" : "dark";
  const d = useDashboard();
  const now = useNow(15000);
  const [filter, setFilter] = useState("");
  const [density, setDensity] = useState<Density>(savedDensity);
  const [selected, setSelected] = useState<number | null>(null);

  const pickDensity = (x: Density) => {
    setDensity(x);
    try {
      localStorage.setItem(densityKey, x);
    } catch {
      // Not kept; the page still switches.
    }
  };

  return (
    <div className="shell">
      <header className="bar">
        <h1 className="title">{t("app.title")}</h1>
        <button
          type="button"
          className="theme"
          title={t(next === "dark" ? "theme.toDark" : "theme.toLight")}
          aria-label={t(next === "dark" ? "theme.toDark" : "theme.toLight")}
          onClick={() => setTheme(next)}
        >
          {t(next === "dark" ? "theme.dark" : "theme.light")}
        </button>
      </header>
      {d.signedOut && (
        <p className="signed-out" role="alert">
          {t("app.signedOut")}
        </p>
      )}
      <div className="body">
        <Tree
          threads={d.threads}
          attention={d.attention}
          now={now}
          filter={filter}
          onFilter={setFilter}
          density={density}
          onDensity={pickDensity}
          selected={selected}
          onSelect={setSelected}
        />
        <main className="main">
          <p className="empty">{t("shell.empty")}</p>
        </main>
      </div>
      <StatusBar selected={d.threads?.find((x) => x.number === selected)} live={d.live} />
    </div>
  );
}
