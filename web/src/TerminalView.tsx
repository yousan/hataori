import "@xterm/xterm/css/xterm.css";
import { Terminal } from "@xterm/xterm";
import { useEffect, useRef, useState } from "react";
import { type TerminalChunk, type ThreadInfo, terminalPath } from "./api";
import { t } from "./catalog";

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

// TerminalView draws a session's live terminal, read-only.
//
// Read-only means nothing goes back: the terminal is made with stdin
// disabled, so xterm.js turns neither keys, a paste nor its answers to
// terminal queries into data, and nothing here listens for data it would
// send (onData, onBinary). The only request this view makes is the stream
// it reads, a GET; Server-Sent Events have no way back.
export function TerminalView({ x }: { x: ThreadInfo }) {
  const host = useRef<HTMLDivElement>(null);
  const [noProcess, setNoProcess] = useState(false);
  const { scope, key } = x.id;

  useEffect(() => {
    if (!host.current) return;
    const term = openReadOnly(host.current);
    const source = new EventSource(terminalPath({ scope, key }));
    // xterm.js parses what it is given later, while reset and resize act at
    // once: each chunk waits for the bytes before it to be drawn, so a reset
    // never lands under output that came before it.
    let drawn = Promise.resolve();
    let gone = false;
    const draw = (c: TerminalChunk) =>
      new Promise<void>((done) => {
        if (gone) return done();
        if (c.reset) term.reset(); // the end of the log, not a continuation: draw it afresh
        if (c.cols > 0 && c.rows > 0 && (c.cols !== term.cols || c.rows !== term.rows)) term.resize(c.cols, c.rows);
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
      source.close();
      term.dispose();
    };
  }, [scope, key]);

  // An asleep session has no process. A stream that was open when its host
  // ended sends nothing more until the next run, so the thread's state says
  // so; a stream opened later starts with the last run's log and no run.
  return (
    <div className="terminal-view" data-terminal={x.number}>
      {(noProcess || x.state === "asleep") && (
        <p className="no-process" data-no-process="">
          {t("pane.noProcess")}
        </p>
      )}
      <div className="xterm-host" ref={host} />
    </div>
  );
}
