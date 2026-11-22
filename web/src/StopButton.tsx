import { useEffect, useRef, useState } from "react";
import { act, stopPath } from "./act";
import { type Event, thread, threadEvents, type ThreadInfo } from "./api";
import { t } from "./catalog";

// turnEvents is how many of a thread's last events are read with its
// state; a running turn's start further back is looked for in the thread's
// log, maxEvents at a time (the server's MaxEvents), at most maxPages
// times.
const turnEvents = 40;
const maxEvents = 100;
const maxPages = 50;

// retryDelay is how long the page waits to read the running turn again
// after a read failed.
const retryDelay = 2000;

// openTurn is the turn that evs, oldest first, leave running: the last
// turn_started whose turn has no turn_ended after it. found is false when
// evs hold neither a start nor an end of a turn.
function openTurn(evs: Event[]): { turn: number | null; found: boolean } {
  for (let i = evs.length - 1; i >= 0; i--) {
    const e = evs[i];
    const tn = typeof e.data?.turn === "number" ? e.data.turn : null;
    if (e.kind === "turn_ended") return { turn: null, found: true };
    if (e.kind === "turn_started") return { turn: tn, found: true };
  }
  return { turn: null, found: false };
}

// useRunningTurn is the number of the thread's running turn, by its
// turn_started, or null when none runs or it is not known; read again at
// each start and end of a turn, when the event stream opens again, and
// after a read that failed, which leaves it unknown meanwhile.
function useRunningTurn(x: ThreadInfo, subscribe: (f: (e: Event) => void) => () => void, opens: number): number | null {
  const [turn, setTurn] = useState<number | null>(null);
  const id = x.id;
  const seq = useRef(0);
  useEffect(() => {
    let gone = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      const n = ++seq.current;
      const current = () => !gone && n === seq.current;
      (async () => {
        const d = await thread(id, turnEvents);
        if (d.state !== "running") return null;
        let page = d.events ?? [];
        for (let i = 0; i <= maxPages; i++) {
          const o = openTurn(page);
          if (o.found) return o.turn;
          if (page.length === 0) break;
          page = await threadEvents(id, page[0].pos, maxEvents);
        }
        return null;
      })()
        .then((tn) => current() && setTurn(tn))
        .catch(() => {
          if (!current()) return;
          setTurn(null); // not known: no turn is named on the strength of an old read
          retry = setTimeout(read, retryDelay);
        });
    };
    read();
    const off = subscribe((e) => {
      if (e.thread === id && (e.kind === "turn_started" || e.kind === "turn_ended")) {
        clearTimeout(retry);
        read();
      }
    });
    return () => {
      gone = true;
      clearTimeout(retry);
      off();
    };
  }, [id, subscribe, opens, x.state]);
  return turn;
}

// StopButton is a running thread's Stop button (design, section 7.2,
// "Stopping"), with one confirmation. It stops the turn that ran when it
// was pressed, by its number: a confirmation given after that turn ended
// stops no later turn, and shows the server's refusal until a turn ends.
export function StopButton({ x, subscribe, opens }: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void; opens: number }) {
  const turn = useRunningTurn(x, subscribe, opens);
  const [asking, setAsking] = useState<number | null>(null); // the turn the confirmation is for
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState<string | null>(null);
  const prevTurn = useRef<number | null>(null);
  const gone = useRef(false);
  const stopRef = useRef<HTMLButtonElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const backToStop = useRef(false); // the confirmation closed: the focus goes back to Stop
  useEffect(() => {
    gone.current = false; // also when React mounts it again, as Strict Mode does
    return () => {
      gone.current = true;
    };
  }, []);
  // A refusal stays until a turn ends after it, or Stop is pressed again.
  useEffect(() => {
    if (prevTurn.current !== null && turn === null) setRefused(null);
    prevTurn.current = turn;
  }, [turn]);
  // The focus follows the confirmation: into it when it opens, back to
  // Stop when it closes.
  useEffect(() => {
    if (asking !== null) cancelRef.current?.focus();
    else if (backToStop.current) {
      backToStop.current = false;
      stopRef.current?.focus();
    }
  }, [asking]);
  const close = () => {
    backToStop.current = true;
    setAsking(null);
  };
  const stop = async (n: number) => {
    setBusy(true);
    const r = await act(stopPath, { thread: x.id, turn: n });
    if (gone.current) return;
    setBusy(false);
    close();
    setRefused(r.ok ? null : r.reason || t("box.failed"));
  };
  const running = x.state === "running" && turn !== null;
  if (!running && asking === null && refused === null) return null;
  return (
    <div className="stop" data-stop={x.id}>
      {asking === null ? (
        running && (
          <button
            type="button"
            ref={stopRef}
            className="stop-button"
            data-stop-button=""
            onClick={() => {
              setRefused(null);
              setAsking(turn);
            }}
          >
            {t("stop.button")}
          </button>
        )
      ) : (
        <span className="stop-confirm" data-stop-confirm={asking} role="group" aria-label={t("stop.ask")}>
          <span>{t("stop.ask")}</span>
          <button type="button" data-stop-yes="" disabled={busy} onClick={() => void stop(asking)}>
            {t("stop.yes")}
          </button>
          <button type="button" ref={cancelRef} data-stop-no="" disabled={busy} onClick={close}>
            {t("stop.no")}
          </button>
        </span>
      )}
      {refused !== null && (
        <span className="box-refused" role="status" data-stop-refused="">
          {t("stop.refused", { reason: refused })}
        </span>
      )}
    </div>
  );
}
