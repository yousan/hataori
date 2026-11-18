// useHostTerminals lists the terminals on the host that hataori does not
// own, as the server gives them, again every few seconds. null means the
// server does not list them: the feature is off, or the caller is not the
// owner; then the page shows nothing of them.

import { useEffect, useState } from "react";
import { type HostTerminal, HostTerminalsOff, hostTerminals } from "./api";
import { t } from "./catalog";

const every = 10000;

export function useHostTerminals(): HostTerminal[] | null {
  const [list, setList] = useState<HostTerminal[] | null>(null);
  useEffect(() => {
    let gone = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () =>
      hostTerminals()
        .then((ts) => {
          if (gone) return;
          setList(ts);
          timer = setTimeout(read, every);
        })
        .catch((err) => {
          if (gone) return;
          if (err instanceof HostTerminalsOff) {
            setList(null); // off: not asked again until the page loads again
            return;
          }
          timer = setTimeout(read, every);
        });
    read();
    return () => {
      gone = true;
      clearTimeout(timer);
    };
  }, []);
  return list;
}

// hostLabel names a host terminal: its tmux session and window, or its
// device, with its process.
export function hostLabel(h: HostTerminal): string {
  const process = h.process || t("table.none");
  if (h.tmux) {
    return t("host.paneLabel", { session: h.tmux.session, window: h.tmux.window_name || h.tmux.window, process });
  }
  return t("host.otherLabel", { device: h.device, process });
}

// hostMatches reports whether h matches the filter q: its device, process,
// tmux session or window name.
export function hostMatches(h: HostTerminal, q: string): boolean {
  const f = q.trim().toLowerCase();
  if (f === "") return true;
  return [h.device, h.process, h.tmux?.session, h.tmux?.window_name].some((v) => !!v && v.toLowerCase().includes(f));
}
