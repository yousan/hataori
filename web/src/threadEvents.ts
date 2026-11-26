// A thread's events as the page shows them: its last ones from the server,
// then each as the page's one event stream delivers it, each once, in
// order. The history is read again each time the stream opens, so an
// event written between a read and the stream's start is not lost.

import { useEffect, useState } from "react";
import { type Event, sameThread, type ThreadID, thread } from "./api";
import { t } from "./catalog";
import { chosenLine } from "./chosen";
import { who } from "./conversation";

export interface ThreadEventsOptions {
  history: number; // how many of the thread's last events to start with
  kinds?: ReadonlySet<string>; // only these kinds; all when absent
  keep?: number; // at most this many, the newest; all when absent
}

// historyRetry is how long a view waits before it reads the history again
// after a failed read.
const historyRetry = 2000;

// merge is have and list together: each event once, by position, in order,
// of the kinds asked for, at most keep of the newest.
function merge(have: Event[], list: Event[], opts: ThreadEventsOptions): Event[] {
  const byPos = new Map(have.map((e) => [e.pos, e]));
  let added = false;
  for (const e of list) {
    if (byPos.has(e.pos) || (opts.kinds && !opts.kinds.has(e.kind))) continue;
    byPos.set(e.pos, e);
    added = true;
  }
  if (!added) return have;
  const all = [...byPos.values()].sort((a, b) => a.pos - b.pos);
  return opts.keep && all.length > opts.keep ? all.slice(all.length - opts.keep) : all;
}

export function useThreadEvents(
  id: ThreadID,
  subscribe: (f: (e: Event) => void) => () => void,
  opens: number,
  opts: ThreadEventsOptions,
): { events: Event[]; read: boolean } {
  const [events, setEvents] = useState<Event[]>([]);
  const [read, setRead] = useState(false);

  // The stream: from now on, for this thread.
  useEffect(() => {
    setEvents([]);
    setRead(false);
    return subscribe((e) => {
      if (sameThread(e.thread, id)) setEvents((have) => merge(have, [e], opts));
    });
  }, [id, subscribe, opts]);

  // The history: now, again after the stream opens, and again after a
  // failed read.
  useEffect(() => {
    let gone = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const fetchHistory = () =>
      thread(id, opts.history)
        .then((d) => {
          if (gone) return;
          setEvents((have) => merge(have, d.events ?? [], opts));
          setRead(true);
        })
        .catch(() => {
          if (!gone) retry = setTimeout(fetchHistory, historyRetry);
        });
    fetchHistory();
    return () => {
      gone = true;
      clearTimeout(retry);
    };
  }, [id, opens, opts]);

  return { events, read };
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
    case "input_failed":
      return t("events.inputFailed", { reason: str(d.reason) });
    case "input":
      return t("events.input");
    case "start_chosen":
      return chosenLine(str(d.choice), who(d));
  }
  return t("events.other", { kind: e.kind });
}
