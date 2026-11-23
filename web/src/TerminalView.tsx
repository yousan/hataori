import "@xterm/xterm/css/xterm.css";
import { Terminal } from "@xterm/xterm";
import { useEffect, useRef, useState } from "react";
import { type TerminalChunk, type ThreadInfo, terminalPath } from "./api";
import { muteQueries } from "./attach";
import { personal } from "./reports";
import { t } from "./catalog";
import { fitted, paneCells, sized } from "./fit";
import { Stream } from "./stream";

// bytes decodes a chunk's base64 data.
function bytes(data: string | undefined): Uint8Array {
  if (!data) return new Uint8Array();
  const s = atob(data);
  const b = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) b[i] = s.charCodeAt(i);
  return b;
}

// openReadOnly makes a terminal in el that sends nothing back: stdin is
// disabled, so xterm.js turns neither keys, a paste nor its answers to
// terminal queries into data, and nothing listens for data it would send.
// Every terminal the page draws is made here.
export function openReadOnly(el: HTMLElement): Terminal {
  const term = new Terminal({ disableStdin: true, cursorBlink: false, scrollback: 5000, convertEol: false });
  term.open(el);
  return term;
}

// Typing is how a session's terminal takes the person's keys while its tab
// allows it (attach.ts): onKeys gets what the terminal emulator made of
// each key and paste, and onLeave is called when the terminal loses focus.
// While attached, the pane's size in cells goes to onResize.
export interface Typing {
  allowed: boolean;
  attached: boolean;
  onKeys: (keys: string) => void;
  onLeave: () => void;
  onResize: (cols: number, rows: number) => void;
}

// resizeQuiet is how long the pane's size must stay before it is given to
// the session: a drag of the window or of a split gives a few sizes, not one
// per frame.
const resizeQuiet = 250;

// TerminalView draws a session's live terminal. With fit, the font is
// sized so that the session's grid fills the view's width or its height
// (fit.ts), centered. While only viewing, the session's own columns and
// rows are kept. While the owner types into it (typing.attached), the
// view gives the session its size in whole cells at the page's font size,
// when it changes and once the attachment is on; a size out of bounds is
// not given, and the grid is then drawn scaled as when viewing.
//
// It is read-only unless typing allows keys: the terminal is made with
// stdin disabled, so xterm.js turns neither keys, a paste nor its answers
// to terminal queries into data. The stream it reads is a GET; Server-Sent
// Events have no way back. While typing allows it, stdin is on and what
// the person types goes to typing.onKeys, and nothing else: the terminal
// answers no query (muteQueries), and mouse and focus reports are dropped.
export function TerminalView({ x, fit, typing }: { x: ThreadInfo; fit?: boolean; typing?: Typing }) {
  const host = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const termRef = useRef<ReturnType<typeof openReadOnly> | null>(null);
  const toPane = useRef<(() => void) | null>(null); // gives the session the pane's size, after a quiet spell
  const live = useRef(typing); // what the terminal's listeners read
  live.current = typing;
  const [noProcess, setNoProcess] = useState(false);
  const id = x.id;

  useEffect(() => {
    if (!host.current) return;
    const term = openReadOnly(host.current);
    termRef.current = term;
    const unmute = muteQueries(term);
    const data = term.onData((d) => {
      if (live.current?.allowed && personal(d)) live.current.onKeys(d);
    });
    const leave = () => live.current?.allowed && live.current.onLeave();
    const textarea = term.textarea;
    textarea?.addEventListener("blur", leave);
    term.options.disableStdin = !live.current?.allowed;
    // The pane's size goes to the session only while attached, and only
    // when it differs from the last one given in this attachment.
    let quiet = 0;
    let given = "";
    const sizeToPane = () => {
      clearTimeout(quiet);
      if (!live.current?.attached) {
        given = "";
        return;
      }
      quiet = window.setTimeout(() => {
        const el = box.current;
        if (gone || !el || !live.current?.attached) return;
        const z = paneCells(term, el);
        if (!z) return; // out of bounds: drawn scaled, as when viewing
        const key = `${z.cols}x${z.rows}`;
        if (key === given) return;
        given = key;
        live.current.onResize(z.cols, z.rows);
      }, resizeQuiet);
    };
    toPane.current = fit ? sizeToPane : null;
    const fitting = fit && box.current ? fitted(term, box.current, sizeToPane) : null;
    const source = new Stream(terminalPath(id));
    // xterm.js parses what it is given later, while reset and resize act at
    // once: each chunk waits for the bytes before it to be drawn, so a reset
    // never lands under output that came before it.
    let drawn = Promise.resolve();
    let gone = false;
    const draw = (c: TerminalChunk) =>
      new Promise<void>((done) => {
        if (gone) return done();
        if (c.reset) term.reset(); // the end of the log, not a continuation: draw it afresh
        if (c.cols > 0 && c.rows > 0 && (c.cols !== term.cols || c.rows !== term.rows)) {
          term.resize(c.cols, c.rows); // the page's copy only: nothing goes to the session
          fitting?.refit();
        }
        sized(term);
        setNoProcess(c.run === "");
        const b = bytes(c.data);
        if (b.length > 0) term.write(b, done);
        else done();
      });
    source.onmessage = (m) => {
      let c: TerminalChunk;
      try {
        c = JSON.parse(m.data) as TerminalChunk;
      } catch {
        return;
      }
      drawn = drawn.then(() => draw(c));
    };
    return () => {
      gone = true;
      clearTimeout(quiet);
      toPane.current = null;
      termRef.current = null;
      data.dispose();
      textarea?.removeEventListener("blur", leave);
      // A terminal taken away with the focus in it, as when another tab is
      // shown, gets no blur: it is left all the same.
      leave();
      unmute();
      fitting?.stop();
      source.close();
      term.dispose();
    };
  }, [id, fit]);
  // Declared after the effect that makes the terminal, so that it runs
  // after it, also when that terminal is made again.
  const allowed = !!typing?.allowed;
  useEffect(() => {
    const term = termRef.current;
    if (term) term.options.disableStdin = !allowed;
  }, [allowed, id, fit]);
  // An attachment that turns on takes the pane's size; one that ends
  // forgets the size it gave.
  const attached = !!typing?.attached;
  useEffect(() => {
    toPane.current?.();
  }, [attached, id, fit]);

  // An asleep session has no process. A stream that was open when its host
  // ended sends nothing more until the next run, so the thread's state says
  // so; a stream opened later starts with the last run's log and no run.
  return (
    <div className={fit ? "terminal-view fit" : "terminal-view"} data-terminal={x.id} data-typing={allowed ? "on" : "off"}>
      {(noProcess || x.state === "asleep") && (
        <p className="no-process" data-no-process="">
          {t("pane.noProcess")}
        </p>
      )}
      <div className="xterm-box" ref={box}>
        <div className="xterm-host" ref={host} />
      </div>
    </div>
  );
}
