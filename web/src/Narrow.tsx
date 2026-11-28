import { unavailable, visibleState } from "./machine";
import { useEffect, useState } from "react";
import type { ChatLayout, Event, HostTerminal, ScopeInfo, ThreadInfo } from "./api";
import { hostLabel } from "./hostTerminals";
import { HostDetails, HostPane } from "./HostViews";
import { t } from "./catalog";
import { displayTitle, fullTitle } from "./format";
import { Details, Waiting } from "./Details";
import type { Attention } from "./live";
import { type OpenInChat, type Show, ShowChoice, shows } from "./show";
import { ThreadPane, type View } from "./ThreadPane";
import { HostSections, Row } from "./Tree";
import type { Workspace } from "./workspaces";

// narrowQuery is when the page becomes a list: a phone held upright, or a
// window as narrow.
export const narrowQuery = "(max-width: 720px)";

// useNarrow reports whether the page is narrow now, following the window.
export function useNarrow(): boolean {
  const [narrow, setNarrow] = useState(() => matchMedia(narrowQuery).matches);
  useEffect(() => {
    const q = matchMedia(narrowQuery);
    const changed = () => setNarrow(q.matches);
    q.addEventListener("change", changed);
    return () => q.removeEventListener("change", changed);
  }, []);
  return narrow;
}

interface ListProps {
  threads: ThreadInfo[] | null;
  attention: ReadonlyMap<number, Attention>;
  now: number;
  onOpen: (n: number) => void;
  hosts: HostTerminal[] | null; // null when the server does not list them
  onOpenHost: (device: string) => void;
  show: Show; // the active threads or all of them
  open: OpenInChat | null; // what the chats' layout says is open in the chat
  onShow: (s: Show) => void;
  hidden: number; // how many threads the choice hides
  hiddenHosts: number; // how many host terminals it hides, counted apart
}

// NarrowList is the page on a narrow screen: the threads that need
// attention (waiting or failed, by their state, also before what they wait
// on is read), then those running, then the rest folded. The rows are the
// tree's. The choice between the active threads and all of them is the
// wide page's, kept in the same place.
export function NarrowList({ threads, attention, now, onOpen, hosts, onOpenHost, show, open, onShow, hidden, hiddenHosts }: ListProps) {
  const every = threads ?? [];
  const all = every.filter((x) => shows(show, x, open));
  const needs = (x: ThreadInfo) => visibleState(x) === "waiting" || visibleState(x) === "failed";
  const waiting = all.filter(needs);
  const running = all.filter((x) => visibleState(x) === "running");
  const away = all.filter(unavailable);
  const pending = all.filter((x) => !unavailable(x) && x.placement_pending);
  const rest = all.filter((x) => !needs(x) && visibleState(x) !== "running" && !unavailable(x) && !x.placement_pending);
  const rows = (list: ThreadInfo[], why: boolean, label: string) => (
    <ul role="listbox" aria-label={label}>
      {list.map((x) => (
        <Row key={x.id} x={x} attention={attention.get(x.id)} why={why} now={now} selected={false} onSelect={onOpen} />
      ))}
    </ul>
  );
  return (
    <nav className="narrow-list tree density-dense" aria-label={t("narrow.label")}>
      <div className="narrow-show">
        <ShowChoice show={show} onShow={onShow} hidden={hidden} terminals={hiddenHosts} />
      </div>
      {threads !== null && every.length === 0 && <p className="none">{t("narrow.none")}</p>}
      {every.length > 0 && all.length === 0 && <p className="none">{t("tree.noActive", { n: hidden })}</p>}
      {waiting.length > 0 && (
        <section data-section="attention">
          <h2>{t("narrow.attention")}</h2>
          {rows(waiting, true, t("narrow.attention"))}
        </section>
      )}
      {running.length > 0 && (
        <section data-section="running">
          <h2>{t("narrow.running")}</h2>
          {rows(running, false, t("narrow.running"))}
        </section>
      )}
      {away.length > 0 && <section data-section="unavailable"><h2>{t("machine.unavailable")}</h2>{rows(away, false, t("machine.unavailable"))}</section>}
      {pending.length > 0 && <section data-section="pending"><h2>{t("machine.pending")}</h2>{rows(pending, false, t("machine.pending"))}</section>}
      {rest.length > 0 && (
        <details data-section="rest">
          <summary>{t("narrow.rest", { n: rest.length })}</summary>
          {rows(rest, false, t("narrow.rest", { n: rest.length }))}
        </details>
      )}
      {hosts && <HostSections hosts={hosts} filter="" now={now} selected={null} onOpen={onOpenHost} fold />}
    </nav>
  );
}

// NarrowHost is a terminal hataori does not own, opened from the list:
// its screen or what the host says of it, its details, and a way back.
export function NarrowHost({ h, device, now, onBack }: { h: HostTerminal | undefined; device: string; now: number; onBack: () => void }) {
  return (
    <div className="narrow-thread" data-narrow-host={h?.device ?? device}>
      <div className="narrow-bar">
        <button type="button" className="narrow-back" onClick={onBack}>
          {t("narrow.back")}
        </button>
        <h2 className="narrow-title">{h ? hostLabel(h) : device}</h2>
      </div>
      <div className="narrow-pane">
        <HostPane h={h} now={now} />
      </div>
      {h && h.tmux && <HostDetails h={h} now={now} />}
    </div>
  );
}

interface ThreadProps {
  x: ThreadInfo;
  attention: Attention | undefined;
  workspace: Workspace | undefined;
  subscribe: (f: (e: Event) => void) => () => void;
  opens: number;
  scopes: ScopeInfo[] | null;
  layout: ChatLayout[] | null;
  onBack: () => void;
  view: View | undefined; // the view this thread's tab shows
  onView: (v: View) => void;
  mayAct: boolean; // the server lets this person give the thread input
  now: number;
  onOpen: (n: number) => void; // opens another thread
}

// NarrowThread is one thread opened from the list: what it waits on, its
// terminal or its rebuilt output, then the details panel, and a way back.
export function NarrowThread({ x, attention, workspace, subscribe, opens, scopes, layout, onBack, view, onView, mayAct, now, onOpen }: ThreadProps) {
  return (
    <div className="narrow-thread" data-narrow-thread={x.id}>
      <div className="narrow-bar">
        <button type="button" className="narrow-back" onClick={onBack}>
          {t("narrow.back")}
        </button>
        <h2 className="narrow-title" title={fullTitle(x) || undefined}>
          {t("narrow.thread", { n: x.id, title: displayTitle(x) })}
        </h2>
      </div>
      {attention && <Waiting a={attention} />}
      <div className="narrow-pane">
        <ThreadPane x={x} now={now} subscribe={subscribe} opens={opens} view={view} onView={onView} mayAct={mayAct} narrow onOpen={onOpen} />
      </div>
      <Details now={now} x={x} attention={attention} workspace={workspace} subscribe={subscribe} opens={opens} scopes={scopes} layout={layout} withWaiting={false} />
    </div>
  );
}
