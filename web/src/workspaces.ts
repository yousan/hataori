// useWorkspaces reads the workspace of each thread that has one to read
// (noWorkspace) from the server: when the thread first shows, when its
// state, turns or last change move, when it is selected, and, while the
// table or the details panel is shown, every minute, since a person may
// change a workspace by hand. Each read runs git on the host, so at most a
// few run at once, and of a thread's reads only the newest answer counts.

import { useEffect, useRef, useState } from "react";
import { type ThreadInfo, type WorkspaceState, workspace } from "./api";

export type Workspace = WorkspaceState | "unreadable";

// noWorkspace says why the server has no workspace of x to read: x is a
// supervisor's thread, whose directory holds no repository, or x was
// removed. Null when there is one.
export function noWorkspace(x: ThreadInfo): "supervisor" | "removed" | null {
  if (x.confined) return "supervisor";
  return x.state === "removed" ? "removed" : null;
}

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
      const queued = queue.current.shift()!;
      // It may have been removed, or be listed no more, since it was queued.
      const x = latest.current.find((y) => y.id === queued.id);
      if (!x || noWorkspace(x)) continue;
      const n = ++seq.current;
      asked.current.set(x.id, n);
      running.current++;
      workspace(x.id)
        .then((w): Workspace => w)
        .catch((): Workspace => "unreadable")
        .then((w) => {
          running.current--;
          // An older read, or one of a thread that has none any more or is
          // no longer listed, is dropped.
          const still = latest.current.some((y) => y.id === x.id && !noWorkspace(y));
          if (asked.current.get(x.id) === n && still) {
            setStates((m) => new Map(m).set(x.id, w));
          }
          pump();
        });
    }
  }).current;
  const ask = (x: ThreadInfo) => {
    if (noWorkspace(x)) return;
    queue.current = [...queue.current.filter((y) => y.id !== x.id), x];
    pump();
  };

  useEffect(() => {
    const listed = new Set((threads ?? []).filter((x) => !noWorkspace(x)).map((x) => x.id));
    for (const n of read.current.keys()) if (!listed.has(n)) read.current.delete(n);
    setStates((m) => {
      if ([...m.keys()].every((n) => listed.has(n))) return m;
      return new Map([...m].filter(([n]) => listed.has(n)));
    });
    for (const x of threads ?? []) {
      if (!listed.has(x.id)) continue;
      const m = moment(x);
      if (read.current.get(x.id) !== m) {
        read.current.set(x.id, m);
        ask(x);
      }
    }
  }, [threads]);

  useEffect(() => {
    const x = latest.current.find((y) => y.id === selected);
    if (x) ask(x);
  }, [selected]);

  useEffect(() => {
    if (!shown) return;
    const id = setInterval(() => latest.current.forEach(ask), refreshEvery);
    return () => clearInterval(id);
  }, [shown]);

  return states;
}
