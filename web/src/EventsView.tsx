import type { Event, ThreadInfo } from "./api";
import { eventLine, useThreadEvents } from "./threadEvents";

// shownKinds are the events this view lists: text, tools and turns.
const shownKinds: ReadonlySet<string> = new Set(["text", "tool_started", "tool_ended", "turn_started", "turn_ended"]);

// historyEvents is how many of a thread's last events the view starts with.
const historyEvents = 100;
const options = { history: historyEvents, kinds: shownKinds };

// EventsView shows a session that has no terminal, such as a Codex
// session: its text, tool and turn events in order, the last ones of the
// thread first, then each as the page's event stream delivers it.
export function EventsView(props: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void; opens: number }) {
  const { x, subscribe, opens } = props;
  const { events } = useThreadEvents(x.id, subscribe, opens, options);
  return (
    <ol className="events-view" data-events={x.id}>
      {events.map((e) => (
        <li key={e.pos} className={`ev ev-${e.kind}`} data-kind={e.kind} data-pos={e.pos}>
          <time className="ev-time" dateTime={e.time}>
            {new Date(e.time).toLocaleTimeString()}
          </time>
          <span className="ev-line">{eventLine(e)}</span>
        </li>
      ))}
    </ol>
  );
}
