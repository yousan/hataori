import { useEffect, useRef, useState } from "react";
import { act, stopPath } from "./act";
import { type Event, thread, type ThreadInfo } from "./api";
import { t } from "./catalog";

// turnEvents is how many of a thread's last events are read to find its
// running turn.
const turnEvents = 40;

// useRunningTurn is the number of the thread's running turn, by its
// turn_started, or null when none runs or it is not known yet; read again
// at each start and end of a turn.
function useRunningTurn(x: ThreadInfo, subscribe: (f: (e: Event) => void) => () => void, opens: number): number | null {
  const [turn, setTurn] = useState<number | null>(null);
  const id = x.id;
  const seq = useRef(0);
  useEffect(() => {
    let gone = false;
    const read = () => {
      const n = ++seq.current;
      thread(id, turnEvents)
        .then((d) => {
          if (gone || n !== seq.current) return;
          let open: number | null = null;
          for (const e of d.events ?? []) {
            const tn = typeof e.data?.turn === "number" ? e.data.turn : null;
            if (e.kind === "turn_started") open = tn;
            else if (e.kind === "turn_ended" && tn === open) open = null;
          }
          setTurn(d.state === "running" ? open : null);
        })
        .catch(() => {});
    };
    read();
    const off = subscribe((e) => {
      if (e.thread === id && (e.kind === "turn_started" || e.kind === "turn_ended")) read();
    });
    return () => {
      gone = true;
      off();
    };
  }, [id, subscribe, opens, x.state]);
  return turn;
}

// StopButton is a running thread's Stop button (design, section 7.2,
// "Stopping"), with one confirmation. It stops the turn that ran when it
// was pressed, by its number: a confirmation given after that turn ended
// stops no later turn, and shows the server's refusal.
export function StopButton({ x, subscribe, opens }: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void; opens: number }) {
  const turn = useRunningTurn(x, subscribe, opens);
  const [asking, setAsking] = useState<number | null>(null); // the turn the confirmation is for
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState<string | null>(null);
  const gone = useRef(false);
  useEffect(
    () => () => {
      gone.current = true;
    },
    [],
  );
  const stop = async (n: number) => {
    setBusy(true);
    const r = await act(stopPath, { thread: x.id, turn: n });
    if (gone.current) return;
    setBusy(false);
    setAsking(null);
    setRefused(r.ok ? null : r.reason || t("box.failed"));
  };
  const running = x.state === "running" && turn !== null;
  if (!running && asking === null && refused === null) return null;
  return (
    <div className="stop" data-stop={x.id}>
      {asking === null
        ? running && (
            <button type="button" className="stop-button" data-stop-button="" onClick={() => (setRefused(null), setAsking(turn))}>
              {t("stop.button")}
            </button>
          )
        : (
            <span className="stop-confirm" data-stop-confirm={asking} role="group" aria-label={t("stop.ask")}>
              <span>{t("stop.ask")}</span>
              <button type="button" data-stop-yes="" disabled={busy} onClick={() => void stop(asking)}>
                {t("stop.yes")}
              </button>
              <button type="button" data-stop-no="" disabled={busy} onClick={() => setAsking(null)}>
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
