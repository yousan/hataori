import type { AgentLimits, ClientState, ThreadInfo } from "./api";
import { t } from "./catalog";
import { contextUse, stateName, when, windowName } from "./format";
import type { Live } from "./live";
import { Passkeys } from "./Passkeys";
import { Trusts } from "./Trusts";
import { clock, left, type OpState } from "./operate";

interface Props {
  selected: ThreadInfo | undefined;
  live: Live;
  limits: AgentLimits[];
  clients: ClientState[];
  now: number;
  signedIn: boolean;
  // operating is the operation of a host pane under way, if any.
  operating?: OpState;
  // attached is the thread whose terminal is attached, if any (attach.ts).
  attached?: number;
}

// Limits shows each agent's remaining usage with its reset time. An agent
// with no entry, or with no window, shows nothing. A report older than its
// agent's own update interval is not current: it is shown with its time.
function Limits({ limits, now }: { limits: AgentLimits[]; now: number }) {
  const shown = limits.filter((l) => l.windows.length > 0);
  if (shown.length === 0) return null;
  return (
    <span className="limits" aria-label={t("limits.label")}>
      {shown.map((l) => {
        const stale = now - Date.parse(l.at) > l.every * 1000;
        return (
          <span key={l.agent} className="limit" data-agent={l.agent} data-stale={stale ? "yes" : "no"}>
            <span className="limit-agent">{l.agent}</span>
            {l.windows.map((w) => (
              <span key={w.name} className="limit-window" data-window={w.name} data-used={w.used}>
                {t("limits.window", { window: windowName(w), used: w.used })}
                {w.resets && <span className="muted">{t("limits.resets", { time: when(w.resets) })}</span>}
              </span>
            ))}
            {stale && <span className="limit-as-of">{t("limits.asOf", { time: when(l.at) })}</span>}
          </span>
        );
      })}
    </span>
  );
}

// Clients names each chat client that is not connected to its chat
// service, and since when: no message from it reaches hataori meanwhile.
function Clients({ clients }: { clients: ClientState[] }) {
  const down = clients.filter((c) => !c.connected);
  if (down.length === 0) return null;
  return (
    <span className="clients" aria-label={t("clients.label")}>
      {down.map((c) => (
        <span key={c.client} className="client-down" data-client={c.client}>
          {t("clients.down", { client: c.client, time: when(c.since) })}
        </span>
      ))}
    </span>
  );
}

// StatusBar shows the selected thread on the left; on the right, each
// chat client that is not connected, each agent's remaining usage, the live mark with the position of the last
// event read, "read-only" or the pane being operated with the time left
// before its idle end, and, signed in, the files trusted from chat and the
// person's passkeys.
export function StatusBar({ selected: x, live, limits, clients, now, signedIn, operating, attached }: Props) {
  let mark: string;
  if (live.connected) {
    mark = live.pos === null ? t("status.liveStarting") : t("status.live", { pos: live.pos });
  } else {
    mark = live.pos === null ? t("status.offlineNone") : t("status.offline", { pos: live.pos });
  }
  return (
    <footer className="status" aria-label={t("status.label")}>
      <div className="status-item" data-selected={x ? x.id : ""}>
        {x ? (
          <>
            <span>{t("thread.number", { n: x.id })}</span>
            <span className={`state-text state-${x.state}`}>{stateName(x.state)}</span>
            {x.model && <span>{x.model}</span>}
            {contextUse(x) && <span>{contextUse(x)}</span>}
            {x.terminal && <span className="device">{x.terminal}</span>}
          </>
        ) : (
          <span className="muted">{t("status.noSelection")}</span>
        )}
      </div>
      <div className="status-right">
        <Clients clients={clients} />
        <Limits limits={limits} now={now} />
        <span className="live" data-live={live.connected ? "on" : "off"} data-pos={live.pos ?? ""}>
          <span className="live-dot" />
          {mark}
        </span>
        {attached !== undefined && (
          <span className="mark operating" data-status-attached={attached}>
            {t("status.attached", { n: attached })}
          </span>
        )}
        {operating?.phase === "on" ? (
          <span className="mark operating" data-status-operating={operating.pane}>
            {t("status.operating", { pane: operating.pane, left: clock(left(operating, now)) })}
          </span>
        ) : (
          <span className="mark">{t("app.readOnly")}</span>
        )}
        {signedIn && <Trusts />}
        {signedIn && <Passkeys />}
      </div>
    </footer>
  );
}
