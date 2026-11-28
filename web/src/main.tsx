import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { lang, t } from "./catalog";
import { adoptSession } from "./session";
import { desktopSetup } from "./Setup";
import "./styles.css";

document.documentElement.lang = lang;
document.title = t("app.title");

// The session first: everything the page shows asks the server with it.
adoptSession().finally(async () => {
  const root = document.getElementById("root");
  if (root) {
    const setup = location.origin === "hataori-desktop://instance" ? await desktopSetup() : null;
    createRoot(root).render(
      <StrictMode>
        {setup ?? <App />}
      </StrictMode>,
    );
  }
});
