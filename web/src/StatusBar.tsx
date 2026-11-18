import type { ThreadInfo } from "./api";
import { t } from "./catalog";
import { contextUse, stateName } from "./format";
import type { Live } from "./live";

interface Props {
  selected: ThreadInfo | undefined;
  live: Live;
}

// StatusBar shows the selected thread on the left; on the right, the live
// mark with the position of the last event read, and "read-only".
export function StatusBar({ selected: x, live }: Props) {
  let mark: string;
  if (live.connected) {
    mark = live.pos === null ? t("status.liveStarting") : t("status.live", { pos: live.pos });
  } else {
    mark = live.pos === null ? t("status.offlineNone") : t("status.offline", { pos: live.pos });
  }
  return (
    <footer className="status" aria-label={t("status.label")}>
      <div className="status-item" data-selected={x ? x.number : ""}>
        {x ? (
          <>
            <span>{t("thread.number", { n: x.number })}</span>
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
        <span className="live" data-live={live.connected ? "on" : "off"} data-pos={live.pos ?? ""}>
          <span className="live-dot" />
          {mark}
        </span>
        <span className="mark">{t("app.readOnly")}</span>
      </div>
    </footer>
  );
}
