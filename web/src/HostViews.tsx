import { useEffect, useRef } from "react";
import { type HostTerminal, hostScreenPath } from "./api";
import { type Key, t } from "./catalog";
import { age } from "./format";
import { fitted, sized } from "./fit";
import { eventKey, type HostKey, pasted, toKeys } from "./keys";
import { clock, type EndReason, left, type OpState } from "./operate";
import { openReadOnly } from "./TerminalView";
import { Stream } from "./stream";

const dash = () => t("table.none");

// wide are the code points a terminal draws two cells wide: East Asian
// wide and full-width forms, and emoji.
const wide: readonly [number, number][] = [
  [0x1100, 0x115f], [0x2e80, 0x303e], [0x3041, 0x33ff], [0x3400, 0x4dbf], [0x4e00, 0x9fff], [0xa000, 0xa4cf],
  [0xac00, 0xd7a3], [0xf900, 0xfaff], [0xfe30, 0xfe4f], [0xff00, 0xff60], [0xffe0, 0xffe6],
  [0x1f300, 0x1f64f], [0x1f900, 0x1f9ff], [0x20000, 0x2fffd], [0x30000, 0x3fffd],
];

// cells is how many terminal cells a line takes: two for a wide character,
// none for a combining mark, one for any other.
function cells(line: string): number {
  let n = 0;
  for (const ch of line) {
    const c = ch.codePointAt(0)!;
    if (/\p{Mn}|\p{Me}/u.test(ch) || c === 0x200d || (c >= 0xfe00 && c <= 0xfe0f)) continue;
    n += wide.some(([a, b]) => c >= a && c <= b) ? 2 : 1;
  }
  return n;
}

// HostScreen draws a tmux pane's screen, as the server reads it, in the
// same read-only terminal as a session's: nothing goes back to the pane.
// With fit, it fills the view as a session's does.
//
// While the pane is operated (operating), the terminal takes input: what
// it produces goes through the key table (keys.ts) to onKeys, and a paste
// is sent as a paste. The server then sends the cursor with each screen,
// and it is drawn there. Otherwise the terminal's input is off, as every
// other terminal's of the page.
function HostScreen({ h, fit, operating, onKeys }: { h: HostTerminal; fit?: boolean; operating?: boolean; onKeys?: (keys: HostKey[]) => void }) {
  const el = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const view = useRef<HTMLDivElement>(null);
  const termRef = useRef<ReturnType<typeof openReadOnly> | null>(null);
  const live = useRef({ operating: false, onKeys }); // what the terminal's listeners read
  live.current = { operating: !!operating, onKeys };
  const pane = h.tmux?.pane ?? "";
  useEffect(() => {
    if (!el.current || !pane) return;
    const term = openReadOnly(el.current);
    termRef.current = term;
    // A named key is taken from the keyboard while operating, before the
    // terminal sees it: the terminal would drop some of its modifiers.
    term.attachCustomKeyEventHandler((e) => {
      if (!live.current.operating || e.isComposing || e.metaKey) return true;
      const k = eventKey(e);
      if (k === null) return true;
      e.preventDefault(); // Tab would move the focus
      if (e.type === "keydown") live.current.onKeys?.([{ key: k }]);
      return false;
    });
    // The page's own copy of the terminal sends nothing unless the pane is
    // operated; then only the keys of the table leave it.
    const data = term.onData((d) => {
      if (live.current.operating) live.current.onKeys?.(toKeys(d));
    });
    // A paste is one paste, not keys: it is taken before the terminal sees
    // it.
    const host = el.current;
    const onPaste = (e: ClipboardEvent) => {
      if (!live.current.operating) return;
      e.preventDefault();
      e.stopImmediatePropagation();
      live.current.onKeys?.(pasted(e.clipboardData?.getData("text/plain") ?? ""));
    };
    host.addEventListener("paste", onPaste, true);
    term.options.disableStdin = !live.current.operating; // made again while operated: it takes keys at once
    const fitting = fit && box.current ? fitted(term, box.current) : null;
    const source = new Stream(hostScreenPath(pane));
    // Each message is the whole screen. It is drawn after the one before
    // has been, as a session's chunks are, so two never mix.
    let drawn = Promise.resolve();
    let gone = false;
    // With the cursor, while the pane is operated, the terminal's cursor is
    // moved where tmux has it once the screen is drawn.
    const draw = (screen: string, cursor?: { x: number; y: number }) =>
      new Promise<void>((done) => {
        if (gone) return done();
        const lines = screen.split("\n");
        // tmux leaves out the spaces at the end of a line: the cursor may
        // be past the widest line, or below the last.
        const cols = Math.max(20, cursor ? cursor.x + 1 : 0, ...lines.map(cells));
        const rows = Math.max(1, lines.length, cursor ? cursor.y + 1 : 0);
        term.reset();
        if (cols !== term.cols || rows !== term.rows) {
          term.resize(cols, rows);
          fitting?.refit();
        }
        sized(term);
        const at = cursor ? `\x1b[${cursor.y + 1};${cursor.x + 1}H` : "";
        term.write(lines.join("\r\n") + at, () => {
          if (view.current) view.current.dataset.cursor = cursor ? `${term.buffer.active.cursorX},${term.buffer.active.cursorY}` : "";
          done();
        });
      });
    source.onmessage = (m) => {
      let a: { screen?: string; cursor?: { x?: unknown; y?: unknown } };
      try {
        a = JSON.parse(m.data);
      } catch {
        return;
      }
      const c = a.cursor && typeof a.cursor.x === "number" && typeof a.cursor.y === "number" ? { x: a.cursor.x, y: a.cursor.y } : undefined;
      drawn = drawn.then(() => draw(a.screen ?? "", c));
    };
    return () => {
      gone = true;
      termRef.current = null;
      data.dispose();
      host.removeEventListener("paste", onPaste, true);
      fitting?.stop();
      source.close();
      term.dispose();
    };
  }, [pane, fit]);
  // Declared after the effect that makes the terminal, so that it runs
  // after it, also when that terminal is made again.
  useEffect(() => {
    const term = termRef.current;
    if (!term) return;
    term.options.disableStdin = !operating;
    if (operating) term.focus();
  }, [operating, pane, fit]);
  return (
    <div className={fit ? "terminal-view fit" : "terminal-view"} data-host-screen={pane} data-operating={operating ? "on" : "off"} ref={view}>
      <div className="xterm-box" ref={box}>
        <div className="xterm-host" ref={el} />
      </div>
    </div>
  );
}

function Field(props: { name: string; label: string; value: string }) {
  return (
    <div className="field" data-field={props.name}>
      <dt>{props.label}</dt>
      <dd>{props.value}</dd>
    </div>
  );
}

// HostDetails is what the host says of a terminal hataori does not own,
// and that hataori does not judge its state: no state, no question, no
// badge.
export function HostDetails({ h, now }: { h: HostTerminal; now: number }) {
  const p = h.tmux;
  return (
    <div className="details-body" data-host-details={h.device}>
      <p className="host-note" data-field="note">
        {t("host.notJudged")}
      </p>
      <dl>
        {p && <Field name="session" label={t("host.session")} value={p.session} />}
        {p && <Field name="window" label={t("host.windowField")} value={p.window_name ? t("host.window", { index: p.window, name: p.window_name }) : p.window} />}
        {p && <Field name="pane" label={t("host.pane")} value={t("host.paneValue", { pane: p.pane, index: p.index })} />}
        <Field name="device" label={t("host.device")} value={h.device} />
        <Field name="process" label={t("host.process")} value={h.process || dash()} />
        {p && <Field name="dir" label={t("host.dir")} value={p.dir || dash()} />}
        <Field name="activity" label={t("host.activity")} value={h.activity ? age(h.activity, now) : dash()} />
        {p && <Field name="attached" label={t("host.attached")} value={t(p.attached ? "host.yes" : "host.no")} />}
      </dl>
    </div>
  );
}

// endText is what the tab says when its operation ended, or did not start.
const endText: Record<EndReason, Key> = {
  ended: "operate.end.ended",
  closed: "operate.end.closed",
  idle: "operate.end.idle",
  "pane gone": "operate.end.gone",
  "hataori's own": "operate.end.own",
  stopping: "operate.end.stopping",
  refused: "operate.end.refused",
  signIn: "operate.end.signIn",
  passkeyCancelled: "operate.end.passkeyCancelled",
  failed: "operate.end.failed",
  lost: "operate.end.lost",
};

// Operate is a tmux pane's switch, and what the operation's state is: the
// switch is off whenever the tab opens, and again whenever the operation
// ends, with why.
function Operate({ op, now, onSwitch }: { op: OpState | undefined; now: number; onSwitch: () => void }) {
  const on = op !== undefined && op.phase !== "ended";
  return (
    <div className="operate" data-operate={op?.phase ?? "off"}>
      <button type="button" role="switch" aria-checked={on} data-operate-switch="" onClick={onSwitch}>
        {t("operate.switch")}
      </button>
      {op?.phase === "starting" && <span className="muted">{t("operate.starting")}</span>}
      {op?.phase === "on" && (
        <span className="operating" data-operating-left={left(op, now)}>
          {t("operate.on", { pane: op.pane, left: clock(left(op, now)) })}
        </span>
      )}
      {op?.phase === "on" && op.error && (
        <span className="operate-end" data-operate-error="" role="status">
          {t("operate.keyRefused", { reason: op.error })}
        </span>
      )}
      {op?.phase === "ended" && op.reason && (
        <span className="operate-end" data-operate-end={op.reason} role="status">
          {t(endText[op.reason], { reason: op.detail ?? "" })}
        </span>
      )}
    </div>
  );
}

// HostPane is a host terminal's pane: a tmux pane's screen, or, for a
// terminal outside tmux, whose screen cannot be read, what the host says
// of it. With fit, the screen fills the pane. A tmux pane has the operate
// switch when the server lets the person operate (may); a terminal outside
// tmux never gets a key.
export function HostPane(props: {
  h: HostTerminal | undefined;
  now: number;
  fit?: boolean;
  may?: boolean;
  op?: OpState;
  onSwitch?: () => void; // toggles the operation
  onKeys?: (keys: HostKey[]) => void;
}) {
  const { h, now, fit, may, op, onSwitch, onKeys } = props;
  if (!h) return <p className="empty">{t("host.gone")}</p>;
  if (h.tmux) {
    return (
      <div className="host-operable">
        {may && onSwitch && <Operate op={op} now={now} onSwitch={onSwitch} />}
        <HostScreen h={h} fit={fit} operating={op?.phase === "on"} onKeys={onKeys} />
      </div>
    );
  }
  return (
    <div className="host-meta" data-host-meta={h.device}>
      <p className="empty">{t("host.noScreen")}</p>
      <HostDetails h={h} now={now} />
    </div>
  );
}
