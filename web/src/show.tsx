// Which threads the tree, the All threads table and the narrow list show:
// the active ones, or all of them; and, with the same choice, whether the
// tree and the narrow list show tmux panes that are idle shells. The
// choice is kept in this browser across reloads. It hides rows only: the
// state counts, the tabs, the details panel and the status bar still see
// every thread and every terminal.

import { useState } from "react";
import type { HostTerminal, State, ThreadInfo } from "./api";
import { t } from "./catalog";

export type Show = "active" | "all";

const storageKey = "hataori.show";

// activeStates are the states of a thread that is doing something or
// waits on a person. Asleep, closed and removed threads are hidden.
const activeStates: ReadonlySet<State> = new Set<State>(["creating", "idle", "running", "waiting", "failed"]);

// OpenInChat is what the chats' layout says (chatTree.ts, #599): the
// scopes whose channel it lays out, and the threads open in the chat.
export interface OpenInChat {
  scopes: ReadonlySet<string>;
  threads: ReadonlySet<number>;
}

// urgentStates are the states of a thread that is shown, as active, also
// when its chat thread is not open: it works or waits on a person.
const urgentStates: ReadonlySet<State> = new Set<State>(["creating", "running", "waiting", "failed"]);

// shows reports whether a thread's row is shown under the choice show. In
// a scope the chat's layout covers (open), the active threads are those
// open in the chat, as the chat shows them, and those that work or wait
// on a person; elsewhere, those in an active state.
export function shows(show: Show, x: ThreadInfo, open?: OpenInChat | null): boolean {
  if (show === "all") return true;
  if (open?.scopes.has(x.scope)) return open.threads.has(x.id) || urgentStates.has(x.state);
  return activeStates.has(x.state);
}

// hiddenCount is how many of ts the choice show hides.
export function hiddenCount(show: Show, ts: readonly ThreadInfo[] | null, open?: OpenInChat | null): number {
  return (ts ?? []).filter((x) => !shows(show, x, open)).length;
}

// shells are the commands of a pane that is only a shell, as the host
// reports them; a login shell's leading "-" is left out first.
const shells: ReadonlySet<string> = new Set(["sh", "bash", "zsh", "fish", "dash", "ksh", "mksh", "tcsh", "csh", "nu"]);

// idleFor is how long a window must have shown no activity for its shell
// pane to be idle: a day.
const idleFor = 24 * 60 * 60 * 1000;

// idleShell reports whether h is a tmux pane whose command is a shell and
// whose window's last activity is more than a day before now, the host's
// time. A terminal outside tmux, and a pane whose activity is not known,
// never is.
export function idleShell(h: HostTerminal, now: number): boolean {
  if (!h.tmux || !h.activity) return false;
  const at = Date.parse(h.activity);
  return shells.has((h.process ?? "").replace(/^-/, "")) && Number.isFinite(at) && now - at > idleFor;
}

// showsHost reports whether a host terminal's row is shown under the
// choice show.
export function showsHost(show: Show, h: HostTerminal, now: number): boolean {
  return show === "all" || !idleShell(h, now);
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
// and all of them, with the number of threads it hides beside it, the
// same number the tree and the narrow list give. The idle shell panes the
// same choice hides (terminals) are counted on their own, never added to
// the threads.
export function ShowChoice({ show, onShow, hidden, terminals = 0 }: { show: Show; onShow: (s: Show) => void; hidden: number; terminals?: number }) {
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
      {show === "active" && terminals > 0 && (
        <>
          <span className="hidden-sep" aria-hidden="true">
            {t("show.hiddenSeparator")}
          </span>
          <span className="hidden-terminals" data-hidden-terminals={terminals}>
            {t("show.hiddenTerminals", { n: terminals })}
          </span>
        </>
      )}
    </div>
  );
}
