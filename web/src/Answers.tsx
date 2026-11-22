import { useEffect, useRef, useState } from "react";
import { act, answerPath } from "./act";
import { type Event, thread, type ThreadInfo } from "./api";
import { t } from "./catalog";

// OpenRequest is a thread's open question or approval, as the page answers
// it (design, section 7.2, "Answering").
export interface OpenRequest {
  id: string;
  approval: boolean;
  text?: string;
  choices: { label: string; token: string }[];
  multi: boolean; // several choices may be chosen at once
}

// detailEvents is how many of a thread's last events are read to find
// whether its question takes several choices: the request event says so.
const detailEvents = 40;

// requestKinds are the events of a thread after which its open request is
// read again.
const requestKinds: ReadonlySet<string> = new Set([
  "request",
  "request_withdrawn",
  "answer",
  "approval_asked",
  "approval_answered",
  "state",
  "turn_ended",
]);

// useOpenRequest is the thread's open request, if any, read again when an
// event of the thread may have opened or closed one. A question waits in a
// waiting thread; an approval a supervisor asks for waits in the
// supervisor's own thread, whatever its state.
export function useOpenRequest(x: ThreadInfo, subscribe: (f: (e: Event) => void) => () => void): OpenRequest | null {
  const [req, setReq] = useState<OpenRequest | null>(null);
  const id = x.id;
  const seq = useRef(0);
  useEffect(() => {
    let gone = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      const n = ++seq.current;
      thread(id, detailEvents)
        .then((d) => {
          if (gone || n !== seq.current) return;
          const r = d.requests?.[0];
          if (!r) return setReq(null);
          const asked = (d.events ?? []).find((e) => e.kind === "request" && e.data?.request === r.id);
          setReq({ id: r.id, approval: r.kind === 1, text: r.text, choices: r.choices ?? [], multi: asked?.data?.multi === true });
        })
        .catch(() => {});
    };
    read();
    const off = subscribe((e) => {
      if (e.thread !== id || !requestKinds.has(e.kind)) return;
      clearTimeout(timer);
      timer = setTimeout(read, 100); // events that come together, one read
    });
    return () => {
      gone = true;
      clearTimeout(timer);
      off();
    };
  }, [id, subscribe, x.state]);
  return req;
}

// Answers is an open request's way to answer from the tab: a question's
// choices as buttons, or checks and one button when several may be chosen;
// an approval's Allow and Deny; and, where the pane has no input box (a
// terminal thread in the wide layout, whose hook holds the question), a
// field of several lines for a free answer. An answer the server refuses,
// as one to a question answered elsewhere just before, goes to onRefused
// with its reason, which the pane keeps showing after the buttons go, once
// the request closes.
export function Answers({ r, field, onRefused }: { r: OpenRequest; field: boolean; onRefused: (reason: string) => void }) {
  const [checked, setChecked] = useState<ReadonlySet<string>>(new Set());
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const gone = useRef(false);
  useEffect(
    () => () => {
      gone.current = true;
    },
    [],
  );
  const answer = async (body: Record<string, unknown>) => {
    if (busy) return;
    setBusy(true);
    const a = await act(answerPath, { request: r.id, ...body });
    if (!a.ok) onRefused(a.reason || t("box.failed")); // also once the buttons are gone
    if (gone.current) return;
    setBusy(false);
    if (a.ok) setText("");
  };
  return (
    <div className="answers" data-answers={r.id} data-approval={r.approval ? "yes" : "no"}>
      {r.text && <p className="answers-text">{r.text}</p>}
      <div className="answers-row">
        {r.approval ? (
          <>
            <button type="button" data-allow="yes" disabled={busy} onClick={() => void answer({ allow: true })}>
              {t("answers.allow")}
            </button>
            <button type="button" data-allow="no" disabled={busy} onClick={() => void answer({ allow: false })}>
              {t("answers.deny")}
            </button>
          </>
        ) : r.multi ? (
          <>
            {r.choices.map((c) => (
              <label key={c.token} className="answers-check">
                <input
                  type="checkbox"
                  data-choice={c.label}
                  checked={checked.has(c.token)}
                  onChange={(e) => {
                    const next = new Set(checked);
                    if (e.target.checked) next.add(c.token);
                    else next.delete(c.token);
                    setChecked(next);
                  }}
                />
                {c.label}
              </label>
            ))}
            <button type="button" data-answer-checked="" disabled={busy || checked.size === 0} onClick={() => void answer({ choices: r.choices.filter((c) => checked.has(c.token)).map((c) => c.token) })}>
              {t("answers.answer")}
            </button>
          </>
        ) : (
          r.choices.map((c) => (
            <button key={c.token} type="button" data-choice={c.label} disabled={busy} onClick={() => void answer({ choices: [c.token] })}>
              {c.label}
            </button>
          ))
        )}
      </div>
      {field && !r.approval && (
        <div className="answers-field">
          <textarea
            aria-label={t("answers.fieldLabel")}
            placeholder={t("answers.field")}
            rows={2}
            value={text}
            data-answer-field=""
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault();
                if (text.trim() !== "") void answer({ text });
              }
            }}
          />
          <button type="button" className="box-send" disabled={busy || text.trim() === ""} onClick={() => void answer({ text })}>
            {t("answers.answer")}
          </button>
        </div>
      )}
    </div>
  );
}
