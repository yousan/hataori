import { useEffect, useState } from "react";
import { type Event, hasTerminal, type ThreadInfo } from "./api";
import type { AttachState } from "./attach";
import { type Key, t } from "./catalog";
import { Conversation } from "./Conversation";
import { InputBox } from "./InputBox";
import { Answers, useOpenRequests } from "./Answers";
import { TerminalView, type Typing } from "./TerminalView";

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

// AttachSwitch is the web listener's switch of a terminal thread's tab
// (attach.ts): its attachment, if any, and what the switch does.
export interface AttachSwitch {
  state?: AttachState;
  onSwitch: () => void;
}

// attachEndKey is the catalog's words for why an attachment ended.
function attachEndKey(s: AttachState): Key {
  switch (s.reason) {
    case "idle":
      return "attach.end.idle";
    case "ended":
      return "attach.end.ended";
    case "agent ended":
      return "attach.end.agent";
    case "session ended":
    case "signIn":
      return "attach.end.signIn";
    case "stopping":
      return "attach.end.stopping";
    case "passkeyCancelled":
      return "attach.end.cancelled";
    case "refused":
      return "attach.end.refused";
  }
  return "attach.end.lost";
}

// Switch is the switch that attaches the terminal on the web listener.
function Switch({ a }: { a: AttachSwitch }) {
  const s = a.state;
  const on = s?.phase === "on" || s?.phase === "starting";
  return (
    <div className="attach" data-attach={s ? s.phase : "off"}>
      <button type="button" role="switch" aria-checked={on} onClick={a.onSwitch} title={t(on ? "attach.offTitle" : "attach.onTitle")}>
        {t("attach.switch")}
      </button>
      {s?.phase === "starting" && <span className="attach-note">{t("attach.starting")}</span>}
      {s?.phase === "on" && <span className="attach-note">{t("attach.on")}</span>}
      {s?.phase === "ended" && (
        <span className="attach-note" data-attach-end={s.reason ?? ""}>
          {t(attachEndKey(s), { detail: s.detail ?? "" })}
        </span>
      )}
    </div>
  );
}

// ThreadPane is a thread's pane: its live terminal or its conversation,
// switched in the pane; a session without a terminal has only the
// conversation. view is the person's choice for this tab, or the view the
// tab opened on. With fit, the terminal fills the pane. typing lets the
// terminal take keys (attach.ts); attach, on the web listener, is its
// switch. A session without a terminal, and every thread in the narrow
// layout, has the input box, for a person who may act.
export function ThreadPane(props: {
  x: ThreadInfo;
  subscribe: (f: (e: Event) => void) => () => void;
  opens: number;
  fit?: boolean;
  view?: View;
  onView?: (v: View) => void;
  typing?: Typing;
  attach?: AttachSwitch;
  mayAct?: boolean; // the server lets this person give the thread input
  narrow?: boolean; // the narrow layout, where every thread has the input box
}) {
  const { x, subscribe, opens, fit, onView, typing, attach, mayAct, narrow } = props;
  const [terminal, setTerminal] = useState<boolean | null>(null);
  const id = x.id;
  // Each open question's and approval's buttons, for a person who may act;
  // a refused answer's reason stays after them, until another request
  // opens. The box answers the open question, if any.
  const requests = useOpenRequests(x, subscribe, opens);
  const question = requests.find((r) => !r.approval) ?? null;
  const [refused, setRefused] = useState<{ request: string; reason: string } | null>(null);
  useEffect(() => {
    if (refused && requests.some((r) => r.id !== refused.request)) setRefused(null);
  }, [requests, refused]);

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
      {terminal && view === "terminal" && attach && <Switch a={attach} />}
      {view === "terminal" ? (
        <TerminalView x={x} fit={fit} typing={terminal ? typing : undefined} />
      ) : (
        <Conversation x={x} subscribe={subscribe} opens={opens} />
      )}
      {mayAct &&
        requests.map((r) => (
          <Answers key={r.id} r={r} field={r === question && !(narrow || !terminal)} onRefused={(reason) => setRefused({ request: r.id, reason })} />
        ))}
      {mayAct && refused && (
        <p className="box-refused" role="status" data-answer-refused="">
          {t("answers.refused", { reason: refused.reason })}
        </p>
      )}
      {mayAct && (narrow || !terminal) && <InputBox key={x.id} x={x} question={question} />}
    </div>
  );
}
