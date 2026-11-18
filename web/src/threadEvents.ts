// A thread's events as the page shows them: its last ones from the server,
// then each as the page's one event stream delivers it, each once, in
// order.

import { useEffect, useState } from "react";
import { type Event, sameThread, type ThreadID, thread } from "./api";
import { t } from "./catalog";

export interface ThreadEventsOptions {
  history: number; // how many of the thread's last events to start with
  kinds?: ReadonlySet<string>; // only these kinds; all when absent
  keep?: number; // at most this many, the newest; all when absent
}

export function useThreadEvents(
  id: ThreadID,
  subscribe: (f: (e: Event) => void) => () => void,
  opts: ThreadEventsOptions,
): Event[] {
  const [events, setEvents] = useState<Event[]>([]);
  const { scope, key } = id;
  const { history, kinds, keep } = opts;

  useEffect(() => {
    let gone = false;
    setEvents([]);
    const early: Event[] = []; // what the stream brings while the history is read
    let ready = false;
    // add appends the events of list that come after those shown, each
    // once, in order: the history and the stream may both carry one.
    const add = (list: Event[]) =>
      setEvents((have) => {
        let last = have.length ? have[have.length - 1].pos : 0;
        const more: Event[] = [];
        for (const e of [...list].sort((a, b) => a.pos - b.pos)) {
          if (e.pos <= last) continue;
          last = e.pos;
          if (!kinds || kinds.has(e.kind)) more.push(e);
        }
        if (!more.length) return have;
        const all = [...have, ...more];
        return keep && all.length > keep ? all.slice(all.length - keep) : all;
      });
    const stop = subscribe((e) => {
      if (!sameThread(e.thread, { scope, key })) return;
      if (ready) add([e]);
      else early.push(e);
    });
    thread({ scope, key }, history)
      .then((d) => {
        if (gone) return;
        ready = true;
        add([...(d.events ?? []), ...early]);
      })
      .catch(() => {
        ready = true;
        add(early);
      });
    return () => {
      gone = true;
      stop();
    };
  }, [scope, key, subscribe, history, kinds, keep]);

  return events;
}

const str = (v: unknown) => (typeof v === "string" ? v : "");

// eventLine is one event as a line for a person.
export function eventLine(e: Event): string {
  const d = e.data ?? {};
  switch (e.kind) {
    case "text":
      return str(d.text) || t("events.textHidden");
    case "tool_started":
      return t("events.toolStarted", { tool: str(d.tool) });
    case "tool_ended":
      return t("events.toolEnded", { tool: str(d.tool) });
    case "turn_started":
      return t("events.turnStarted");
    case "turn_ended":
      return d.reason
        ? t("events.turnEndedWhy", { outcome: str(d.outcome), reason: str(d.reason) })
        : t("events.turnEnded", { outcome: str(d.outcome) });
    case "state":
      return d.reason
        ? t("events.stateWhy", { from: str(d.from), to: str(d.to), reason: str(d.reason) })
        : t("events.state", { from: str(d.from), to: str(d.to) });
    case "request":
      return str(d.text) ? t("events.request", { text: str(d.text) }) : t("events.requestHidden");
    case "answer":
      return t("events.answer");
    case "input":
      return t("events.input");
  }
  return e.kind;
}
