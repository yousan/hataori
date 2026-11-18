import { useEffect, useRef } from "react";
import { type HostTerminal, hostScreenPath } from "./api";
import { t } from "./catalog";
import { age } from "./format";
import { openReadOnly } from "./TerminalView";

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
function HostScreen({ h }: { h: HostTerminal }) {
  const el = useRef<HTMLDivElement>(null);
  const pane = h.tmux?.pane ?? "";
  useEffect(() => {
    if (!el.current || !pane) return;
    const term = openReadOnly(el.current);
    const source = new EventSource(hostScreenPath(pane));
    // Each message is the whole screen. It is drawn after the one before
    // has been, as a session's chunks are, so two never mix.
    let drawn = Promise.resolve();
    let gone = false;
    const draw = (screen: string) =>
      new Promise<void>((done) => {
        if (gone) return done();
        const lines = screen.split("\n");
        const cols = Math.max(20, ...lines.map(cells));
        term.reset();
        if (cols !== term.cols || lines.length !== term.rows) term.resize(cols, Math.max(1, lines.length));
        term.write(lines.join("\r\n"), done);
      });
    source.onmessage = (m) => {
      let screen: string;
      try {
        screen = (JSON.parse(m.data) as { screen: string }).screen ?? "";
      } catch {
        return;
      }
      drawn = drawn.then(() => draw(screen));
    };
    return () => {
      gone = true;
      source.close();
      term.dispose();
    };
  }, [pane]);
  return (
    <div className="terminal-view" data-host-screen={pane}>
      <div className="xterm-host" ref={el} />
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

// HostPane is a host terminal's pane: a tmux pane's screen, or, for a
// terminal outside tmux, whose screen cannot be read, what the host says
// of it.
export function HostPane({ h, now }: { h: HostTerminal | undefined; now: number }) {
  if (!h) return <p className="empty">{t("host.gone")}</p>;
  if (h.tmux) return <HostScreen h={h} />;
  return (
    <div className="host-meta" data-host-meta={h.device}>
      <p className="empty">{t("host.noScreen")}</p>
      <HostDetails h={h} now={now} />
    </div>
  );
}
