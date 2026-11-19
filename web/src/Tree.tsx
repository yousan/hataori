import { type HostTerminal, type State, states, type ThreadInfo } from "./api";
import { hostID, hostMatches } from "./hostTerminals";
import { t } from "./catalog";
import { age, contextUse, mark, matches, stateName } from "./format";
import type { Attention } from "./live";
import { type Show, shows } from "./show";
import { hostNode, scopeNode, useFolds } from "./folds";

export type Density = "dense" | "normal";

interface Props {
  threads: ThreadInfo[] | null;
  attention: ReadonlyMap<number, Attention>;
  now: number;
  filter: string;
  onFilter: (q: string) => void;
  density: Density;
  onDensity: (d: Density) => void;
  selected: number | null;
  onSelect: (n: number) => void;
  // show: the active threads or all of them; hidden is how many the choice
  // hides. The filter works on the threads shown; the counts in the footer
  // count every thread.
  show: Show;
  hidden: number;
  // The host's terminals hataori does not own; null when the server does
  // not list them, and then neither section is shown.
  hosts: HostTerminal[] | null;
  selectedHost: string | null;
  onOpenHost: (device: string) => void;
}

// attentionText is what a thread that needs attention waits on.
export function attentionText(a: Attention): string {
  switch (a.kind) {
    case "question":
      return a.text ? t("thread.question", { text: a.text }) : t("thread.questionHidden");
    case "approval":
      return a.text ? t("thread.approvalText", { text: a.text }) : t("thread.approval");
    case "failure":
      return a.reason ? t("thread.failure", { reason: a.reason }) : t("thread.failureUnknown");
  }
}

// Row is one thread. In needs attention, what it waits on has a line of
// its own, shown in either density; elsewhere it is on the second line.
export function Row(props: {
  x: ThreadInfo;
  attention?: Attention;
  why: boolean;
  now: number;
  selected: boolean;
  onSelect: (n: number) => void;
  parent?: string; // in the tree: the node it is under
}) {
  const { x, attention, why, now, selected, onSelect, parent } = props;
  const ctx = contextUse(x);
  return (
    <li
      className={selected ? "row selected" : "row"}
      data-thread={x.id}
      data-state={x.state}
      role={parent ? "treeitem" : "option"}
      aria-level={parent ? 2 : undefined}
      data-tree-item={parent ? `thread:${x.id}` : undefined}
      data-parent={parent}
      tabIndex={0}
      aria-selected={selected}
      onClick={() => onSelect(x.id)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect(x.id);
        }
      }}
    >
      <div className="line1">
        <span className={`agent-mark state-${x.state}`} title={stateName(x.state)}>
          {mark(x)}
        </span>
        <span className="num">{t("thread.number", { n: x.id })}</span>
        <span className="title">{x.title}</span>
        <span className="age">{age(x.updated, now)}</span>
      </div>
      {why && attention && <div className="why">{attentionText(attention)}</div>}
      <div className="line2">
        {x.model && <span className="model">{x.model}</span>}
        {ctx && <span className="ctx">{ctx}</span>}
        {attention && !why ? (
          <span className="flag">{attentionText(attention)}</span>
        ) : (
          x.last && <span className="last">{x.last}</span>
        )}
      </div>
    </li>
  );
}

// scopeLabel is what a scope's node says: its name, from the config or its
// channel as the chat service shows it, else its key.
function scopeLabel(scope: string, list: readonly ThreadInfo[]): string {
  return list.find((x) => x.scope_name)?.scope_name ?? scope;
}

// newestFirst orders threads by their last activity, the newest first.
function newestFirst(a: ThreadInfo, b: ThreadInfo): number {
  return Date.parse(b.updated) - Date.parse(a.updated) || b.id - a.id;
}

// Node is a node of the tree that folds: its head, then what it holds.
function Node(props: {
  node: string;
  label: string;
  count: number;
  open: boolean;
  onFold: (node: string, folded: boolean) => void;
  dots?: ThreadInfo[];
  children: React.ReactNode;
  attrs?: Record<string, string>;
}) {
  const { node, label, count, open, onFold, dots, children, attrs } = props;
  return (
    <div className="node" data-node={node} data-open={open ? "yes" : "no"} {...attrs}>
      <div
        className="node-head"
        role="treeitem"
        aria-level={1}
        aria-expanded={open}
        tabIndex={0}
        data-tree-item={node}
        title={t(open ? "tree.fold" : "tree.unfold", { name: label })}
        onClick={() => onFold(node, open)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onFold(node, open);
          }
        }}
      >
        <span className="fold-mark" aria-hidden="true">
          {t(open ? "tree.openMark" : "tree.foldedMark")}
        </span>
        <span className="scope-name">{label}</span>
        <span className="node-count" data-count={count}>
          {t("tree.count", { n: count })}
        </span>
        {dots && (
          <span className="dots">
            {dots.map((x) => (
              <span key={x.id} className={`dot state-${x.state}`} title={stateName(x.state)} />
            ))}
          </span>
        )}
      </div>
      {open && (
        <div role="group" className="node-body">
          {children}
        </div>
      )}
    </div>
  );
}

// treeKeys moves along the tree's items as a tree view does: up and down
// to the item before or after, left folds a node or goes to the node an
// item is under, right unfolds a node or goes to its first item. Enter
// opens, as each item does by itself.
function treeKeys(e: React.KeyboardEvent<HTMLElement>, setFolded: (node: string, f: boolean) => void) {
  const items = [...e.currentTarget.querySelectorAll<HTMLElement>("[data-tree-item]")];
  const i = items.indexOf(document.activeElement as HTMLElement);
  if (i < 0) return;
  const el = items[i];
  const node = el.parentElement?.dataset.node && el.classList.contains("node-head") ? el.parentElement.dataset.node : undefined;
  switch (e.key) {
    case "ArrowDown":
      items[i + 1]?.focus();
      break;
    case "ArrowUp":
      items[i - 1]?.focus();
      break;
    case "ArrowLeft":
      if (node && el.getAttribute("aria-expanded") === "true") setFolded(node, true);
      else if (el.dataset.parent) items.find((x) => x.dataset.treeItem === el.dataset.parent)?.focus();
      break;
    case "ArrowRight":
      if (node && el.getAttribute("aria-expanded") === "false") setFolded(node, false);
      else if (node && items[i + 1]?.dataset.parent === node) items[i + 1].focus();
      break;
    default:
      return;
  }
  e.preventDefault();
}

export function Tree(props: Props) {
  const { threads, attention, now, filter, onFilter, density, onDensity, selected, onSelect, show, hidden, hosts, selectedHost, onOpenHost } = props;
  const [folded, setFolded] = useFolds();
  const all = threads ?? [];
  const visible = all.filter((x) => shows(show, x));
  const shown = visible.filter((x) => matches(x, filter));
  const waiting = shown.filter((x) => attention.has(x.id));
  const filtering = filter.trim() !== "";

  const scopes = new Map<string, ThreadInfo[]>();
  for (const x of shown) {
    const list = scopes.get(x.scope) ?? [];
    list.push(x);
    scopes.set(x.scope, list);
  }
  const nodes = [...scopes]
    .map(([scope, list]) => ({ scope, label: scopeLabel(scope, list), list: [...list].sort(newestFirst) }))
    .sort((a, b) => a.label.localeCompare(b.label) || a.scope.localeCompare(b.scope));
  const counts = new Map<State, number>();
  for (const x of all) counts.set(x.state, (counts.get(x.state) ?? 0) + 1);
  const hostsShown = hosts?.filter((h) => hostMatches(h, filter)) ?? [];
  // A node opens by itself while the filter finds something in it.
  const open = (node: string, found: boolean) => (filtering ? found : !folded(node));
  const fold = (node: string, wasOpen: boolean) => setFolded(node, wasOpen);

  const row = (why: boolean, parent?: string) => (x: ThreadInfo) => (
    <Row
      key={x.id}
      x={x}
      attention={attention.get(x.id)}
      why={why}
      now={now}
      selected={selected === x.id}
      onSelect={onSelect}
      parent={parent}
    />
  );

  return (
    <nav className={`tree density-${density}`} aria-label={t("tree.label")}>
      <div className="tree-top">
        <input
          className="filter"
          type="search"
          value={filter}
          placeholder={t("tree.filter")}
          aria-label={t("tree.filter")}
          onChange={(e) => onFilter(e.target.value)}
        />
        <div className="density" role="group" aria-label={t("tree.density")}>
          {(["dense", "normal"] as const).map((d) => (
            <button key={d} type="button" data-density={d} aria-pressed={density === d} onClick={() => onDensity(d)}>
              {t(d === "dense" ? "tree.dense" : "tree.normal")}
            </button>
          ))}
        </div>
      </div>
      <div className="tree-body">
        {waiting.length > 0 && (
          <section className="attention" data-section="attention">
            <h2>{t("tree.attention")}</h2>
            <ul role="listbox" aria-label={t("tree.attention")}>
              {waiting.map(row(true))}
            </ul>
          </section>
        )}
        <div className="threads" role="tree" aria-label={t("tree.label")} onKeyDown={(e) => treeKeys(e, setFolded)}>
          <div data-section="threads">
            {all.length === 0 && threads !== null && <p className="none">{t("tree.none")}</p>}
            {all.length > 0 && visible.length === 0 && <p className="none">{t("tree.noActive", { n: hidden })}</p>}
            {visible.length > 0 && shown.length === 0 && (
              <p className="none">{hidden > 0 ? t("tree.noMatchHidden", { n: hidden }) : t("tree.noMatch")}</p>
            )}
            {nodes.map(({ scope, label, list }) => (
              <Node
                key={scope}
                node={scopeNode(scope)}
                label={label}
                count={list.length}
                open={open(scopeNode(scope), list.length > 0)}
                onFold={fold}
                dots={list}
                attrs={{ "data-scope": scope }}
              >
                <ul>{list.map(row(false, scopeNode(scope)))}</ul>
              </Node>
            ))}
          </div>
          {hosts && (
            <Node
              node={hostNode}
              label={t("tree.host")}
              count={hostsShown.length}
              open={open(hostNode, hostsShown.length > 0)}
              onFold={fold}
              attrs={{ "data-section": "host" }}
            >
              <HostSections hosts={hosts} filter={filter} now={now} selected={selectedHost} onOpen={onOpenHost} parent={hostNode} />
            </Node>
          )}
        </div>
      </div>
      <ul className="counts" aria-label={t("tree.counts")}>
        {states
          .filter((s) => counts.has(s))
          .map((s) => (
            <li key={s} data-count={s} data-n={counts.get(s)}>
              <span className={`dot state-${s}`} />
              {t("state.count", { state: stateName(s), n: counts.get(s) ?? 0 })}
            </li>
          ))}
      </ul>
    </nav>
  );
}

function HostRow(props: { h: HostTerminal; now: number; selected: boolean; onOpen: (device: string) => void; parent?: string }) {
  const { h, now, selected, onOpen, parent } = props;
  const p = h.tmux;
  return (
    <li
      className={selected ? "row host-row selected" : "row host-row"}
      data-host={h.device}
      data-host-id={hostID(h)}
      role={parent ? "treeitem" : "option"}
      aria-level={parent ? 2 : undefined}
      data-tree-item={parent ? `host:${hostID(h)}` : undefined}
      data-parent={parent}
      tabIndex={0}
      aria-selected={selected}
      onClick={() => onOpen(hostID(h))}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen(hostID(h));
        }
      }}
    >
      <div className="line1">
        <span className="title">{p ? (p.window_name ? t("host.window", { index: p.window, name: p.window_name }) : p.window) : h.device}</span>
        <span className="num">{h.process || t("table.none")}</span>
      </div>
      <div className="line2">
        {p && <span className="device">{h.device}</span>}
        {h.activity && <span className="age">{age(h.activity, now)}</span>}
      </div>
    </li>
  );
}

// HostSections are the tree's sections of terminals on the host that
// hataori does not own: tmux panes by session, then other terminals. They
// show no state: hataori judges none.
export function HostSections(props: {
  hosts: HostTerminal[];
  filter: string;
  now: number;
  selected: string | null;
  onOpen: (device: string) => void;
  fold?: boolean; // fold each section, as the narrow list does
  parent?: string; // in the tree: the node the rows are under
}) {
  const { hosts, filter, now, selected, onOpen, fold, parent } = props;
  const shown = hosts.filter((h) => hostMatches(h, filter));
  const panes = shown.filter((h) => h.tmux);
  const others = shown.filter((h) => !h.tmux);
  const sessions = new Map<string, HostTerminal[]>();
  for (const h of panes) {
    const list = sessions.get(h.tmux!.session) ?? [];
    list.push(h);
    sessions.set(h.tmux!.session, list);
  }
  const row = (h: HostTerminal) => <HostRow key={hostID(h)} h={h} now={now} selected={selected === hostID(h)} onOpen={onOpen} parent={parent} />;
  const tmux = (
    <>
      {panes.length === 0 && <p className="none">{t("host.none")}</p>}
      {[...sessions].map(([session, list]) => (
        <div className="scope" key={session} data-session={session}>
          <h3>
            <span className="scope-name">{session}</span>
          </h3>
          <ul role="listbox" aria-label={session}>
            {list.map(row)}
          </ul>
        </div>
      ))}
    </>
  );
  const other = (
    <>
      {others.length === 0 && <p className="none">{t("host.none")}</p>}
      <ul role="listbox" aria-label={t("host.others")}>
        {others.map(row)}
      </ul>
    </>
  );
  if (fold) {
    return (
      <>
        <details data-section="tmux">
          <summary>{t("host.tmux")}</summary>
          {tmux}
        </details>
        <details data-section="others">
          <summary>{t("host.others")}</summary>
          {other}
        </details>
      </>
    );
  }
  return (
    <>
      <section data-section="tmux">
        <h3 className="host-heading">{t("host.tmux")}</h3>
        {tmux}
      </section>
      <section data-section="others">
        <h3 className="host-heading">{t("host.others")}</h3>
        {other}
      </section>
    </>
  );
}
