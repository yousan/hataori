import { t } from "./catalog";
import { type Theme, useTheme } from "./theme";

// App is the page's empty shell. The panels come with their own issues.
export function App() {
  const [theme, setTheme] = useTheme();
  const next: Theme = theme === "dark" ? "light" : "dark";

  return (
    <div className="shell">
      <header className="bar">
        <h1 className="title">{t("app.title")}</h1>
        <span className="mark">{t("app.readOnly")}</span>
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
      <main className="main">
        <p className="empty">{t("shell.empty")}</p>
      </main>
    </div>
  );
}
