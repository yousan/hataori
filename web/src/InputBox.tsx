import { useEffect, useRef, useState } from "react";
import { act, newKey, sendPath } from "./act";
import type { ThreadInfo } from "./api";
import { t } from "./catalog";

// InputBox is the box that sends a thread input from its tab (design,
// section 7.2): for a session without a terminal, and for every thread in
// the narrow layout. It keeps several lines: Enter sends and Shift+Enter
// starts a new line; its button sends too, as a phone needs. A message
// goes with Send, delivered into a running turn as a chat message is, with
// a key of the page's own: a send that failed keeps its text and its key,
// so that sending it again gives the agent the message once.
export function InputBox({ x }: { x: ThreadInfo }) {
  const [text, setText] = useState("");
  const [key, setKey] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [refused, setRefused] = useState<string | null>(null);
  const gone = useRef(false);
  useEffect(
    () => () => {
      gone.current = true;
    },
    [],
  );

  const send = async () => {
    if (sending || text.trim() === "") return;
    const k = key ?? newKey();
    const sent = text;
    setKey(k);
    setSending(true);
    setRefused(null);
    const r = await act(sendPath, { thread: x.id, key: k, text: sent });
    if (gone.current) return;
    setSending(false);
    if (r.ok) {
      // What was typed while it went stays, for the next message.
      setText((now) => (now === sent ? "" : now));
      setKey((now) => (now === k ? null : now));
      return;
    }
    setRefused(r.check === "signIn" ? t("box.signIn") : r.check === "passkeyCancelled" ? t("box.cancelled") : r.reason || t("box.failed"));
  };

  return (
    <div className="input-box" data-input-box={x.id} data-box-running={x.state === "running" ? "yes" : "no"}>
      {x.state === "running" && <p className="box-note">{t("box.running")}</p>}
      <div className="box-row">
        <textarea
          aria-label={t("box.label")}
          placeholder={t("box.placeholder")}
          rows={2}
          value={text}
          onChange={(e) => {
            setText(e.target.value);
            setKey(null); // another message: another key
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <button type="button" className="box-send" disabled={sending || text.trim() === ""} onClick={() => void send()}>
          {t("box.send")}
        </button>
      </div>
      {refused !== null && (
        <p className="box-refused" role="status" data-box-refused="">
          {t("box.notSent", { reason: refused })}
        </p>
      )}
    </div>
  );
}
