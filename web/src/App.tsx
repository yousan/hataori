import { useEffect, useMemo, useRef, useState } from "react";
import { t } from "./catalog";
import { useLayout } from "./layout";
import { linkRefused } from "./session";
import { SignInNotice } from "./Passkeys";
import { useDashboard, useNow } from "./live";
import { StatusBar } from "./StatusBar";
import { TabBar, tabID } from "./TabBar";
import { type AttachSwitch, defaultView, ThreadPane, type View } from "./ThreadPane";
import type { Typing } from "./TerminalView";
import { type AttachState, Attachment, useMayAct, web } from "./attach";
import { AllThreads } from "./AllThreads";
import { Details } from "./Details";
import { NarrowHost, NarrowList, NarrowThread, useNarrow } from "./Narrow";
import { useWorkspaces } from "./workspaces";
import { hostID, hostLabel, useHostTerminals } from "./hostTerminals";
import { HostDetails, HostPane } from "./HostViews";
import { active, allTab, close, hostOf, hostTab, initialTabs, panes, type Split, type TabKey, type Tabs, threadOf, threadTab, view } from "./tabs";
import { useTheme } from "./theme";
import type { HostKey } from "./keys";
import { Operation, type OpState, useMayOperate } from "./operate";
import { displayTitle } from "./format";
import { hiddenCount, showsHost, useShow } from "./show";
import { openInChat } from "./chatTree";
import { type Density, Tree } from "./Tree";

const densityKey = "hataori.density";

function savedDensity(): Density {
  try {
    return localStorage.getItem(densityKey) === "normal" ? "normal" : "dense";
  } catch {
    return "dense";
  }
}

// App is the page: the tree on the left, the tab bar over the panes, the
// details panel on the right, the status bar at the bottom. What a pane and
// the details panel show comes with their own issues.
export function App() {
  const [theme, setTheme] = useTheme();
  const d = useDashboard();
  const now = useNow(1000);
  const [filter, setFilter] = useState("");
  const [density, setDensity] = useState<Density>(savedDensity);
  const [tabs, setTabs] = useState<Tabs>(initialTabs);
  // views is what each open thread's tab shows: the view it opened on, then
  // the person's choice, kept while the tab is open.
  const [views, setViews] = useState<ReadonlyMap<TabKey, View>>(new Map());
  const [split, setSplit] = useState<Split>(1);
  const [layout, setLayout] = useLayout();
  const [showing, setShowing] = useShow();
  // What the chats' layout says is open in the chat: with it, the active
  // threads of a scope it covers are those the chat shows (show.tsx).
  const open = useMemo(() => openInChat(d.layout, d.threads ?? []), [d.layout, d.threads]);
  const hiddenThreads = hiddenCount(showing, d.threads, open);

  const pickDensity = (x: Density) => {
    setDensity(x);
    try {
      localStorage.setItem(densityKey, x);
    } catch {
      // Not kept; the page still switches.
    }
  };
  const thread = (n: number) => d.threads?.find((x) => x.id === n);
  // A thread's tab opens on the view its state gives at that moment; a
  // closed tab forgets its view.
  useEffect(() => {
    setViews((have) => {
      const next = new Map(have);
      for (const k of tabs.open) {
        const n = threadOf(k);
        const x = n === null ? undefined : thread(n);
        if (x && !next.has(k)) next.set(k, defaultView(x));
      }
      for (const k of have.keys()) if (!tabs.open.includes(k)) next.delete(k);
      return next.size === have.size && [...next].every(([k, v]) => have.get(k) === v) ? have : next;
    });
  }, [tabs.open, d.threads]);
  const viewOf = (k: TabKey) => views.get(k);
  const chooseView = (k: TabKey) => (v: View) => setViews((have) => new Map(have).set(k, v));
  const current = threadOf(active(tabs));
  const [hosts, hostSkew] = useHostTerminals();
  // The rows of host terminals the tree and the narrow list show: idle
  // shell panes are hidden with asleep threads, and counted apart. A
  // pane's tab and details still find it in hosts.
  const shownHosts = hosts === null ? null : hosts.filter((h) => showsHost(showing, h, now + hostSkew));
  // Counted apart: the threads the choice hides, and the idle shell panes.
  const hiddenHosts = (hosts?.length ?? 0) - (shownHosts?.length ?? 0);
  const host = (id: string) => hosts?.find((h) => hostID(h) === id);
  const currentHost = hostOf(active(tabs));
  const openHost = (device: string) => setTabs((s) => view(s, hostTab(device)));
  const workspaces = useWorkspaces(d.threads, panes(tabs, split).includes(allTab) || layout.details, current);

  // Operating host tmux panes: the server says whether this person may;
  // each host pane's tab has its own operation, off when the tab opens and
  // ended when it closes.
  const may = useMayOperate();
  const [ops, setOps] = useState<ReadonlyMap<TabKey, OpState>>(new Map());
  const operations = useRef(new Map<TabKey, Operation>());
  const opOf = (k: TabKey) => ops.get(k);
  // The switch toggles the tab's operation as it is now, not as it was
  // last drawn: a second click while it starts switches it off. One that
  // ended on the page while the server sends what was typed before it is
  // left to close itself.
  const switchOp = (k: TabKey, pane: string) => () => {
    const now = operations.current.get(k);
    if (now?.live) {
      now.stop();
      return;
    }
    const op: Operation = new Operation(pane, (s) => {
      if (operations.current.get(k) === op) setOps((m) => new Map(m).set(k, s));
    });
    operations.current.set(k, op);
    op.begin();
  };
  const keysTo = (k: TabKey) => (keys: HostKey[]) => operations.current.get(k)?.send(keys);
  const closeTab = (k: TabKey) => {
    operations.current.get(k)?.close();
    operations.current.delete(k);
    attachments.current.get(k)?.close();
    attachments.current.delete(k);
    setAtts((m) => {
      if (!m.has(k)) return m;
      const next = new Map(m);
      next.delete(k);
      return next;
    });
    setOps((m) => {
      if (!m.has(k)) return m;
      const next = new Map(m);
      next.delete(k);
      return next;
    });
    setTabs((s) => close(s, k));
  };
  // Leaving the page closes every operation's WebSocket with it; so does
  // unmounting.
  useEffect(() => () => operations.current.forEach((o) => o.close()), []);
  const operating = (ops.get(active(tabs))?.phase === "on" ? ops.get(active(tabs)) : undefined) ?? [...ops.values()].find((s) => s.phase === "on");

  // Typing into a session's terminal (attach.ts): the server says whether
  // this person may act; each thread's tab has its own attachment. On the
  // loopback listener the first key opens it and leaving the terminal ends
  // it; on the web listener the tab's switch does both.
  const mayAct = useMayAct();
  const [atts, setAtts] = useState<ReadonlyMap<TabKey, AttachState>>(new Map());
  const attachments = useRef(new Map<TabKey, Attachment>());
  const attOf = (k: TabKey) => atts.get(k);
  const attach = (k: TabKey, n: number): Attachment => {
    const a: Attachment = new Attachment(n, (s) => {
      if (attachments.current.get(k) === a) setAtts((m) => new Map(m).set(k, s));
    });
    attachments.current.set(k, a);
    a.begin();
    return a;
  };
  const typingFor = (k: TabKey, n: number): Typing => {
    const phase = atts.get(k)?.phase;
    return {
      allowed: mayAct && (!web() || phase === "on" || phase === "starting"),
      attached: mayAct && phase === "on",
      onResize: (cols, rows) => {
        const a = attachments.current.get(k);
        if (a?.live) a.resize(cols, rows);
      },
      onKeys: (keys) => {
        let a = attachments.current.get(k);
        if (!a?.live) {
          if (web()) return; // the switch is off
          a = attach(k, n);
        }
        a.send(keys);
      },
      onLeave: () => {
        if (!web()) attachments.current.get(k)?.stop();
      },
    };
  };
  const switchFor = (k: TabKey, n: number): AttachSwitch | undefined =>
    mayAct && web()
      ? {
          state: atts.get(k),
          onSwitch: () => {
            const now = attachments.current.get(k);
            if (now?.live) now.stop();
            else attach(k, n);
          },
        }
      : undefined;
  useEffect(() => () => attachments.current.forEach((a) => a.close()), []);
  const attached = [...atts.values()].find((s) => s.phase === "on")?.thread;

  const paneThread = (k: TabKey) => {
    const n = threadOf(k);
    return n === null ? undefined : thread(n);
  };
  const paneName = (k: TabKey) => {
    const n = threadOf(k);
    const device = hostOf(k);
    if (device !== null) {
      const h = host(device);
      return h ? hostLabel(h) : device;
    }
    if (n === null) return t("tabs.all");
    const x = thread(n);
    return x ? t("pane.thread", { n, title: displayTitle(x) }) : t("thread.number", { n });
  };

  // On a narrow screen the same page is a list; a thread opened from it is
  // one entry of the browser's history, so Back returns to the list.
  // The thread a history entry names: a reload keeps the thread open. The
  // opened thread is the active tab's, so both layouts show the same one.
  const narrow = useNarrow();
  const entry = (): number | null => {
    const n = (history.state as { hataoriThread?: unknown } | null)?.hataoriThread;
    return typeof n === "number" ? n : null;
  };
  const entryHost = (): string | null => {
    const h = (history.state as { hataoriHost?: unknown } | null)?.hataoriHost;
    return typeof h === "string" ? h : null;
  };
  const [opened, setOpened] = useState<number | null>(entry);
  const [openedHost, setOpenedHost] = useState<string | null>(entryHost);
  const show = (n: number | null) => {
    setOpened(n);
    if (n !== null) setTabs((s) => view(s, threadTab(n)));
  };
  useEffect(() => {
    if (opened !== null) setTabs((s) => view(s, threadTab(opened)));
    const popped = () => {
      show(entry());
      setOpenedHost(entryHost());
    };
    addEventListener("popstate", popped);
    return () => removeEventListener("popstate", popped);
  }, []);
  const openNarrow = (n: number) => {
    show(n);
    history.pushState({ hataoriThread: n }, "");
  };
  const openNarrowHost = (device: string) => {
    openHost(device);
    setOpened(null); // a host terminal is no thread: no thread's state under it
    setOpenedHost(device);
    history.pushState({ hataoriHost: device }, "");
  };
  const backNarrow = () => {
    if (entry() !== null || entryHost() !== null) history.back();
    else {
      setOpened(null);
      setOpenedHost(null);
    }
  };
  // A thread chosen in the wide layout is the one the narrow layout shows.
  useEffect(() => {
    if (narrow && opened !== null && current !== null && current !== opened) {
      setOpened(current);
      history.replaceState({ hataoriThread: current }, "");
    }
  }, [narrow]);
  // A thread that has gone from the list closes, and so does its entry.
  useEffect(() => {
    if (opened !== null && d.threads !== null && !d.threads.some((x) => x.id === opened)) {
      setOpened(null);
      history.replaceState(null, "");
    }
  }, [opened, d.threads]);

  if (narrow) {
    const x = opened === null ? undefined : thread(opened);
    const loading = opened !== null && d.threads === null;
    return (
      <div className="shell narrow" data-layout="narrow">
        <SignInNotice signedOut={d.signedOut} refused={linkRefused()} />
        <div className="narrow-body">
          {openedHost !== null ? (
            <NarrowHost h={host(openedHost)} device={openedHost} now={now} onBack={backNarrow} />
          ) : loading ? null : x ? (
            <NarrowThread
              x={x}
              attention={d.attention.get(x.id)}
              workspace={workspaces.get(x.id)}
              subscribe={d.subscribe}
              opens={d.opens}
              onBack={backNarrow}
              view={viewOf(threadTab(x.id))}
              onView={chooseView(threadTab(x.id))}
              mayAct={mayAct}
            />
          ) : (
            <NarrowList
              threads={d.threads}
              attention={d.attention}
              now={now}
              onOpen={openNarrow}
              hosts={shownHosts}
              onOpenHost={openNarrowHost}
              show={showing}
              open={open}
              onShow={setShowing}
              hidden={hiddenThreads}
              hiddenHosts={hiddenHosts}
            />
          )}
        </div>
        <StatusBar
          selected={openedHost !== null ? undefined : x}
          live={d.live}
          limits={d.limits}
          clients={d.clients}
          now={now}
          signedIn={!d.signedOut}
          operating={operating}
        />
      </div>
    );
  }

  return (
    <div className="shell" data-layout="wide">
      <SignInNotice signedOut={d.signedOut} refused={linkRefused()} />
      <div className="body">
        {layout.tree && (
          <Tree
            threads={d.threads}
            scopes={d.scopes}
            layout={d.layout}
            open={open}
            attention={d.attention}
            now={now}
            filter={filter}
            onFilter={setFilter}
            density={density}
            onDensity={pickDensity}
            selected={current}
            onSelect={(n) => setTabs((s) => view(s, threadTab(n)))}
            show={showing}
            hidden={hiddenThreads}
            hosts={shownHosts}
            selectedHost={currentHost}
            onOpenHost={openHost}
          />
        )}
        <div className="center">
          <TabBar
            open={tabs.open}
            active={active(tabs)}
            thread={thread}
            host={host}
            op={opOf}
            att={attOf}
            now={now}
            onView={(k) => setTabs((s) => view(s, k))}
            onClose={closeTab}
            split={split}
            onSplit={setSplit}
            layout={layout}
            onLayout={setLayout}
            theme={theme}
            onTheme={setTheme}
            show={showing}
            onShow={setShowing}
            hidden={hiddenThreads}
            hiddenHosts={hiddenHosts}
          />
          <main className="panes" data-split={split}>
            {panes(tabs, split).map((k, i) => (
              <section
                key={i}
                className={i === 0 ? "pane active" : "pane"}
                data-pane={i}
                data-tab={k ?? ""}
                role={i === 0 ? "tabpanel" : undefined}
                aria-labelledby={i === 0 && k ? tabID(k) : undefined}
              >
                {k ? (
                  <>
                    <h2 className="pane-name">{paneName(k)}</h2>
                    {k === allTab ? (
                      <AllThreads
                        threads={d.threads}
                        workspaces={workspaces}
                        now={now}
                        selected={current}
                        onOpen={(n) => setTabs((s) => view(s, threadTab(n)))}
                        show={showing}
                        open={open}
                        onShow={setShowing}
                        scopes={d.scopes}
                      />
                    ) : hostOf(k) !== null ? (
                      <HostPane
                        h={host(hostOf(k)!)}
                        now={now}
                        fit
                        may={may}
                        op={opOf(k)}
                        onSwitch={host(hostOf(k)!)?.tmux ? switchOp(k, host(hostOf(k)!)!.tmux!.pane) : undefined}
                        onKeys={keysTo(k)}
                      />
                    ) : paneThread(k) ? (
                      <ThreadPane
                        key={k}
                        x={paneThread(k)!}
                        subscribe={d.subscribe}
                        opens={d.opens}
                        fit
                        view={viewOf(k)}
                        onView={chooseView(k)}
                        typing={typingFor(k, paneThread(k)!.id)}
                        attach={switchFor(k, paneThread(k)!.id)}
                        mayAct={mayAct}
                      />
                    ) : (
                      <p className="empty">{t("pane.later")}</p>
                    )}
                  </>
                ) : (
                  <p className="empty">{t("pane.empty")}</p>
                )}
              </section>
            ))}
          </main>
        </div>
        {layout.details && (
          <aside className="details" aria-label={t("details.label")}>
            {currentHost !== null ? (
              host(currentHost) ? <HostDetails h={host(currentHost)!} now={now} /> : <p className="empty">{t("host.gone")}</p>
            ) : (
            <Details
              x={current === null ? undefined : thread(current)}
              attention={current === null ? undefined : d.attention.get(current)}
              workspace={current === null ? undefined : workspaces.get(current)}
              subscribe={d.subscribe}
              opens={d.opens}
            />
            )}
          </aside>
        )}
      </div>
      <StatusBar
        selected={current === null ? undefined : thread(current)}
        live={d.live}
        limits={d.limits}
        clients={d.clients}
        now={now}
        signedIn={!d.signedOut}
        operating={operating}
        attached={attached}
      />
    </div>
  );
}

