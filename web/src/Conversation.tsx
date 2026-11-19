import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { type Event, sameThread, type ThreadInfo, threadEvents } from "./api";
import { t } from "./catalog";
import { conversationKinds, type Entry, entries, merge } from "./conversation";

// page is how many events one read asks for: the control API's most.
const page = 100;
// retryDelay is how long the view waits before it reads again after a
// failed read.
const retryDelay = 2000;
// nearTop is how close to the top, in pixels, a scroll loads older entries.
const nearTop = 48;

function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString();
}

// Text is a text written by a person or the agent, shown as it is: no
// Markdown, no HTML. Absent, it was withheld from this caller.
function Text({ text }: { text: string | undefined }) {
  if (text === undefined) return <span className="withheld">{t("conv.withheld")}</span>;
  return <div className="conv-text">{text}</div>;
}

function Head({ who, time }: { who: string; time: string }) {
  return (
    <div className="conv-head">
      <span className="conv-who">{who}</span>
      <time className="conv-time" dateTime={time}>
        {clock(time)}
      </time>
    </div>
  );
}

function turnBy(e: Extract<Entry, { kind: "turn_started" }>): string {
  if (e.by === "agent") return t("conv.byAgent");
  if (e.by === "terminal") return t("conv.byTerminal");
  return e.who ?? t("conv.someone");
}

function Line({ e }: { e: Entry }) {
  switch (e.kind) {
    case "turn_started":
      return (
        <li className="conv-turn" data-kind={e.kind} data-pos={e.pos}>
          {t("conv.turnStarted", { n: e.turn ?? "", who: turnBy(e), time: clock(e.time) })}
        </li>
      );
    case "turn_ended":
      return (
        <li className="conv-turn conv-turn-end" data-kind={e.kind} data-pos={e.pos}>
          {e.reason
            ? t("conv.turnEndedWhy", { n: e.turn ?? "", outcome: e.outcome, reason: e.reason, time: clock(e.time) })
            : t("conv.turnEnded", { n: e.turn ?? "", outcome: e.outcome, time: clock(e.time) })}
        </li>
      );
    case "input":
      return (
        <li className="conv-entry conv-input" data-kind={e.kind} data-pos={e.pos}>
          <Head who={e.who ?? t("conv.someone")} time={e.time} />
          <Text text={e.text} />
        </li>
      );
    case "request":
      return (
        <li className="conv-entry conv-request" data-kind={e.kind} data-pos={e.pos}>
          <Head who={t(e.approval ? "conv.approval" : "conv.question")} time={e.time} />
          <Text text={e.text} />
          {e.choices && e.choices.length > 0 && <div className="conv-choices">{t("conv.choices", { choices: e.choices.join(" · ") })}</div>}
        </li>
      );
    case "answer":
      return (
        <li className="conv-entry conv-answer" data-kind={e.kind} data-pos={e.pos}>
          <Head who={t("conv.answerFrom", { who: e.who ?? t("conv.someone") })} time={e.time} />
          <Text text={e.text} />
        </li>
      );
    case "text":
      return (
        <li className={e.final ? "conv-entry conv-agent conv-final" : "conv-entry conv-agent"} data-kind={e.kind} data-pos={e.pos}>
          <Head who={t("conv.agent")} time={e.time} />
          <Text text={e.text} />
        </li>
      );
    case "tool":
      return (
        <li className="conv-tool" data-kind={e.kind} data-pos={e.pos}>
          <details>
            <summary>{t("conv.tool", { tool: e.tool })}</summary>
            <p className="conv-tool-detail">
              {e.ended
                ? t("conv.toolEnded", {
                    started: clock(e.time),
                    ended: clock(e.ended),
                    seconds: ((Date.parse(e.ended) - Date.parse(e.time)) / 1000).toFixed(1),
                  })
                : t("conv.toolRunning", { started: clock(e.time) })}
              {e.parent && <> {t("conv.toolIn", { parent: e.parent })}</>}
            </p>
          </details>
        </li>
      );
  }
}

// Conversation is a thread's conversation from its event log: it starts at
// the end, reads older entries when scrolled to the top until the thread's
// start, and adds new ones as the page's event stream delivers them. Each
// time the stream opens again it reads back from the newest event to what
// it holds, so a reconnect leaves no gap; each event is kept once, by its
// position, so nothing repeats.
export function Conversation(props: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void; opens: number }) {
  const { x, subscribe, opens } = props;
  const id = x.id;
  const [events, setEvents] = useState<Event[]>([]);
  const [start, setStart] = useState(false); // the thread's first event is read
  const [loading, setLoading] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  const held = useRef<Event[]>([]); // events, for the reads to see without waiting for a render
  const oldest = useRef(0); // the oldest position a read returned; 0 before any
  const reading = useRef(false);
  const atEnd = useRef(true); // the view shows the newest entry: it follows new ones
  const anchor = useRef<{ height: number; top: number } | null>(null);

  const add = (more: readonly Event[]) => {
    const next = merge(held.current, more);
    if (next !== held.current) {
      held.current = next;
      setEvents(next);
    }
  };
  const read = (evs: readonly Event[]) => {
    if (evs.length > 0) oldest.current = oldest.current === 0 ? evs[0].pos : Math.min(oldest.current, evs[0].pos);
    add(evs);
    if (evs.length < page) setStart(true);
  };

  // The stream, from now on, for this thread.
  useEffect(() => {
    held.current = [];
    oldest.current = 0;
    setEvents([]);
    setStart(false);
    return subscribe((e) => {
      if (sameThread(e.thread, id) && conversationKinds.has(e.kind)) add([e]);
    });
  }, [id, subscribe]);

  // The newest events: now, and again each time the stream opens, back to
  // the newest one held. The first read takes one page; older ones come
  // with scrolling.
  useEffect(() => {
    let gone = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const catchUp = async () => {
      const first = oldest.current === 0;
      const newest = held.current.length > 0 ? held.current[held.current.length - 1].pos : 0;
      try {
        for (let before = 0; ; ) {
          const evs = await threadEvents(id, before, page);
          if (gone) return;
          if (first || evs.length < page) {
            read(evs);
            return;
          }
          add(evs);
          if (evs[0].pos <= newest) return;
          before = evs[0].pos;
        }
      } catch {
        if (!gone) retry = setTimeout(catchUp, retryDelay);
      }
    };
    catchUp();
    return () => {
      gone = true;
      clearTimeout(retry);
    };
  }, [id, opens]);

  // older reads the page before the oldest event read.
  const older = () => {
    const el = box.current;
    if (!el || start || reading.current || oldest.current === 0) return;
    reading.current = true;
    setLoading(true);
    anchor.current = { height: el.scrollHeight, top: el.scrollTop };
    threadEvents(id, oldest.current, page)
      .then((evs) => read(evs))
      .catch(() => {
        anchor.current = null; // the next scroll to the top tries again
      })
      .finally(() => {
        reading.current = false;
        setLoading(false);
      });
  };

  // After a render: older entries keep what was on screen in place; new
  // ones are followed when the newest was shown; and a view not yet full
  // reads older entries until it is, or until the start.
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    if (anchor.current) {
      el.scrollTop = anchor.current.top + (el.scrollHeight - anchor.current.height);
      anchor.current = null;
    } else if (atEnd.current) {
      el.scrollTop = el.scrollHeight;
    }
    if (el.scrollTop < nearTop && !start && !reading.current) older();
  }, [events, start]);

  const onScroll = () => {
    const el = box.current;
    if (!el) return;
    atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 4;
    if (el.scrollTop < nearTop) older();
  };

  const list = useMemo(() => entries(events), [events]);
  return (
    <div className="conversation" data-conversation={id} data-start={start ? "yes" : "no"} ref={box} onScroll={onScroll}>
      {start ? (
        <p className="conv-start">{t("conv.start")}</p>
      ) : (
        loading && <p className="conv-loading">{t("conv.loading")}</p>
      )}
      {start && list.length === 0 && <p className="conv-empty">{t("conv.empty")}</p>}
      <ol className="conv-list">
        {list.map((e) => (
          <Line key={e.pos} e={e} />
        ))}
      </ol>
    </div>
  );
}
