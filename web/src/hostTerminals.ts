// useHostTerminals lists the terminals on the host that hataori does not
// own, as the server gives them, again every few seconds. null means the
// server does not list them: the feature is off, or the caller is not the
// owner; then the page shows nothing of them.

import { useEffect, useState } from "react";
import { type HostTerminal, HostTerminalsOff, hostTerminals } from "./api";
import { t } from "./catalog";

const every = 10000;

// hostID names a host terminal for the page: its device, and for a tmux
// pane the pane's id too, so that a tab of a pane that went away never
// shows another terminal that later gets the same device.
export function hostID(h: HostTerminal): string {
  return h.tmux ? `${h.device}#${h.tmux.pane}` : h.device;
}

// offEvery is how often the page asks again after the server said the
// feature is off, so that turning it on shows without a reload.
const offEvery = 60000;

// useHostTerminals returns the list, and how far the host's clock is ahead
// of this browser's (hostTerminals), so that a time of the list is
// measured on the host's clock.
export function useHostTerminals(): [HostTerminal[] | null, number] {
  const [list, setList] = useState<HostTerminal[] | null>(null);
  const [skew, setSkew] = useState(0);
  useEffect(() => {
    let gone = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () =>
      hostTerminals()
        .then(({ list: ts, skew }) => {
          if (gone) return;
          setList(ts);
          setSkew(skew);
          timer = setTimeout(read, every);
        })
        .catch((err) => {
          if (gone) return;
          if (err instanceof HostTerminalsOff) {
            setList(null); // off: nothing is shown; asked again now and then
            timer = setTimeout(read, offEvery);
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
  return [list, skew];
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
