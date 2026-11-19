import { useEffect, useState } from "react";
import { type Event, hasTerminal, type ThreadInfo } from "./api";
import { t } from "./catalog";
import { Conversation } from "./Conversation";
import { TerminalView } from "./TerminalView";

// probeRetry is how long a pane waits before it asks again whether its
// session has a terminal.
const probeRetry = 2000;

// View is what a thread's pane shows: its terminal or its conversation.
export type View = "terminal" | "conversation";

// defaultView is the view a thread's tab opens on: the conversation of a
// thread with no process to show (asleep, closed, removed, failed), the
// terminal of one with a running process.
export function defaultView(x: ThreadInfo): View {
  return x.state === "asleep" || x.state === "closed" || x.state === "removed" || x.state === "failed" ? "conversation" : "terminal";
}

// ThreadPane is a thread's pane: its live terminal or its conversation,
// switched in the pane; a session without a terminal has only the
// conversation. view is the person's choice for this tab, or the view the
// tab opened on. With fit, the terminal fills the pane.
export function ThreadPane(props: {
  x: ThreadInfo;
  subscribe: (f: (e: Event) => void) => () => void;
  opens: number;
  fit?: boolean;
  view?: View;
  onView?: (v: View) => void;
}) {
  const { x, subscribe, opens, fit, onView } = props;
  const [terminal, setTerminal] = useState<boolean | null>(null);
  const id = x.id;

  // Only an answer of the server decides the view; a failed probe, such as
  // while the daemon restarts, is tried again.
  useEffect(() => {
    let gone = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    setTerminal(null);
    const probe = () =>
      hasTerminal(id)
        .then((ok) => !gone && setTerminal(ok))
        .catch(() => {
          if (!gone) retry = setTimeout(probe, probeRetry);
        });
    probe();
    return () => {
      gone = true;
      clearTimeout(retry);
    };
  }, [id]);

  if (terminal === null) return null;
  const view: View = terminal ? (props.view ?? defaultView(x)) : "conversation";
  return (
    <div className="thread-pane" data-view={view}>
      {terminal && (
        <div className="view-switch" role="group" aria-label={t("view.label")}>
          {(["terminal", "conversation"] as const).map((v) => (
            <button key={v} type="button" data-view={v} aria-pressed={view === v} onClick={() => onView?.(v)}>
              {t(v === "terminal" ? "view.terminal" : "view.conversation")}
            </button>
          ))}
        </div>
      )}
      {!terminal && x.state === "asleep" && (
        <p className="no-process" data-no-process="">
          {t("pane.noProcess")}
        </p>
      )}
      {view === "terminal" ? <TerminalView x={x} fit={fit} /> : <Conversation x={x} subscribe={subscribe} opens={opens} />}
    </div>
  );
}
