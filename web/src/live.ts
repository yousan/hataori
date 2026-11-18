// useDashboard keeps the page's picture of the threads: the list from the
// server, what each thread that needs attention is waiting on, and the
// event stream that says when to read them again. The server is the only
// source; the page infers no state from the events themselves.

import { useEffect, useRef, useState } from "react";
import { type Event, eventsPath, SignedOut, type ThreadDetail, type ThreadInfo, thread, threads } from "./api";

// Attention is why a thread needs attention.
export type Attention =
  | { kind: "question"; text?: string }
  | { kind: "approval"; text?: string }
  | { kind: "failure"; reason?: string };

export interface Live {
  // connected: the event stream is open.
  connected: boolean;
  // pos: the position of the last event received, or of the caught_up
  // mark; null before either.
  pos: number | null;
}

export interface Dashboard {
  threads: ThreadInfo[] | null; // null until the first list arrives
  attention: ReadonlyMap<number, Attention>; // by thread number
  live: Live;
  signedOut: boolean;
}

// refreshDelay gathers the events that come together into one read.
const refreshDelay = 150;
// reopenDelay is how long the page waits before it opens a stream that the
// server refused again.
const reopenDelay = 2000;
// detailEvents is how many of a thread's last events are read for its
// failure reason.
const detailEvents = 20;

function needsAttention(t: ThreadInfo): boolean {
  return t.state === "waiting" || t.state === "failed";
}

// attentionOf reads what a waiting or failed thread is waiting on.
export function attentionOf(d: ThreadDetail): Attention | undefined {
  if (d.state === "waiting") {
    const r = d.requests?.[0];
    if (r?.kind === 1) return { kind: "approval", text: r.text };
    return { kind: "question", text: r?.text };
  }
  if (d.state === "failed") {
    const evs = d.events ?? [];
    for (let i = evs.length - 1; i >= 0; i--) {
      const e = evs[i];
      if (e.kind === "state" && e.data?.to === "failed") {
        return { kind: "failure", reason: reasonOf(e) };
      }
    }
    for (let i = evs.length - 1; i >= 0; i--) {
      const e = evs[i];
      if (e.kind === "turn_ended" && e.data?.outcome === "failure") {
        return { kind: "failure", reason: reasonOf(e) };
      }
    }
    return { kind: "failure" };
  }
  return undefined;
}

function reasonOf(e: Event): string | undefined {
  const r = e.data?.reason;
  return typeof r === "string" && r !== "" ? r : undefined;
}

// lastPosition is the position of the newest event of any of ts: the
// stream starts after it, so the page does not read the whole log again.
async function lastPosition(ts: ThreadInfo[]): Promise<number> {
  const ds = await Promise.all(ts.map((x) => thread(x.id, 1)));
  return ds.reduce((p, d) => Math.max(p, d.events?.[d.events.length - 1]?.pos ?? 0), 0);
}

export function useDashboard(): Dashboard {
  const [state, setState] = useState<Dashboard>({
    threads: null,
    attention: new Map(),
    live: { connected: false, pos: null },
    signedOut: false,
  });
  const reads = useRef(0);

  useEffect(() => {
    let closed = false;
    let source: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let last = 0; // the position to go on after
    let out = false; // the session has ended

    const signedOut = () => {
      out = true;
      source?.close();
      if (!closed) setState((s) => ({ ...s, signedOut: true, live: { ...s.live, connected: false } }));
    };

    // read reads the list, and what each thread that needs attention waits
    // on. Only the newest read is kept.
    const read = async () => {
      const n = ++reads.current;
      try {
        const ts = await threads();
        const details = await Promise.all(ts.filter(needsAttention).map((x) => thread(x.id, detailEvents)));
        if (closed || n !== reads.current) return;
        const attention = new Map<number, Attention>();
        for (const d of details) {
          const a = attentionOf(d);
          if (a) attention.set(d.number, a);
        }
        setState((s) => ({ ...s, threads: ts, attention }));
      } catch (err) {
        if (err instanceof SignedOut) signedOut();
      }
    };
    const later = () => {
      clearTimeout(timer);
      timer = setTimeout(read, refreshDelay);
    };
    const at = (pos: number) => {
      last = Math.max(last, pos);
      setState((s) => ({ ...s, live: { connected: true, pos } }));
    };

    const open = (after: number) => {
      last = after;
      source = new EventSource(`${eventsPath}?after=${after}`);
      source.onopen = () => {
        setState((s) => ({ ...s, live: { ...s.live, connected: true } }));
        later(); // what changed while the stream was down
      };
      source.onmessage = (m) => {
        const pos = Number(m.lastEventId);
        if (Number.isFinite(pos)) at(pos);
        later();
      };
      source.addEventListener("caught_up", (m) => {
        const pos = Number((m as MessageEvent).lastEventId);
        if (Number.isFinite(pos)) at(pos);
      });
      source.onerror = () => {
        setState((s) => ({ ...s, live: { ...s.live, connected: false } }));
        // A stream the server refused does not come back by itself: find
        // out whether the session ended, and open it again if not.
        if (source?.readyState === EventSource.CLOSED) {
          source.close();
          read().then(() => {
            if (!closed && !out) setTimeout(() => !closed && !out && open(last), reopenDelay);
          });
        }
      };
    };

    (async () => {
      try {
        const first = await threads();
        const after = await lastPosition(first);
        if (closed) return;
        open(after);
        await read();
      } catch (err) {
        if (err instanceof SignedOut) signedOut();
      }
    })();

    return () => {
      closed = true;
      clearTimeout(timer);
      source?.close();
    };
  }, []);

  return state;
}

// useNow is the time, renewed every interval milliseconds.
export function useNow(interval: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), interval);
    return () => clearInterval(id);
  }, [interval]);
  return now;
}
