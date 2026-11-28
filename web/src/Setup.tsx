import { useState, type FormEvent, type ReactNode } from "react";
import { t } from "./catalog";
import { readDesktopSetup, type SetupInfo } from "./desktop-setup";

// Only the fixed native origin asks for setup. The ordinary native identity
// omits setup; a restricted first-run server explicitly supplies true.
export async function desktopSetup(): Promise<ReactNode | null> {
  try {
    const info = await readDesktopSetup();
    return info === null ? null : <Setup info={info} />;
  } catch {
    return <main><h1>{t("desktop.setup.title")}</h1><p role="alert">{t("desktop.setup.failed")}</p></main>;
  }
}

function Setup({ info }: { info: SetupInfo }) {
  const [owner, setOwner] = useState("");
  const [ownerId, setOwnerId] = useState("");
  const [botId, setBotId] = useState("");
  const [token, setToken] = useState("");
  const [scope, setScope] = useState("");
  const [repository, setRepository] = useState("");
  const [agent, setAgent] = useState(info.agents[0]?.name ?? "");
  const [login, setLogin] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const [saved, setSaved] = useState(false);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (busy || saved || !login) return;
    setBusy(true);
    setFailed(false);
    try {
      const response = await fetch("/api/setup", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ owner, owner_id: ownerId, bot_id: botId, token, scope, repository, agent, start_at_login: login }),
      });
      if (!response.ok || (await response.json() as { complete?: boolean }).complete !== true) throw new Error("Setup refused");
      setToken("");
      setSaved(true);
    } catch { setFailed(true); }
    finally { setBusy(false); }
  };

  return <main>
    <h1>{t("desktop.setup.title")}</h1>
    <p>{t("desktop.setup.explanation")}</p>
    {info.existing ? <p>{t("desktop.setup.existing")}</p> : <form onSubmit={submit} autoComplete="off">
      <p><label>{t("desktop.setup.owner")}<input required value={owner} onChange={e => setOwner(e.target.value)} /></label></p>
      <p><label>{t("desktop.setup.ownerId")}<input required value={ownerId} onChange={e => setOwnerId(e.target.value)} /></label></p>
      <p><label>{t("desktop.setup.botId")}<input required value={botId} onChange={e => setBotId(e.target.value)} /></label></p>
      <p><label>{t("desktop.setup.token")}<input required type="password" spellCheck={false} value={token} onChange={e => setToken(e.target.value)} /></label></p>
      <p><label>{t("desktop.setup.scope")}<input required value={scope} onChange={e => setScope(e.target.value)} /></label></p>
      <p><label>{t("desktop.setup.repository")}<input required value={repository} onChange={e => setRepository(e.target.value)} /></label></p>
      <p><label>{t("desktop.setup.agent")}<select value={agent} onChange={e => setAgent(e.target.value)}>
        {info.agents.map(a => <option key={a.name} value={a.name}>{a.name}</option>)}
      </select></label></p>
      {info.agents.length === 0 && <p role="alert">{t("desktop.setup.noAgent")}</p>}
      <p><label><input type="checkbox" required checked={login} onChange={e => setLogin(e.target.checked)} />{t("desktop.setup.login")}</label></p>
      <button disabled={busy || saved || !login || !agent}>{t(busy ? "desktop.setup.working" : "desktop.setup.submit")}</button>
    </form>}
    {info.existing && <form onSubmit={submit}>
      <p><label><input type="checkbox" required checked={login} onChange={e => setLogin(e.target.checked)} />{t("desktop.setup.login")}</label></p>
      <button disabled={busy || saved || !login}>{t(busy ? "desktop.setup.working" : "desktop.setup.submit")}</button>
    </form>}
    {failed && <p role="alert">{t("desktop.setup.failed")}</p>}
    {saved && <p role="status">{t("desktop.setup.saved")}</p>}
  </main>;
}
