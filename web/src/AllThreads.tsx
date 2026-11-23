import type { ReactNode } from "react";
import type { ScopeInfo, ThreadInfo } from "./api";
import { type Key, lang, t } from "./catalog";
import { age, displayTitle, fullTitle, scopeLabel, stateName } from "./format";
import { type OpenInChat, type Show, shows } from "./show";
import type { Workspace } from "./workspaces";

const numbers = new Intl.NumberFormat(lang);

// dash is what a cell shows for a value the control API leaves out.
const dash = () => t("table.none");

interface Props {
  threads: ThreadInfo[] | null;
  workspaces: ReadonlyMap<number, Workspace>;
  now: number;
  selected: number | null;
  onOpen: (n: number) => void;
  show: Show; // the active threads or all of them
  open: OpenInChat | null; // what the chats' layout says is open in the chat
  onShow: (s: Show) => void;
  scopes: ScopeInfo[] | null; // their names, for the groups' headers; null until read
}

type Column = "state" | "number" | "title" | "agent" | "age" | "context" | "tokens" | "branch" | "changes" | "unpushed" | "last";

const columns: { col: Column; head: Key }[] = [
  { col: "state", head: "table.state" },
  { col: "number", head: "table.number" },
  { col: "title", head: "table.title" },
  { col: "agent", head: "table.agent" },
  { col: "age", head: "table.age" },
  { col: "context", head: "table.context" },
  { col: "tokens", head: "table.tokens" },
  { col: "branch", head: "table.branch" },
  { col: "changes", head: "table.changes" },
  { col: "unpushed", head: "table.unpushed" },
  { col: "last", head: "table.last" },
];

// readable is the workspace of a thread when it could be read.
function readable(w: Workspace | undefined) {
  return w && w !== "unreadable" ? w : undefined;
}

// contextPct is a thread's context use in percent, or null when the agent
// has not reported it.
function contextPct(x: ThreadInfo): number | null {
  return x.context && x.context.size > 0 ? Math.round((100 * x.context.used) / x.context.size) : null;
}

// hasValue reports whether the cell of column c has a value for x, rather
// than the dash: a column with none for any thread shown takes no space.
function hasValue(c: Column, x: ThreadInfo, w: Workspace | undefined): boolean {
  switch (c) {
    case "title":
      return !!displayTitle(x);
    case "agent":
      return !!(x.agent || x.model);
    case "context":
      return contextPct(x) !== null;
    case "tokens":
      return !!x.tokens;
    case "branch":
      return !!readable(w)?.branch;
    case "changes":
    case "unpushed":
      return !!readable(w);
    case "last":
      return !!x.last;
  }
  return true; // state, number, age: every thread has them
}

function Row(props: { x: ThreadInfo; w: Workspace | undefined; now: number; selected: boolean; onOpen: (n: number) => void; shown: ReadonlySet<Column> }) {
  const { x, w, now, selected, onOpen, shown } = props;
  const ws = readable(w);
  const pct = contextPct(x);
  const cell = (c: Column, td: ReactNode) => (shown.has(c) ? td : null);
  return (
    <tr
      className={selected ? "selected" : undefined}
      data-thread={x.id}
      data-state={x.state}
      aria-current={selected ? "true" : undefined}
      onClick={() => onOpen(x.id)}
    >
      <td data-col="state">
        <span className={`dot state-${x.state}`} title={stateName(x.state)} />
      </td>
      <td data-col="number">
        <button
          type="button"
          className="row-open"
          aria-label={t("table.open", { n: x.id })}
          onClick={(e) => {
            e.stopPropagation();
            onOpen(x.id);
          }}
        >
          {t("thread.number", { n: x.id })}
        </button>
      </td>
      {cell(
        "title",
        <td data-col="title" className="cell-title" title={fullTitle(x) || undefined}>
          {displayTitle(x) || dash()}
        </td>,
      )}
      {cell(
        "agent",
        <td data-col="agent" className="cell-agent" title={t("table.agentModel", { agent: x.agent || dash(), model: x.model || dash() })}>
          {t("table.agentModel", { agent: x.agent || dash(), model: x.model || dash() })}
        </td>,
      )}
      <td data-col="age" data-since={x.updated}>
        {t("table.stateAge", { state: stateName(x.state), age: age(x.updated, now) })}
      </td>
      {cell(
        "context",
        <td data-col="context">
          {pct === null ? (
            dash()
          ) : (
            <span
              className="ctx-bar"
              role="meter"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.min(100, Math.max(0, pct))}
              aria-valuetext={t("table.percent", { pct })}
            >
              <span className="ctx-fill" style={{ width: `${Math.min(100, pct)}%` }} />
              <span className="ctx-text">{t("table.percent", { pct })}</span>
            </span>
          )}
        </td>,
      )}
      {cell("tokens", <td data-col="tokens">{x.tokens ? numbers.format(x.tokens.today) : dash()}</td>)}
      {cell(
        "branch",
        <td data-col="branch" className="cell-branch" title={ws?.branch || undefined}>
          {ws?.branch || dash()}
        </td>,
      )}
      {cell(
        "changes",
        <td data-col="changes">{ws ? t("table.changesValue", { added: ws.added, removed: ws.removed, files: ws.uncommitted }) : dash()}</td>,
      )}
      {cell("unpushed", <td data-col="unpushed">{ws ? numbers.format(ws.unpushed) : dash()}</td>)}
      {cell(
        "last",
        <td data-col="last" className="cell-last" title={x.last || undefined}>
          {x.last || dash()}
        </td>,
      )}
    </tr>
  );
}

// AllThreads is the "All threads" tab: one row per thread shown, grouped
// by scope. Every cell is a value from the control API, or a dash where it
// leaves the value out; nothing is guessed. A row opens its thread's tab.
export function AllThreads({ threads, workspaces, now, selected, onOpen, show, open, onShow, scopes }: Props) {
  const every = threads ?? [];
  const list = every.filter((x) => shows(show, x, open));
  // Every thread hidden by the choice: say so, with the way to show them,
  // instead of an empty table.
  if (threads !== null && list.length === 0) {
    const hidden = every.length;
    return (
      <div className="table-empty" data-empty={hidden > 0 ? "inactive" : "none"}>
        <p>{hidden > 0 ? t("tree.noActive", { n: hidden }) : t("tree.none")}</p>
        {hidden > 0 && show === "active" && (
          <button type="button" data-show-all="" onClick={() => onShow("all")}>
            {t("table.showAll")}
          </button>
        )}
      </div>
    );
  }
  const groups = new Map<string, ThreadInfo[]>();
  for (const x of list) {
    const l = groups.get(x.scope) ?? [];
    l.push(x);
    groups.set(x.scope, l);
  }
  // A column with no value for any thread shown is left out.
  const shown = new Set<Column>(columns.map((c) => c.col).filter((c) => list.some((x) => hasValue(c, x, workspaces.get(x.id)))));
  const heads = columns.filter((c) => shown.has(c.col));
  return (
    <table className="all-threads" aria-label={t("table.label")} data-columns={heads.map((c) => c.col).join(" ")}>
      <thead>
        <tr>
          {heads.map((c) => (
            <th key={c.col} scope="col" data-col={c.col} title={t(c.head)}>
              {t(c.head)}
            </th>
          ))}
        </tr>
      </thead>
      {[...groups].map(([scope, l]) => (
        <tbody key={scope} data-scope={scope}>
          <tr className="scope-row">
            <th colSpan={heads.length} scope="rowgroup">
              {scopeLabel(scope, scopes)}
            </th>
          </tr>
          {l.map((x) => (
            <Row key={x.id} x={x} w={workspaces.get(x.id)} now={now} selected={selected === x.id} onOpen={onOpen} shown={shown} />
          ))}
        </tbody>
      ))}
    </table>
  );
}
