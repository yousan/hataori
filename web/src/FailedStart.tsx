import { useEffect, useRef, useState } from "react";
import { act, choosePath } from "./act";
import { type Event, type FailedStart, sameThread, thread, type ThreadID, type ThreadInfo } from "./api";
import { type Key, t } from "./catalog";
import { chosenLine, closed, trusted, withoutSettings } from "./chosen";
import { who } from "./conversation";
import { startFailure, type StartFailure } from "./failure";

// detailEvents is how many of a thread's last events are read for the last
// choice made for its start.
const detailEvents = 40;

// retryDelay is how long the page waits to read again after a read failed.
const retryDelay = 2000;

// startKinds are the events of a thread after which its start is read again.
const startKinds: ReadonlySet<string> = new Set(["state", "session_replaced", "start_chosen", "turn_started"]);

// Start is how a thread's start went: failed, and the last choice a person
// made for it, until the second turn after it starts.
export interface Start {
  failed: FailedStart | null;
  chosen: string | null; // who chose what
}

const none: Start = { failed: null, chosen: null };

// chosenOf is the choice that evs, a thread's last events, show; before is
// the one shown until now, which stays when evs do not reach back to it.
function chosenOf(evs: readonly Event[], before: string | null): string | null {
  let turns = 0;
  for (let i = evs.length - 1; i >= 0; i--) {
    const e = evs[i];
    if (e.kind === "turn_started" && ++turns > 1) return null;
    if (e.kind === "start_chosen") return chosenLine(typeof e.data?.choice === "string" ? e.data.choice : "", who(e.data ?? {}));
  }
  return before;
}

// useStart is thread x's start, read again when an event of the thread may
// have changed it, when the event stream opens again, and after a read that
// failed.
export function useStart(x: ThreadInfo, subscribe: (f: (e: Event) => void) => () => void, opens: number): Start {
  const [start, setStart] = useState<Start>(none);
  const id = x.id;
  const seq = useRef(0);
  useEffect(() => setStart(none), [id]);
  useEffect(() => {
    let gone = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      const n = ++seq.current;
      const current = () => !gone && n === seq.current;
      thread(id, detailEvents)
        .then((d) => {
          if (current()) setStart((s) => ({ failed: d.failed_start ?? null, chosen: chosenOf(d.events ?? [], s.chosen) }));
        })
        .catch(() => {
          if (current()) timer = setTimeout(read, retryDelay);
        });
    };
    read();
    const off = subscribe((e) => {
      if (!sameThread(e.thread, id) || !startKinds.has(e.kind)) return;
      clearTimeout(timer);
      timer = setTimeout(read, 100); // events that come together, one read
    });
    return () => {
      gone = true;
      clearTimeout(timer);
      off();
    };
  }, [id, subscribe, opens, x.state]);
  return start;
}

const whyKeys: Record<StartFailure, Key> = {
  trust: "start.why.trust",
  blocked: "start.why.blocked",
  gone: "start.why.gone",
  start: "start.why.start",
  resume: "start.why.resume",
  takeIn: "start.why.takeIn",
  ended: "start.why.ended",
  other: "start.why.other",
};

// StartedBy is who started x: a person's name, or the supervisor of its
// channel, whose thread it opens.
export function StartedBy({ x, supervisor, onOpen }: { x: ThreadInfo; supervisor: ThreadID | undefined; onOpen?: (id: ThreadID) => void }) {
  if (supervisor === undefined) return <>{x.started_by || t("conv.someone")}</>;
  const name = t("start.supervisor", { n: supervisor });
  if (!onOpen) return <>{name}</>;
  return (
    <button type="button" className="link" data-supervisor={supervisor} onClick={() => onOpen(supervisor)}>
      {name}
    </button>
  );
}

// FailedStartView says what a thread that failed as it started was asked,
// who started it and why it stopped, in one sentence; the details panel has
// the full reason. For a start that files blocked, a person who may act has
// the choices that chat offers (internal/chat, trustbutton.go).
export function FailedStartView(props: { x: ThreadInfo; f: FailedStart; mayAct: boolean; onOpen?: (id: ThreadID) => void }) {
  const { x, f, mayAct, onOpen } = props;
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState("");
  const gone = useRef(false);
  useEffect(() => {
    gone.current = false; // also when React mounts it again, as Strict Mode does
    return () => {
      gone.current = true;
    };
  }, []);
  const trust = f.trust ?? [];
  const choose = async (choice: string) => {
    if (busy) return;
    setBusy(true);
    setRefused("");
    const a = await act(choosePath, { thread: x.id, session: f.session, choice, files: trust });
    if (gone.current) return;
    setBusy(false);
    if (!a.ok) setRefused(a.reason || t("box.failed"));
  };
  const files = trust.join(", ");
  return (
    <section className="failed-start" data-failed-start={f.session} aria-label={t("start.label")}>
      <dl>
        {f.text !== undefined && (
          <div data-field="asked">
            <dt>{t("start.asked")}</dt>
            <dd className="failed-start-text">{f.text}</dd>
          </div>
        )}
        <div data-field="by">
          <dt>{t("start.by")}</dt>
          <dd>
            <StartedBy x={x} supervisor={f.supervisor} onOpen={onOpen} />
          </dd>
        </div>
        <div data-field="why">
          <dt>{t("start.why")}</dt>
          <dd>{t(whyKeys[startFailure(f.reason, trust)], { files, reason: f.reason ?? "" })}</dd>
        </div>
      </dl>
      {mayAct && x.state === "failed" && trust.length > 0 && (
        <>
          <div className="answers-row">
            <button type="button" data-start-choice={withoutSettings} disabled={busy} title={t("start.without.title")} onClick={() => void choose(withoutSettings)}>
              {t("start.without")}
            </button>
            <button type="button" data-start-choice={trusted} disabled={busy} title={t("start.trust.title", { files, repo: x.repo })} onClick={() => void choose(trusted)}>
              {t("start.trust")}
            </button>
            <button type="button" data-start-choice={closed} disabled={busy} title={t("start.close.title")} onClick={() => void choose(closed)}>
              {t("start.close")}
            </button>
          </div>
          {refused && (
            <p className="box-refused" role="status" data-start-refused="">
              {t("start.refused", { reason: refused })}
            </p>
          )}
        </>
      )}
    </section>
  );
}
