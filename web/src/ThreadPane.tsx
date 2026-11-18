import { useEffect, useState } from "react";
import { type Event, hasTerminal, type ThreadInfo } from "./api";
import { t } from "./catalog";
import { EventsView } from "./EventsView";
import { TerminalView } from "./TerminalView";

// ThreadPane is a thread's pane: its live terminal, or its events when the
// session has no terminal.
export function ThreadPane(props: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void }) {
  const { x, subscribe } = props;
  const [terminal, setTerminal] = useState<boolean | null>(null);
  const { scope, key } = x.id;

  useEffect(() => {
    let gone = false;
    setTerminal(null);
    hasTerminal({ scope, key })
      .then((ok) => !gone && setTerminal(ok))
      .catch(() => !gone && setTerminal(false));
    return () => {
      gone = true;
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
