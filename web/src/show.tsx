// Which threads the tree, the All threads table and the narrow list show:
// the active ones, or all of them. The choice is kept in this browser
// across reloads. It hides rows only: the state counts, the tabs, the
// details panel and the status bar still see every thread.

import { useState } from "react";
import type { State, ThreadInfo } from "./api";
import { t } from "./catalog";

export type Show = "active" | "all";

const storageKey = "hataori.show";

// activeStates are the states of a thread that is doing something or
// waits on a person. Asleep, closed and removed threads are hidden.
const activeStates: ReadonlySet<State> = new Set<State>(["creating", "idle", "running", "waiting", "failed"]);

// shows reports whether a thread's row is shown under the choice show.
export function shows(show: Show, x: ThreadInfo): boolean {
  return show === "all" || activeStates.has(x.state);
}

// hiddenCount is how many of ts the choice show hides.
export function hiddenCount(show: Show, ts: readonly ThreadInfo[] | null): number {
  return (ts ?? []).filter((x) => !shows(show, x)).length;
}

function saved(): Show {
  try {
    return localStorage.getItem(storageKey) === "all" ? "all" : "active";
  } catch {
    return "active";
  }
}

export function useShow(): [Show, (s: Show) => void] {
  const [show, setShow] = useState<Show>(saved);
  const choose = (s: Show) => {
    setShow(s);
    try {
      localStorage.setItem(storageKey, s);
    } catch {
      // Not kept; the page still switches.
    }
  };
  return [show, choose];
}

// ShowChoice is the one control that switches between the active threads
// and all of them, with the number of threads it hides beside it.
export function ShowChoice({ show, onShow, hidden }: { show: Show; onShow: (s: Show) => void; hidden: number }) {
  return (
    <div className="show" role="group" aria-label={t("show.label")} data-show={show}>
      {(["active", "all"] as const).map((s) => (
        <button
          key={s}
          type="button"
          data-choice={s}
          aria-pressed={show === s}
          title={t(s === "active" ? "show.activeTitle" : "show.allTitle")}
          onClick={() => onShow(s)}
        >
          {t(s === "active" ? "show.active" : "show.all")}
        </button>
      ))}
      {show === "active" && (
        <span className="hidden-count" data-hidden={hidden}>
          {t("show.hidden", { n: hidden })}
        </span>
      )}
    </div>
  );
}
