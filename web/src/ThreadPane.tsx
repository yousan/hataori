import { useEffect, useState } from "react";
import { type Event, hasTerminal, type ThreadInfo } from "./api";
import { t } from "./catalog";
import { EventsView } from "./EventsView";
import { TerminalView } from "./TerminalView";

// probeRetry is how long a pane waits before it asks again whether its
// session has a terminal.
const probeRetry = 2000;

// ThreadPane is a thread's pane: its live terminal, or its events when the
// session has no terminal.
export function ThreadPane(props: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void }) {
  const { x, subscribe } = props;
  const [terminal, setTerminal] = useState<boolean | null>(null);
  const { scope, key } = x.id;

  // Only an answer of the server decides the view; a failed probe, such as
  // while the daemon restarts, is tried again.
  useEffect(() => {
    let gone = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    setTerminal(null);
    const probe = () =>
      hasTerminal({ scope, key })
        .then((ok) => !gone && setTerminal(ok))
        .catch(() => {
          if (!gone) retry = setTimeout(probe, probeRetry);
        });
    probe();
    return () => {
      gone = true;
      clearTimeout(retry);
    };
  }, [scope, key]);

  if (terminal === null) return null;
  return (
    <div className="thread-pane">
      {!terminal && x.state === "asleep" && (
        <p className="no-process" data-no-process="">
          {t("pane.noProcess")}
        </p>
      )}
      {terminal ? <TerminalView x={x} /> : <EventsView x={x} subscribe={subscribe} />}
    </div>
  );
}
