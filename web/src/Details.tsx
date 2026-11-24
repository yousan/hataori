import type { ChatLayout, Event, ScopeInfo, ThreadInfo } from "./api";
import { lang, t } from "./catalog";
import { displayTitle, fullTitle, scopeName, stateName } from "./format";
import type { Attention } from "./live";
import { eventLine, useThreadEvents } from "./threadEvents";
import { noWorkspace, type Workspace } from "./workspaces";

const numbers = new Intl.NumberFormat(lang);
const dash = () => t("table.none");

// recentEvents is how many of the thread's events the panel shows.
const recentEvents = 20;
const eventOptions = { history: recentEvents, keep: recentEvents };

function Field(props: { name: string; label: string; value: string; time?: string; tip?: string }) {
  return (
    <div className="field" data-field={props.name}>
      <dt>{props.label}</dt>
      <dd title={props.tip || undefined}>{props.time ? <time dateTime={props.time}>{props.value}</time> : props.value}</dd>
    </div>
  );
}

// Waiting is what the thread waits on: an open question with its choices,
// shown as text only and with nothing that answers, or the failure's full
// reason, folded: the pane says it in one sentence (FailedStart.tsx).
export function Waiting({ a }: { a: Attention }) {
  if (a.kind === "failure") {
    return (
      <section className="details-top failure" data-block="failure">
        <h3>{t("details.failure")}</h3>
        {a.reason ? (
          <details>
            <summary>{t("details.failureFull")}</summary>
            <p data-field="reason">{a.reason}</p>
          </details>
        ) : (
          <p data-field="reason">{t("details.failureUnknown")}</p>
        )}
      </section>
    );
  }
  return (
    <section className="details-top question" data-block="question">
      <h3>{t(a.kind === "approval" ? "details.approval" : "details.question")}</h3>
      <p data-field="question">{a.text || t(a.kind === "approval" ? "details.approvalHidden" : "details.questionHidden")}</p>
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

function Recent({ x, subscribe, opens }: { x: ThreadInfo; subscribe: (f: (e: Event) => void) => () => void; opens: number }) {
  const { events, read } = useThreadEvents(x.id, subscribe, opens, eventOptions);
  return (
    <section data-block="events">
      <h3>{t("details.events")}</h3>
      {events.length === 0 ? (
        read && <p className="empty">{t("details.noEvents")}</p>
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
  opens: number;
  scopes: ScopeInfo[] | null; // their names; null until read
  layout: ChatLayout[] | null; // the chats' channels, whose names a scope without one takes
  // withWaiting shows what the thread waits on at the top; a view that
  // shows it elsewhere leaves it out here.
  withWaiting?: boolean;
}

// Details is the panel on the right for the active tab's thread. Every
// value comes from the control API; one it leaves out is a dash. It only
// shows: nothing in it answers, sends or changes anything.
export function Details({ x, attention, workspace, subscribe, opens, scopes, layout, withWaiting = true }: Props) {
  if (!x) return <p className="empty">{t("details.none")}</p>;
  const ws = workspace && workspace !== "unreadable" ? workspace : undefined;
  const none = noWorkspace(x);
  const pct = x.context && x.context.size > 0 ? Math.round((100 * x.context.used) / x.context.size) : null;
  const n = (v: number | undefined) => (v === undefined ? dash() : numbers.format(v));
  return (
    <div className="details-body" data-details={x.id}>
      {withWaiting && attention && <Waiting a={attention} />}
      <section data-block="thread">
        <h3>{t("details.thread")}</h3>
        <dl>
          <Field name="title" label={t("details.title")} value={displayTitle(x) || dash()} tip={fullTitle(x)} />
          <Field name="state" label={t("details.state")} value={stateName(x.state)} />
          <Field name="scope" label={t("details.scope")} value={x.scope ? scopeName(x.scope, scopes, layout) : dash()} tip={x.scope} />
          <Field
            name="startedBy"
            label={t("details.startedBy")}
            value={attention?.kind === "failure" && attention.supervisor !== undefined ? t("start.supervisor", { n: attention.supervisor }) : x.started_by || dash()}
          />
          <Field name="created" label={t("details.created")} value={new Date(x.created).toLocaleString()} time={x.created} />
          {/* turns is left out for none: that is zero, not unknown */}
          <Field name="turns" label={t("details.turns")} value={numbers.format(x.turns ?? 0)} />
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
        {none && (
          <p className="empty" data-field="noWorkspace" data-why={none}>
            {t(none === "supervisor" ? "details.noRepo" : "details.workspaceRemoved")}
          </p>
        )}
        <dl>
          {none !== "supervisor" && <Field name="repo" label={t("details.repo")} value={x.repo || dash()} />}
          {!none && (
            <>
              <Field name="branch" label={t("details.branch")} value={ws?.branch || dash()} />
              <Field name="lines" label={t("details.lines")} value={ws ? t("details.linesValue", { added: ws.added, removed: ws.removed }) : dash()} />
              <Field name="uncommitted" label={t("details.uncommitted")} value={ws ? numbers.format(ws.uncommitted) : dash()} />
              <Field name="unpushed" label={t("details.unpushed")} value={ws ? numbers.format(ws.unpushed) : dash()} />
            </>
          )}
          <Field name="terminal" label={t("details.terminal")} value={x.terminal || dash()} />
        </dl>
      </section>
      <Recent key={x.id} x={x} subscribe={subscribe} opens={opens} />
    </div>
  );
}
