import type { Event, ThreadInfo } from "./api";
import { lang, t } from "./catalog";
import { stateName } from "./format";
import type { Attention } from "./live";
import { eventLine, useThreadEvents } from "./threadEvents";
import type { Workspace } from "./workspaces";

const numbers = new Intl.NumberFormat(lang);
const dash = () => t("table.none");

// recentEvents is how many of the thread's events the panel shows.
const recentEvents = 20;
const eventOptions = { history: recentEvents, keep: recentEvents };

function Field(props: { name: string; label: string; value: string; time?: string }) {
  return (
    <div className="field" data-field={props.name}>
      <dt>{props.label}</dt>
      <dd>{props.time ? <time dateTime={props.time}>{props.value}</time> : props.value}</dd>
    </div>
  );
}

// Waiting is what the thread waits on: an open question with its choices,
// shown as text only and with nothing that answers, or the failure reason.
function Waiting({ a }: { a: Attention }) {
  if (a.kind === "failure") {
    return (
      <section className="details-top failure" data-block="failure">
        <h3>{t("details.failure")}</h3>
        <p data-field="reason">{a.reason || t("details.failureUnknown")}</p>
      </section>
    );
  }
  return (
    <section className="details-top question" data-block="question">
      <h3>{t(a.kind === "approval" ? "details.approval" : "details.question")}</h3>
      <p data-field="question">{a.text || t("details.questionHidden")}</p>
      {a.choices && a.choices.length > 0 && (
        <>
          <h4>{t("details.choices")}</h4>
          <ul className="choices" data-field="choices">
            {a.choices.map((c, i) => (
              <li key={i}>{c}</li>
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

function Recent({ x, subscribe }: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void }) {
  const events = useThreadEvents(x.id, subscribe, eventOptions);
  return (
    <section data-block="events">
      <h3>{t("details.events")}</h3>
      {events.length === 0 ? (
        <p className="empty">{t("details.noEvents")}</p>
      ) : (
        <ol className="recent">
          {[...events].reverse().map((e) => (
            <li key={e.pos} data-kind={e.kind} data-pos={e.pos}>
              <time dateTime={e.time}>{new Date(e.time).toLocaleTimeString()}</time>
              <span className="recent-line">{eventLine(e)}</span>
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}

interface Props {
  x: ThreadInfo | undefined;
  attention: Attention | undefined;
  workspace: Workspace | undefined;
  subscribe: (f: (e: Event) => void) => () => void;
}

// Details is the panel on the right for the active tab's thread. Every
// value comes from the control API; one it leaves out is a dash. It only
// shows: nothing in it answers, sends or changes anything.
export function Details({ x, attention, workspace, subscribe }: Props) {
  if (!x) return <p className="empty">{t("details.none")}</p>;
  const ws = workspace && workspace !== "unreadable" ? workspace : undefined;
  const pct = x.context && x.context.size > 0 ? Math.round((100 * x.context.used) / x.context.size) : null;
  const n = (v: number | undefined) => (v === undefined ? dash() : numbers.format(v));
  return (
    <div className="details-body" data-details={x.number}>
      {attention && <Waiting a={attention} />}
      <section data-block="thread">
        <h3>{t("details.thread")}</h3>
        <dl>
          <Field name="state" label={t("details.state")} value={stateName(x.state)} />
          <Field name="scope" label={t("details.scope")} value={x.id.scope || dash()} />
          <Field name="startedBy" label={t("details.startedBy")} value={x.started_by || dash()} />
          <Field name="created" label={t("details.created")} value={new Date(x.created).toLocaleString()} time={x.created} />
          <Field name="turns" label={t("details.turns")} value={n(x.turns)} />
        </dl>
      </section>
      <section data-block="agent">
        <h3>{t("details.agent")}</h3>
        <dl>
          <Field name="agent" label={t("details.kind")} value={x.agent || dash()} />
          <Field name="model" label={t("details.model")} value={x.model || dash()} />
          <Field name="effort" label={t("details.effort")} value={x.effort || dash()} />
          <Field
            name="context"
            label={t("details.context")}
            value={
              pct === null || !x.context
                ? dash()
                : t("details.contextValue", { pct, used: numbers.format(x.context.used), size: numbers.format(x.context.size) })
            }
          />
          <Field name="tokensTurn" label={t("details.tokensTurn")} value={n(x.tokens?.turn)} />
          <Field name="tokensToday" label={t("details.tokensToday")} value={n(x.tokens?.today)} />
          <Field name="background" label={t("details.background")} value={n(x.background)} />
        </dl>
      </section>
      <section data-block="workspace">
        <h3>{t("details.workspace")}</h3>
        <dl>
          <Field name="repo" label={t("details.repo")} value={x.repo || dash()} />
          <Field name="branch" label={t("details.branch")} value={ws?.branch || dash()} />
          <Field name="lines" label={t("details.lines")} value={ws ? t("details.linesValue", { added: ws.added, removed: ws.removed }) : dash()} />
          <Field name="uncommitted" label={t("details.uncommitted")} value={ws ? numbers.format(ws.uncommitted) : dash()} />
          <Field name="unpushed" label={t("details.unpushed")} value={ws ? numbers.format(ws.unpushed) : dash()} />
          <Field name="terminal" label={t("details.terminal")} value={x.terminal || dash()} />
        </dl>
      </section>
      <Recent key={x.number} x={x} subscribe={subscribe} />
    </div>
  );
}
