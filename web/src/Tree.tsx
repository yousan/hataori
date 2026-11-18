import { type HostTerminal, type State, states, type ThreadInfo } from "./api";
import { hostMatches } from "./hostTerminals";
import { t } from "./catalog";
import { age, contextUse, mark, matches, stateName } from "./format";
import type { Attention } from "./live";

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
}) {
  const { x, attention, why, now, selected, onSelect } = props;
  const ctx = contextUse(x);
  return (
    <li
      className={selected ? "row selected" : "row"}
      data-thread={x.number}
      data-state={x.state}
      role="option"
      tabIndex={0}
      aria-selected={selected}
      onClick={() => onSelect(x.number)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect(x.number);
        }
      }}
    >
      <div className="line1">
        <span className={`agent-mark state-${x.state}`} title={stateName(x.state)}>
          {mark(x)}
        </span>
        <span className="num">{t("thread.number", { n: x.number })}</span>
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

export function Tree(props: Props) {
  const { threads, attention, now, filter, onFilter, density, onDensity, selected, onSelect, hosts, selectedHost, onOpenHost } = props;
  const all = threads ?? [];
  const shown = all.filter((x) => matches(x, filter));
  const waiting = shown.filter((x) => attention.has(x.number));

  const scopes = new Map<string, ThreadInfo[]>();
  for (const x of shown) {
    const list = scopes.get(x.id.scope) ?? [];
    list.push(x);
    scopes.set(x.id.scope, list);
  }
  const counts = new Map<State, number>();
  for (const x of all) counts.set(x.state, (counts.get(x.state) ?? 0) + 1);

  const row = (why: boolean) => (x: ThreadInfo) => (
    <Row
      key={x.number}
      x={x}
      attention={attention.get(x.number)}
      why={why}
      now={now}
      selected={selected === x.number}
      onSelect={onSelect}
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
        <section className="threads" data-section="threads">
          <h2>{t("tree.threads")}</h2>
          {all.length === 0 && threads !== null && <p className="none">{t("tree.none")}</p>}
          {all.length > 0 && shown.length === 0 && <p className="none">{t("tree.noMatch")}</p>}
          {[...scopes].map(([scope, list]) => (
            <div className="scope" key={scope} data-scope={scope}>
              <h3>
                <span className="scope-name">{scope}</span>
                <span className="dots">
                  {list.map((x) => (
                    <span key={x.number} className={`dot state-${x.state}`} title={stateName(x.state)} />
                  ))}
                </span>
              </h3>
              <ul role="listbox" aria-label={scope}>
                {list.map(row(false))}
              </ul>
            </div>
          ))}
        </section>
        {hosts && <HostSections hosts={hosts} filter={filter} now={now} selected={selectedHost} onOpen={onOpenHost} />}
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

function HostRow(props: { h: HostTerminal; now: number; selected: boolean; onOpen: (device: string) => void }) {
  const { h, now, selected, onOpen } = props;
  const p = h.tmux;
  return (
    <li
      className={selected ? "row host-row selected" : "row host-row"}
      data-host={h.device}
      role="option"
      tabIndex={0}
      aria-selected={selected}
      onClick={() => onOpen(h.device)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen(h.device);
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
  const row = (h: HostTerminal) => <HostRow key={h.device} h={h} now={now} selected={selected === h.device} onOpen={onOpen} />;
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
