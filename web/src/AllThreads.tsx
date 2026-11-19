import type { ThreadInfo } from "./api";
import { type Key, lang, t } from "./catalog";
import { age, stateName } from "./format";
import { type Show, shows } from "./show";
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
}

const columns: Key[] = [
  "table.state",
  "table.number",
  "table.title",
  "table.agent",
  "table.age",
  "table.context",
  "table.tokens",
  "table.branch",
  "table.changes",
  "table.unpushed",
  "table.last",
];

function Row(props: { x: ThreadInfo; w: Workspace | undefined; now: number; selected: boolean; onOpen: (n: number) => void }) {
  const { x, w, now, selected, onOpen } = props;
  const ws = w && w !== "unreadable" ? w : undefined;
  const pct = x.context && x.context.size > 0 ? Math.round((100 * x.context.used) / x.context.size) : null;
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
      <td data-col="title" className="cell-title">
        {x.title || dash()}
      </td>
      <td data-col="agent">{t("table.agentModel", { agent: x.agent || dash(), model: x.model || dash() })}</td>
      <td data-col="age" data-since={x.updated}>
        {t("table.stateAge", { state: stateName(x.state), age: age(x.updated, now) })}
      </td>
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
      </td>
      <td data-col="tokens">{x.tokens ? numbers.format(x.tokens.today) : dash()}</td>
      <td data-col="branch">{ws?.branch || dash()}</td>
      <td data-col="changes">
        {ws ? t("table.changesValue", { added: ws.added, removed: ws.removed, files: ws.uncommitted }) : dash()}
      </td>
      <td data-col="unpushed">{ws ? numbers.format(ws.unpushed) : dash()}</td>
      <td data-col="last" className="cell-last">
        {x.last || dash()}
      </td>
    </tr>
  );
}

// AllThreads is the "All threads" tab: one row per thread shown, grouped
// by scope. Every cell is a value from the control API, or a dash where it
// leaves the value out; nothing is guessed. A row opens its thread's tab.
export function AllThreads({ threads, workspaces, now, selected, onOpen, show }: Props) {
  const scopes = new Map<string, ThreadInfo[]>();
  for (const x of (threads ?? []).filter((x) => shows(show, x))) {
    const list = scopes.get(x.scope) ?? [];
    list.push(x);
    scopes.set(x.scope, list);
  }
  return (
    <table className="all-threads" aria-label={t("table.label")}>
      <thead>
        <tr>
          {columns.map((c) => (
            <th key={c} scope="col">
              {t(c)}
            </th>
          ))}
        </tr>
      </thead>
      {[...scopes].map(([scope, list]) => (
        <tbody key={scope} data-scope={scope}>
          <tr className="scope-row">
            <th colSpan={columns.length} scope="rowgroup">
              {scope}
            </th>
          </tr>
          {list.map((x) => (
            <Row key={x.id} x={x} w={workspaces.get(x.id)} now={now} selected={selected === x.id} onOpen={onOpen} />
          ))}
        </tbody>
      ))}
    </table>
  );
}
