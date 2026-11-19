// Operating a host tmux pane from its tab (design, section 7.2, "Typing
// into a tmux pane on the host"; internal/dashboard, operate.go).
//
// The page asks once whether its person may operate at all, and shows the
// switch only then. Switching it on asks the server what the start needs:
// on the loopback listener nothing; on the web listener a passkey check
// made for this start, or, for a person with no passkey, a sign-in less
// than five minutes old, else the server says to sign in again. Then the
// page opens a WebSocket of its own for the operation, sends the session
// as its first message, and sends the keys the key table makes (keys.ts).
// The operation ends when the person switches it off, when the tab closes
// (the WebSocket closes with it), and when the server ends it, which says
// why; the tab is then read-only again.

import { useEffect, useState } from "react";
import type { HostKey } from "./keys";
import { type AssertionOptions, PasskeyError, passkeyAnswer } from "./passkeys";
import { authorized, sessionToken } from "./session";

const mayPath = "/api/operate/may";
const checkPath = "/api/operate/check";
const operatePath = "/api/operate";

// useMayOperate reports whether the server lets this page's person operate
// host panes. Until it has answered, and when it cannot be asked, no.
export function useMayOperate(): boolean {
  const [may, setMay] = useState(false);
  useEffect(() => {
    let gone = false;
    fetch(mayPath, authorized())
      .then((r) => (r.ok ? (r.json() as Promise<{ may?: unknown }>) : { may: false }))
      .then((a) => !gone && setMay(a.may === true))
      .catch(() => {});
    return () => {
      gone = true;
    };
  }, []);
  return may;
}

// Why an operation ended, or did not start. The first six are the
// server's reasons (control.HostEnded and the rest); the others are the
// page's own.
export type EndReason =
  | "ended" // switched off
  | "closed"
  | "idle"
  | "pane gone"
  | "hataori's own"
  | "stopping"
  | "refused" // the server refused the start; detail says why
  | "signIn" // the web listener needs a newer sign-in
  | "passkeyCancelled"
  | "failed" // the connection failed, or the check did
  | "lost"; // the connection closed without the server's reason

const serverReasons: readonly string[] = ["ended", "closed", "idle", "pane gone", "hataori's own", "stopping"];

// OpState is what the page shows of one tab's operation.
export interface OpState {
  pane: string;
  phase: "starting" | "on" | "ended";
  idle: number; // seconds without a key after which the server ends it
  lastKey: number; // when a key was last sent, or the start, in ms since the epoch
  reason?: EndReason;
  detail?: string; // the server's words for a refusal
}

// left is how long, in seconds, the operation has before its idle end. The
// page's clock ticks once a second, so now may be a little before the last
// key: never more than the idle period.
export function left(s: OpState, now: number): number {
  return Math.min(s.idle, Math.max(0, Math.ceil((s.idle * 1000 - (now - s.lastKey)) / 1000)));
}

// Operation is one operation of a pane, from the switch to its end.
export class Operation {
  private ws: WebSocket | null = null;
  private state: OpState;
  private done = false;
  private stopping = false; // switched off: no key goes after the end

  constructor(pane: string, private readonly onChange: (s: OpState) => void) {
    this.state = { pane, phase: "starting", idle: 0, lastKey: Date.now() };
    onChange(this.state);
    this.start();
  }

  private set(s: Partial<OpState>): void {
    if (this.done) return;
    this.state = { ...this.state, ...s };
    if (this.state.phase === "ended") this.done = true;
    this.onChange(this.state);
  }

  private end(reason: EndReason, detail?: string): void {
    this.set({ phase: "ended", reason, detail });
    this.ws?.close();
  }

  // start asks what the start needs, makes the passkey check when it is
  // asked for, and opens the WebSocket.
  private async start(): Promise<void> {
    let first: Record<string, unknown> = { session: sessionToken() ?? "", pane: this.state.pane };
    try {
      const r = await fetch(
        checkPath,
        authorized({ method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ pane: this.state.pane }) }),
      );
      if (r.status === 403 && location.protocol === "https:") return this.end("signIn");
      if (!r.ok) return this.end("refused", (await r.text()).trim());
      const a = (await r.json()) as { needs?: string; ceremony?: string; options?: { publicKey: AssertionOptions } };
      if (a.needs === "passkey" && a.ceremony && a.options) {
        const credential = await passkeyAnswer(a.options.publicKey);
        first = { ...first, ceremony: a.ceremony, credential };
      }
    } catch (e) {
      return this.end(e instanceof PasskeyError && e.why === "cancelled" ? "passkeyCancelled" : "failed");
    }
    if (this.done) return; // closed while the check ran
    const ws = new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}${operatePath}`);
    this.ws = ws;
    ws.onopen = () => ws.send(JSON.stringify(first));
    ws.onmessage = (m) => {
      let a: { started?: { idle?: number }; ended?: string; refused?: string };
      try {
        a = JSON.parse(String(m.data));
      } catch {
        return;
      }
      if (a.started) this.set({ phase: "on", idle: a.started.idle ?? 0, lastKey: Date.now() });
      else if (a.ended) this.end(serverReasons.includes(a.ended) ? (a.ended as EndReason) : "lost");
      else if (a.refused) this.end("refused", a.refused);
    };
    ws.onclose = (e) => {
      if (this.done) return;
      if (e.code === 1000 && serverReasons.includes(e.reason)) this.end(e.reason as EndReason);
      else if (e.code === 1008) this.end("refused", e.reason);
      else this.end("lost");
    };
    ws.onerror = () => {}; // onclose follows
  }

  // send gives the pane keys, in order. Nothing is sent before the
  // operation has started, nor after it ended.
  send(keys: HostKey[]): void {
    if (this.stopping || this.state.phase !== "on" || keys.length === 0 || this.ws?.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ keys }));
    this.set({ lastKey: Date.now() });
  }

  // stop is the switch turned off: the server ends the operation after the
  // keys sent before, and answers with the reason.
  stop(): void {
    if (this.done) return;
    this.stopping = true;
    if (this.ws?.readyState === WebSocket.OPEN && this.state.phase === "on") {
      this.ws.send(JSON.stringify({ end: true }));
      return;
    }
    this.end("ended");
  }

  // close is the tab closed: the WebSocket closes at once, which ends the
  // operation on the server.
  close(): void {
    this.done = true;
    this.ws?.close();
  }
}

// clock is a number of seconds as minutes and seconds, such as 4:05.
export function clock(seconds: number): string {
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}
