import type { ThreadInfo } from "./api";
import { t } from "./catalog";
import { age, when } from "./format";
import { unavailable } from "./machine";
import type { Workspace } from "./workspaces";

export function workspaceNotice(x: ThreadInfo, w: Workspace | undefined, now: number): string {
  if (!w?.unavailable && !unavailable(x) && !x.placement_pending) return "";
  return w?.data && w.at
    ? t("machine.workspaceCached", { age: age(w.at, now), time: when(w.at) })
    : t("machine.workspaceUnavailable");
}

export function statsNotice(x: ThreadInfo, now: number): string {
  if (!unavailable(x)) return "";
  const at = x.snapshotReadAt;
  return at ? t("machine.statsCached", { age: age(at, now), time: when(at) }) : t("machine.statsUnknown");
}
