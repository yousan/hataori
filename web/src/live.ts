// useDashboard keeps the page's picture of the threads: the list from the
// server, what each thread that needs attention is waiting on, and the
// event stream that says when to read them again. The server is the only
// source; the page infers no state from the events themselves.

import { useEffect, useRef, useState } from "react";
import {
  type AgentLimits,
  type ChatLayout,
  type ClientState,
  clients,
  layout,
  type ScopeInfo,
  scopes,
  type Event,
  eventsPath,
  limits,
  SignedOut,
  type ThreadDetail,
  type ThreadInfo,
  thread,
  threads,
} from "./api";
import { failureReason } from "./failure";
import { Stream } from "./stream";

// Attention is why a thread needs attention.
export type Attention =
  | { kind: "question"; text?: string; choices?: string[] }
  | { kind: "approval"; text?: string; choices?: string[] }
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
  limits: AgentLimits[]; // each agent's remaining usage; none until read
  clients: ClientState[]; // whether each chat client is connected; none until read
  scopes: ScopeInfo[] | null; // the scopes the caller may see, with their names; null until read
  layout: ChatLayout[] | null; // the chats' channels and open threads; null until read
  // subscribe calls f with each event the stream delivers from now on,
  // until the returned function is called. One stream serves the page.
  subscribe: (f: (e: Event) => void) => () => void;
  // opens counts the times the stream has opened. A view that reads
  // history reads it again after each, so that no event between its read
  // and the stream's start is missed.
  opens: number;
}

// refreshDelay gathers the events that come together into one read.
const refreshDelay = 150;
// retryDelay is how long the page waits before it tries again to read the
// list it starts from.
const retryDelay = 2000;
// reopenDelay is how long the page waits before it opens a stream that the
// server refused again.
const reopenDelay = 2000;
// limitsInterval is how often the page reads the remaining usage again:
// it changes without an event.
const limitsInterval = 60000;
// clientsInterval is how often the page reads again whether the chat
// clients are connected: it changes without an event.
const clientsInterval = 15000;
// detailEvents is how many of a thread's last events are read for its
// failure reason.
const detailEvents = 20;

function needsAttention(t: ThreadInfo): boolean {
  return t.state === "waiting" || t.state === "failed";
}

// attentionOf reads what a waiting or failed thread is waiting on.
// A waiting thread whose request the server did not return (one that
// closed as it was read) has nothing to show yet. A failed thread's reason
// is that of its latest state change, when that is the change to failed;
// an older failure's reason is never shown as the current one.
export function attentionOf(d: ThreadDetail): Attention | undefined {
  if (d.state === "waiting") {
    const r = d.requests?.[0];
    if (!r) return undefined;
    const choices = r.choices?.map((c) => c.label);
    if (r.kind === 1) return { kind: "approval", text: r.text, choices };
    return { kind: "question", text: r.text, choices };
  }
  if (d.state === "failed") {
    const evs = d.events ?? [];
    for (let i = evs.length - 1; i >= 0; i--) {
      const e = evs[i];
      if (e.kind === "state") {
        return { kind: "failure", reason: e.data?.to === "failed" ? reasonOf(e) : undefined };
      }
    }
    return { kind: "failure" };
  }
  return undefined;
}

function reasonOf(e: Event): string | undefined {
  return failureReason(e.data);
}

// lastPosition is the position of the newest event of any of ts: the
// stream starts after it, so the page does not read the whole log again.
// The page takes its picture from the reads, not from the events, and
// reads again once the stream is open, so an event written while these
// are read changes nothing it shows. A thread that went away meanwhile
// counts as none.
async function lastPosition(ts: ThreadInfo[]): Promise<number> {
  const ds = await Promise.all(
    ts.map((x) =>
      thread(x.id, 1).catch((err) => {
        if (err instanceof SignedOut) throw err;
        return null;
      }),
    ),
  );
  return ds.reduce((p, d) => Math.max(p, d?.events?.[d.events.length - 1]?.pos ?? 0), 0);
}

type State = Omit<Dashboard, "subscribe">;

export function useDashboard(): Dashboard {
  const [state, setState] = useState<State>({
    threads: null,
    attention: new Map(),
    live: { connected: false, pos: null },
    signedOut: false,
    limits: [],
    clients: [],
    scopes: null,
    layout: null,
    opens: 0,
  });
  const reads = useRef(0);
  const listeners = useRef(new Set<(e: Event) => void>());
  const subscribe = useRef((f: (e: Event) => void) => {
    listeners.current.add(f);
    return () => {
      listeners.current.delete(f);
    };
  }).current;

  useEffect(() => {
    let closed = false;
    let source: Stream | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined; // a read to come
    let retry: ReturnType<typeof setTimeout> | undefined; // a start to try again
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
        readLimits();
        readClients();
        // The layout is read with the list: a layout event, like any other,
        // brings a read.
        const [ts, ss, ls] = await Promise.all([threads(), scopes(), layout()]);
        // A thread that goes away between the list and its detail keeps
        // the list good: it shows without what it waits on until the next
        // read.
        const details = await Promise.all(
          ts.filter(needsAttention).map((x) =>
            thread(x.id, detailEvents).catch((err) => {
              if (err instanceof SignedOut) throw err;
              return null;
            }),
          ),
        );
        if (closed || n !== reads.current) return;
        const attention = new Map<number, Attention>();
        for (const d of details) {
          const a = d && attentionOf(d);
          if (d && a) attention.set(d.id, a);
        }
        setState((s) => ({ ...s, threads: ts, scopes: ss, layout: ls, attention }));
      } catch (err) {
        if (err instanceof SignedOut) signedOut();
      }
    };
    // readLimits reads the remaining usage apart from the threads: a server
    // that cannot answer it holds nothing else back, and the last usage read
    // stays. Only the newest read is kept.
    let limitReads = 0;
    const readLimits = async () => {
      const n = ++limitReads;
      try {
        const ls = await limits();
        if (!closed && n === limitReads) setState((s) => ({ ...s, limits: ls }));
      } catch (err) {
        if (err instanceof SignedOut) signedOut();
      }
    };
    // readClients reads whether the chat clients are connected, as
    // readLimits reads the usage.
    let clientReads = 0;
    const readClients = async () => {
      const n = ++clientReads;
      try {
        const cs = await clients();
        if (!closed && n === clientReads) setState((s) => ({ ...s, clients: cs }));
      } catch (err) {
        if (err instanceof SignedOut) signedOut();
      }
    };
    // later reads once after the events of the next refreshDelay, however
    // many come: a steady flow of events does not hold the read back.
    const later = () => {
      if (timer !== undefined) return;
      timer = setTimeout(() => {
        timer = undefined;
        read();
      }, refreshDelay);
    };
    const at = (pos: number) => {
      last = Math.max(last, pos);
      setState((s) => ({ ...s, live: { connected: true, pos } }));
    };

    const open = (after: number) => {
      last = after;
      source = new Stream(`${eventsPath}?after=${after}`);
      source.onopen = () => {
        setState((s) => ({ ...s, live: { ...s.live, connected: true }, opens: s.opens + 1 }));
        later(); // what changed while the stream was down
      };
      source.onmessage = (m) => {
        const pos = Number(m.lastEventId);
        if (Number.isFinite(pos)) at(pos);
        try {
          const e = JSON.parse(m.data) as Event;
          for (const f of listeners.current) f(e);
        } catch {
          // Not an event: nothing to pass on.
        }
        later();
      };
      source.addEventListener("caught_up", (m) => {
        const pos = Number(m.lastEventId);
        if (Number.isFinite(pos)) at(pos);
      });
      source.onerror = () => {
        setState((s) => ({ ...s, live: { ...s.live, connected: false } }));
        // A stream the server refused does not come back by itself: find
        // out whether the session ended, and open it again if not.
        if (source?.readyState === Stream.CLOSED) {
          source.close();
          read().then(() => {
            if (!closed && !out) setTimeout(() => !closed && !out && open(last), reopenDelay);
          });
        }
      };
    };

    // begin finds where the stream starts and opens it; until it can, it
    // tries again.
    const begin = async () => {
      try {
        const first = await threads();
        const after = await lastPosition(first);
        if (closed) return;
        open(after);
        await read();
      } catch (err) {
        if (err instanceof SignedOut) signedOut();
        else if (!closed) retry = setTimeout(begin, retryDelay);
      }
    };
    begin();
    const poll = setInterval(readLimits, limitsInterval);
    const pollClients = setInterval(readClients, clientsInterval);

    return () => {
      closed = true;
      clearInterval(poll);
      clearInterval(pollClients);
      clearTimeout(timer);
      clearTimeout(retry);
      source?.close();
    };
  }, []);

  return { ...state, subscribe };
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
