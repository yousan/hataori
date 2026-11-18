// useWorkspaces reads the workspace of each thread from the server: when
// the thread first shows, when its state, turns or last change move, when
// it is selected, and, while the table or the details panel is shown,
// every minute, since a person may change a workspace by hand. Each read runs git on the host, so at most a few run
// at once, and of a thread's reads only the newest answer counts.

import { useEffect, useRef, useState } from "react";
import { type ThreadInfo, type WorkspaceState, workspace } from "./api";

export type Workspace = WorkspaceState | "unreadable";

const refreshEvery = 60000;
const atOnce = 4;

function moment(x: ThreadInfo): string {
  return `${x.state}|${x.updated}|${x.turns ?? 0}`;
}

export function useWorkspaces(threads: ThreadInfo[] | null, shown: boolean, selected: number | null): ReadonlyMap<number, Workspace> {
  const [states, setStates] = useState<ReadonlyMap<number, Workspace>>(new Map());
  const read = useRef(new Map<number, string>()); // the moment each thread was last asked at
  const asked = useRef(new Map<number, number>()); // the newest read of each thread
  const latest = useRef<ThreadInfo[]>([]);
  latest.current = threads ?? [];
  const queue = useRef<ThreadInfo[]>([]);
  const running = useRef(0);
  const seq = useRef(0);

  // pump starts queued reads while fewer than atOnce run.
  const pump = useRef(() => {
    while (running.current < atOnce && queue.current.length > 0) {
      const x = queue.current.shift()!;
      const n = ++seq.current;
      asked.current.set(x.number, n);
      running.current++;
      workspace(x.id)
        .then((w): Workspace => w)
        .catch((): Workspace => "unreadable")
        .then((w) => {
          running.current--;
          // An older read, or one of a thread that is no longer listed, is dropped.
          const still = latest.current.some((y) => y.number === x.number && y.id.scope === x.id.scope && y.id.key === x.id.key);
          if (asked.current.get(x.number) === n && still) {
            setStates((m) => new Map(m).set(x.number, w));
          }
          pump();
        });
    }
  }).current;
  const ask = (x: ThreadInfo) => {
    queue.current = [...queue.current.filter((y) => y.number !== x.number), x];
    pump();
  };

  useEffect(() => {
    const listed = new Set((threads ?? []).map((x) => x.number));
    for (const n of read.current.keys()) if (!listed.has(n)) read.current.delete(n);
    setStates((m) => {
      if ([...m.keys()].every((n) => listed.has(n))) return m;
      return new Map([...m].filter(([n]) => listed.has(n)));
    });
    for (const x of threads ?? []) {
      const m = moment(x);
      if (read.current.get(x.number) !== m) {
        read.current.set(x.number, m);
        ask(x);
      }
    }
  }, [threads]);

  useEffect(() => {
    const x = latest.current.find((y) => y.number === selected);
    if (x) ask(x);
  }, [selected]);

  useEffect(() => {
    if (!shown) return;
    const id = setInterval(() => latest.current.forEach(ask), refreshEvery);
    return () => clearInterval(id);
  }, [shown]);

  return states;
}
