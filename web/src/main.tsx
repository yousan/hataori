import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { lang, t } from "./catalog";
import { adoptSession } from "./session";
import "./styles.css";

document.documentElement.lang = lang;
document.title = t("app.title");

// The session first: everything the page shows asks the server with it.
adoptSession().finally(() => {
  const root = document.getElementById("root");
  if (root) {
    createRoot(root).render(
      <StrictMode>
        <App />
      </StrictMode>,
    );
  }
});
