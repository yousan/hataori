// useWorkspaces reads the workspace of each thread from the server: when
// the thread first shows, when its state, turns or last change move, and
// every minute, since a person may change a workspace by hand.

import { useEffect, useRef, useState } from "react";
import { type ThreadInfo, type WorkspaceState, workspace } from "./api";

export type Workspace = WorkspaceState | "unreadable";

const refreshEvery = 60000;

function moment(x: ThreadInfo): string {
  return `${x.state}|${x.updated}|${x.turns ?? 0}`;
}

export function useWorkspaces(threads: ThreadInfo[] | null): ReadonlyMap<number, Workspace> {
  const [states, setStates] = useState<ReadonlyMap<number, Workspace>>(new Map());
  const read = useRef(new Map<number, string>()); // the moment each thread was read at
  const latest = useRef<ThreadInfo[]>([]);
  latest.current = threads ?? [];

  const fetchOne = (x: ThreadInfo) => {
    workspace(x.id)
      .then((w): Workspace => w)
      .catch((): Workspace => "unreadable")
      .then((w) =>
        setStates((m) => {
          const next = new Map(m);
          next.set(x.number, w);
          return next;
        }),
      );
  };

  useEffect(() => {
    for (const x of threads ?? []) {
      const m = moment(x);
      if (read.current.get(x.number) !== m) {
        read.current.set(x.number, m);
        fetchOne(x);
      }
    }
  }, [threads]);

  useEffect(() => {
    const id = setInterval(() => latest.current.forEach(fetchOne), refreshEvery);
    return () => clearInterval(id);
  }, []);

  return states;
}
