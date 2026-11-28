import { statsNotice } from "./cache";
import type { ThreadInfo } from "./api";
import { t } from "./catalog";
import { stateName, when } from "./format";
import { threadStateName, unavailable } from "./machine";

// Only the scoped API's public observations reach the page. The machine
// key is a label, never a destination or a browser connection parameter.
export function MachineStatus({ x, now }: { x: ThreadInfo; now: number }) {
  const a = x.availability;
  return (
    <span className="machine-status" data-machine-status="">
      <span data-machine={x.machine ?? ""}>{t("machine.key", { machine: x.machine || t("table.none") })}</span>
      {x.placement_pending && <span data-placement-pending="">{t("machine.pendingReason", { reason: x.placement_reason || t("machine.noReason") })}</span>}
      {a && unavailable(x) && (
        <>
          <span data-availability={a.state}><time dateTime={a.at}>{t("machine.since", { state: threadStateName(x), time: when(a.at) })}</time></span>
          {a.reason && <span data-availability-reason="">{a.reason}</span>}
          {a.last_known_state && (
            <span data-last-known-state={a.last_known_state}>
              {a.last_known_at
                ? t("machine.lastKnownAt", { state: stateName(a.last_known_state), time: when(a.last_known_at) })
                : t("machine.lastKnown", { state: stateName(a.last_known_state) })}
            </span>
          )}
        </>
      )}
      {statsNotice(x, now) && <span className="cached" data-stats-cached="" data-snapshot-read={x.snapshotReadAt}>{statsNotice(x, now)}</span>}
      {x.io_paused && <span data-io-paused="">{t("machine.outputPaused", { reason: x.io_paused })}</span>}
    </span>
  );
}
