import { useEffect, useState } from "react";
import { type Event, sameThread, type ThreadInfo, thread } from "./api";
import { t } from "./catalog";

// shownKinds are the events this view lists: text, tools and turns.
const shownKinds = new Set(["text", "tool_started", "tool_ended", "turn_started", "turn_ended"]);

// historyEvents is how many of a thread's last events the view starts with.
const historyEvents = 100;

function line(e: Event): string {
  const d = e.data ?? {};
  const s = (v: unknown) => (typeof v === "string" ? v : "");
  switch (e.kind) {
    case "text":
      return s(d.text) || t("events.textHidden");
    case "tool_started":
      return t("events.toolStarted", { tool: s(d.tool) });
    case "tool_ended":
      return t("events.toolEnded", { tool: s(d.tool) });
    case "turn_started":
      return t("events.turnStarted");
    case "turn_ended":
      return d.reason ? t("events.turnEndedWhy", { outcome: s(d.outcome), reason: s(d.reason) }) : t("events.turnEnded", { outcome: s(d.outcome) });
  }
  return "";
}

// EventsView shows a session that has no terminal, such as a Codex
// session: its text, tool and turn events in order, the last ones of the
// thread first, then each as the page's event stream delivers it.
export function EventsView(props: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void }) {
  const { x, subscribe } = props;
  const [events, setEvents] = useState<Event[]>([]);
  const { scope, key } = x.id;

  useEffect(() => {
    let gone = false;
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
          if (shownKinds.has(e.kind)) more.push(e);
        }
        return more.length ? [...have, ...more] : have;
      });
    const stop = subscribe((e) => {
      if (!sameThread(e.thread, { scope, key })) return;
      if (ready) add([e]);
      else early.push(e);
    });
    thread({ scope, key }, historyEvents)
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
  }, [scope, key, subscribe]);

  return (
    <ol className="events-view" data-events={x.number}>
      {events.map((e) => (
        <li key={e.pos} className={`ev ev-${e.kind}`} data-kind={e.kind} data-pos={e.pos}>
          <time className="ev-time" dateTime={e.time}>
            {new Date(e.time).toLocaleTimeString()}
          </time>
          <span className="ev-line">{line(e)}</span>
        </li>
      ))}
    </ol>
  );
}
