import type { ThreadInfo } from "./api";
import { type Key, t } from "./catalog";
import { stateName } from "./format";

export const unavailableStates = ["checking", "away", "catching_up", "unavailable", "pending"] as const;

export function unavailable(x: ThreadInfo): boolean {
  return !!x.availability && x.availability.state !== "ready";
}

// Use availability before the underlying state everywhere, including dots
// and aggregate counts. An unrecognised non-ready observation fails closed.
export function visibleState(x: ThreadInfo): string {
  if (unavailable(x)) {
    const s = x.availability!.state;
    return unavailableStates.some((v) => v === s) ? s : "unavailable";
  }
  return x.placement_pending ? "pending" : x.state;
}

export function stateSince(x: ThreadInfo): string {
  return unavailable(x) ? x.availability!.at : x.updated;
}

export function threadStateName(x: ThreadInfo): string {
  const s = visibleState(x);
  return unavailableStates.some((v) => v === s) ? t(`machine.${s}` as Key) : stateName(x.state);
}
