import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { type HostTerminal, type ScopeInfo, type State, states, type ThreadInfo } from "./api";
import { hostID, hostMatches } from "./hostTerminals";
import { t } from "./catalog";
import { age, contextUse, displayTitle, fullTitle, mark, matches, scopeLabel, stateName } from "./format";
import type { Attention } from "./live";
import { type Show, shows } from "./show";
import { hostNode, scopeNode, useFolds } from "./folds";

export type Density = "dense" | "normal";

interface Props {
  threads: ThreadInfo[] | null;
  scopes: ScopeInfo[] | null; // the scopes the caller may see, with their names; null until read
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
  // The host's terminals hataori does not own that the choice shows; null
  // when the server does not list them, and then neither section is shown.
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
      // The first line: the details panel shows the rest.
      return a.reason ? t("thread.failure", { reason: a.reason.split("\n")[0] }) : t("thread.failureUnknown");
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
      tabIndex={parent ? -1 : 0}
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
        <span className="title" title={fullTitle(x) || undefined}>
          {displayTitle(x)}
        </span>
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

// newestFirst orders threads by their last activity, the newest first.
function newestFirst(a: ThreadInfo, b: ThreadInfo): number {
  return Date.parse(b.updated) - Date.parse(a.updated) || b.id - a.id;
}

// Node is a node of the tree that folds: a tree item that holds its head
// and, open, the items under it.
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
    <li
      className="node"
      role="treeitem"
      aria-level={1}
      aria-expanded={open}
      aria-label={label}
      tabIndex={-1}
      data-tree-item={node}
      data-node={node}
      data-open={open ? "yes" : "no"}
      {...attrs}
      onKeyDown={(e) => {
        if (e.target === e.currentTarget && (e.key === "Enter" || e.key === " ")) {
          e.preventDefault();
          onFold(node, open);
        }
      }}
    >
      <div className="node-head" title={t(open ? "tree.fold" : "tree.unfold", { name: label })} onClick={() => onFold(node, open)}>
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
        <ul role="group" className="node-body">
          {children}
        </ul>
      )}
    </li>
  );
}

// treeKeys moves along the tree's items as a tree view does: up and down
// to the item before or after, left folds a node or goes to the item it is
// under, right unfolds a node or goes to its first item. Enter opens, as
// each item does by itself.
function treeKeys(e: React.KeyboardEvent<HTMLElement>, fold: (node: string, folded: boolean) => void) {
  const items = [...e.currentTarget.querySelectorAll<HTMLElement>("[data-tree-item]")];
  const i = items.indexOf(document.activeElement as HTMLElement);
  if (i < 0) return;
  const el = items[i];
  const node = el.dataset.node; // a node that folds
  const expanded = el.getAttribute("aria-expanded");
  switch (e.key) {
    case "ArrowDown":
      items[i + 1]?.focus();
      break;
    case "ArrowUp":
      items[i - 1]?.focus();
      break;
    case "ArrowLeft":
      if (node && expanded === "true") fold(node, true);
      else if (el.dataset.parent) items.find((x) => x.dataset.treeItem === el.dataset.parent)?.focus();
      break;
    case "ArrowRight":
      if (node && expanded === "false") fold(node, false);
      else if (expanded === "true" && items[i + 1]?.dataset.parent === el.dataset.treeItem) items[i + 1].focus();
      break;
    default:
      return;
  }
  e.preventDefault();
}

export function Tree(props: Props) {
  const { threads, scopes, attention, now, filter, onFilter, density, onDensity, selected, onSelect, show, hidden, hosts, selectedHost, onOpenHost } =
    props;
  const [folded, setFolded] = useFolds();
  // The tree is one Tab stop: the item last focused in it, else its first;
  // the arrow keys move in it.
  const tree = useRef<HTMLDivElement>(null);
  const current = useRef<string | null>(null);
  const tabStop = () => {
    const items = [...(tree.current?.querySelectorAll<HTMLElement>("[data-tree-item]") ?? [])];
    const stop = items.find((i) => i.dataset.treeItem === current.current) ?? items[0];
    for (const i of items) i.tabIndex = i === stop ? 0 : -1;
  };
  useLayoutEffect(tabStop);
  // While the filter is on, a node opens by itself when the filter finds
  // something in it, and a fold lasts until the filter is cleared.
  const filtering = filter.trim() !== "";
  const [filterFolds, setFilterFolds] = useState<Record<string, boolean>>({});
  useEffect(() => {
    if (!filtering) setFilterFolds({});
  }, [filtering]);
  const all = threads ?? [];
  const visible = all.filter((x) => shows(show, x));
  const shown = visible.filter((x) => matches(x, filter));
  const waiting = shown.filter((x) => attention.has(x.id));

  // One node per scope the caller may see, also one with no thread; while
  // filtering, only those with a thread the filter finds.
  const lists = new Map<string, ThreadInfo[]>();
  for (const s of scopes ?? []) lists.set(s.key, []);
  for (const x of all) if (!lists.has(x.scope)) lists.set(x.scope, []);
  for (const x of shown) lists.get(x.scope)!.push(x);
  const label = (key: string) => scopeLabel(key, scopes);
  const nodes = [...lists]
    .filter(([, list]) => !filtering || list.length > 0)
    .map(([scope, list]) => ({ scope, label: label(scope), list: [...list].sort(newestFirst) }))
    .sort((a, b) => a.label.localeCompare(b.label) || a.scope.localeCompare(b.scope));
  const counts = new Map<State, number>();
  for (const x of all) counts.set(x.state, (counts.get(x.state) ?? 0) + 1);
  const hostsShown = hosts?.filter((h) => hostMatches(h, filter)) ?? [];
  const open = (node: string, found: boolean) => (filtering ? found && !(filterFolds[node] ?? false) : !folded(node));
  const fold = (node: string, f: boolean) => {
    if (filtering) setFilterFolds((have) => ({ ...have, [node]: f }));
    else setFolded(node, f);
  };

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
        <div
          className="threads"
          role="tree"
          aria-label={t("tree.label")}
          ref={tree}
          onKeyDown={(e) => treeKeys(e, fold)}
          onFocus={(e) => {
            const item = (e.target as HTMLElement).closest<HTMLElement>("[data-tree-item]");
            if (item) {
              current.current = item.dataset.treeItem ?? null;
              tabStop();
            }
          }}
        >
          <div data-section="threads" role="none">
            {all.length === 0 && threads !== null && <p className="none">{t("tree.none")}</p>}
            {all.length > 0 && visible.length === 0 && <p className="none">{t("tree.noActive", { n: hidden })}</p>}
            {visible.length > 0 && shown.length === 0 && (
              <p className="none">{hidden > 0 ? t("tree.noMatchHidden", { n: hidden }) : t("tree.noMatch")}</p>
            )}
            <ul role="none">
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
                  {list.map(row(false, scopeNode(scope)))}
                </Node>
              ))}
            </ul>
          </div>
          {hosts && (
            <ul role="none">
              <Node
                node={hostNode}
                label={t("tree.host")}
                count={hostsShown.length}
                open={open(hostNode, hostsShown.length > 0)}
                onFold={fold}
              >
                <HostTree hosts={hostsShown} now={now} selected={selectedHost} onOpen={onOpenHost} open={(n) => open(n, true)} onFold={fold} />
              </Node>
            </ul>
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

// HostTree is what the host terminals' node holds: each tmux session with
// its panes, then the other terminals, as tree items. They show no state:
// hataori judges none.
function HostTree(props: {
  hosts: HostTerminal[];
  now: number;
  selected: string | null;
  onOpen: (device: string) => void;
  open: (node: string) => boolean;
  onFold: (node: string, folded: boolean) => void;
}) {
  const { hosts, now, selected, onOpen, open, onFold } = props;
  const sessions = new Map<string, HostTerminal[]>();
  for (const h of hosts) {
    if (!h.tmux) continue;
    const list = sessions.get(h.tmux.session) ?? [];
    list.push(h);
    sessions.set(h.tmux.session, list);
  }
  const others = hosts.filter((h) => !h.tmux);
  const group = (item: string, label: string, list: HostTerminal[], attrs: Record<string, string>) => {
    const isOpen = open(item);
    return (
      <li
        key={item}
        className="subnode"
        role="treeitem"
        aria-level={2}
        aria-expanded={isOpen}
        aria-label={label}
        tabIndex={-1}
        data-tree-item={item}
        data-node={item}
        data-open={isOpen ? "yes" : "no"}
        data-parent={hostNode}
        {...attrs}
        onKeyDown={(e) => {
          if (e.target === e.currentTarget && (e.key === "Enter" || e.key === " ")) {
            e.preventDefault();
            onFold(item, isOpen);
          }
        }}
      >
        <div className="subnode-head" onClick={() => onFold(item, isOpen)}>
          <span className="fold-mark" aria-hidden="true">
            {t(isOpen ? "tree.openMark" : "tree.foldedMark")}
          </span>
          {label}
        </div>
        {isOpen && (
          <ul role="group">
            {list.map((h) => (
              <HostRow key={hostID(h)} h={h} now={now} selected={selected === hostID(h)} onOpen={onOpen} parent={item} level={3} />
            ))}
          </ul>
        )}
      </li>
    );
  };
  return (
    <>
      {[...sessions].map(([session, list]) => group(`host-session:${session}`, t("tree.tmuxSession", { session }), list, { "data-session": session }))}
      {others.length > 0 && group("host-others", t("host.others"), others, { "data-section": "others" })}
    </>
  );
}

function HostRow(props: {
  h: HostTerminal;
  now: number;
  selected: boolean;
  onOpen: (device: string) => void;
  parent?: string; // in the tree: the item it is under
  level?: number;
}) {
  const { h, now, selected, onOpen, parent, level } = props;
  const p = h.tmux;
  const r = p ? h.reported : undefined;
  return (
    <li
      className={selected ? "row host-row selected" : "row host-row"}
      data-host={h.device}
      data-host-id={hostID(h)}
      role={parent ? "treeitem" : "option"}
      aria-level={parent ? level : undefined}
      data-tree-item={parent ? `host:${hostID(h)}` : undefined}
      data-parent={parent}
      tabIndex={parent ? -1 : 0}
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
      {r && (
        // What the session's own hooks reported, as they said it: no state.
        <div className="line2 reported" data-reported={r.session}>
          <span className="reported-event">{r.event}</span>
          <time className="age" dateTime={r.at} title={t("host.reportedAt")}>
            {age(r.at, now)}
          </time>
          <span className="reported-session" title={t("host.reportedSessionTitle", { session: r.session })}>
            {r.session}
          </span>
          <span className="reported-dir" title={t("host.reportedDirTitle", { dir: r.dir })}>
            {r.dir}
          </span>
        </div>
      )}
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
}) {
  const { hosts, filter, now, selected, onOpen, fold } = props;
  const shown = hosts.filter((h) => hostMatches(h, filter));
  const panes = shown.filter((h) => h.tmux);
  const others = shown.filter((h) => !h.tmux);
  const sessions = new Map<string, HostTerminal[]>();
  for (const h of panes) {
    const list = sessions.get(h.tmux!.session) ?? [];
    list.push(h);
    sessions.set(h.tmux!.session, list);
  }
  const row = (h: HostTerminal) => <HostRow key={hostID(h)} h={h} now={now} selected={selected === hostID(h)} onOpen={onOpen} />;
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
        <h2>{t("host.tmux")}</h2>
        {tmux}
      </section>
      <section data-section="others">
        <h2>{t("host.others")}</h2>
        {other}
      </section>
    </>
  );
}
