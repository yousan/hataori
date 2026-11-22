// Typing into a session's terminal from its tab: `hataori attach` in the
// browser (design, section 7.2, "Giving a thread input from its tab";
// internal/dashboard, attach.go and act.go).
//
// The page asks once whether its person may act at all; a person who may
// not gets a terminal that takes no key, and no switch. On the loopback
// listener the terminal takes keys when it has focus: the first key opens
// the attachment, a WebSocket of its own, and the keys typed while it opens
// are sent, in order, once it is open. It ends when the terminal loses
// focus, after the idle period without a key, and when the tab closes; the
// next key opens it again. On the web listener the tab has a switch: on, it
// makes the check acting needs (a passkey check, or nothing after a recent
// sign-in), then opens the attachment, which ends when the switch goes off,
// after the idle period, and when the tab closes.
//
// What the page sends is what xterm.js makes of the person's keys and
// pastes, the bytes a terminal sends; xterm.js draws the session's raw
// output, so it encodes each key for the modes the agent set. The page's
// terminal answers no query (muteQueries), and sends no mouse report and no
// focus report (reports.ts).

import type { Terminal } from "@xterm/xterm";
import { useEffect, useState } from "react";
import { type AssertionOptions, PasskeyError, passkeyAnswer } from "./passkeys";
import { mouseModes } from "./reports";
import { authorized, sessionToken } from "./session";

const mayPath = "/api/act/may";
const checkPath = "/api/act/check";
const verifyPath = "/api/act/verify";
const attachPath = "/api/attach";

// web reports whether the page came through the web listener, the one
// served over HTTPS; the loopback listener is plain HTTP.
export function web(): boolean {
  return location.protocol === "https:";
}

// useMayAct reports whether the server lets this page's person give
// threads input. Until it has answered, and when it cannot be asked, no.
export function useMayAct(): boolean {
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

// CheckFailure is why the check acting needs was not made.
export type CheckFailure = "signIn" | "passkeyCancelled" | "refused" | "failed";

// actCheck makes the check that acting needs on the listener the page came
// through: nothing on loopback; on the web listener a passkey check, or
// nothing for a person with no passkey right after a sign-in. It returns
// null when the page may act, else why not, with the server's words.
export async function actCheck(): Promise<{ failure: CheckFailure; detail?: string } | null> {
  const post = (path: string, body: unknown) =>
    fetch(path, authorized({ method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }));
  try {
    const r = await post(checkPath, {});
    if (!r.ok) {
      const why = (await r.text()).trim();
      return { failure: r.status === 403 && why.includes("sign in again") ? "signIn" : "refused", detail: why };
    }
    const a = (await r.json()) as { needs?: string; ceremony?: string; options?: { publicKey: AssertionOptions } };
    if (a.needs !== "passkey") return null;
    if (!a.ceremony || !a.options) return { failure: "failed" };
    const credential = await passkeyAnswer(a.options.publicKey);
    const v = await post(verifyPath, { ceremony: a.ceremony, credential });
    if (!v.ok) return { failure: "refused", detail: (await v.text()).trim() };
    return null;
  } catch (e) {
    return { failure: e instanceof PasskeyError && e.why === "cancelled" ? "passkeyCancelled" : "failed" };
  }
}

// Why an attachment ended, or did not start. The first six are the
// server's reasons (attach.go); the others are the page's own.
export type AttachEnd =
  | "ended" // switched off, or the terminal lost focus
  | "closed"
  | "idle"
  | "session ended"
  | "agent ended"
  | "stopping"
  | "refused" // the server refused the start; detail says why
  | CheckFailure
  | "lost"; // the connection closed without the server's reason

const serverReasons: readonly string[] = ["ended", "closed", "idle", "session ended", "agent ended", "stopping"];

// AttachState is what the page shows of one tab's attachment.
export interface AttachState {
  thread: number;
  phase: "starting" | "on" | "ended";
  idle: number; // seconds without a key after which the server ends it
  reason?: AttachEnd;
  detail?: string; // the server's words for a refusal
  error?: string; // the server's words for the last keys it could not write
}

// Attachment is one attachment of a session's terminal, from its start to
// its end.
export class Attachment {
  private ws: WebSocket | null = null;
  private state: AttachState;
  private done = false;
  private queued: string[] = []; // keys typed before it is on
  private stopWhenOn = false; // stopped while it started: the queued keys go first

  constructor(
    thread: number,
    private readonly onChange: (s: AttachState) => void,
  ) {
    this.state = { thread, phase: "starting", idle: 0 };
  }

  // live reports whether it has not ended.
  get live(): boolean {
    return !this.done;
  }

  // begin starts it: its first state is "starting".
  begin(): void {
    this.onChange(this.state);
    this.start().catch(() => this.end("failed"));
  }

  private set(s: Partial<AttachState>): void {
    if (this.done) return;
    this.state = { ...this.state, ...s };
    if (this.state.phase === "ended") this.done = true;
    this.onChange(this.state);
  }

  private end(reason: AttachEnd, detail?: string): void {
    this.queued = [];
    this.set({ phase: "ended", reason, detail, error: undefined });
    this.ws?.close();
  }

  private async start(): Promise<void> {
    const failed = await actCheck();
    if (failed) return this.end(failed.failure, failed.detail);
    if (this.done) return; // closed or switched off while the check ran
    let ws: WebSocket;
    try {
      ws = new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}${attachPath}`);
    } catch {
      return this.end("failed");
    }
    this.ws = ws;
    ws.onopen = () => ws.send(JSON.stringify({ session: sessionToken() ?? "", thread: this.state.thread }));
    ws.onmessage = (m) => {
      if (this.done) return;
      let a: { attached?: { idle?: number }; ended?: string; refused?: string; error?: string };
      try {
        a = JSON.parse(String(m.data));
      } catch {
        return;
      }
      if (a.attached) {
        this.set({ phase: "on", idle: a.attached.idle ?? 0 });
        // What was typed while it opened, in order.
        for (const keys of this.queued) ws.send(JSON.stringify({ keys }));
        this.queued = [];
        if (this.stopWhenOn) this.stop();
      } else if (a.ended) this.end(serverReasons.includes(a.ended) ? (a.ended as AttachEnd) : "lost");
      else if (a.refused) this.end("refused", a.refused);
      else if (a.error) this.set({ error: a.error });
    };
    ws.onclose = (e) => {
      if (this.done) return;
      if (e.code === 1000 && serverReasons.includes(e.reason)) this.end(e.reason as AttachEnd);
      else if (e.code === 1008) this.end("refused", e.reason);
      else this.end("lost");
    };
    ws.onerror = () => {}; // onclose follows
  }

  // send gives the session's terminal keys: at once while it is on, after
  // the start while it starts, and never after its end.
  send(keys: string): void {
    if (this.done || keys === "") return;
    if (this.state.phase === "starting") {
      this.queued.push(keys);
      return;
    }
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify({ keys }));
  }

  // stop ends it, after the keys sent before, also those typed while it
  // opened: the switch turned off, or the terminal left.
  stop(): void {
    if (this.done) return;
    if (this.state.phase === "starting" && this.queued.length > 0) {
      // Keys typed before it opened are sent first: it stops once on.
      this.stopWhenOn = true;
      return;
    }
    if (this.ws?.readyState === WebSocket.OPEN && this.state.phase === "on") {
      this.ws.send(JSON.stringify({ end: true }));
      this.set({ phase: "ended", reason: "ended" });
      const ws = this.ws;
      setTimeout(() => ws.close(), 15000); // the server's answer closes it first
      return;
    }
    this.end("ended");
  }

  // close is the tab closed: the WebSocket closes at once.
  close(): void {
    this.done = true;
    this.queued = [];
    this.ws?.close();
  }
}

// muteQueries makes term answer no terminal query, so that neither a
// replayed log nor the agent's own query types anything into the session:
// device attributes (primary, secondary, tertiary), status and cursor
// position reports, mode requests, the version request, setting requests,
// and colour queries; and it keeps the page's copy out of the modes that
// report the mouse or the focus. It returns what undoes it.
export function muteQueries(term: Terminal): () => void {
  const swallow = () => true;
  const p = term.parser;
  const off = [
    p.registerCsiHandler({ final: "c" }, swallow), // DA1
    p.registerCsiHandler({ prefix: ">", final: "c" }, swallow), // DA2
    p.registerCsiHandler({ prefix: "=", final: "c" }, swallow), // DA3
    p.registerCsiHandler({ final: "n" }, swallow), // DSR, CPR
    p.registerCsiHandler({ prefix: "?", final: "n" }, swallow), // DECDSR
    p.registerCsiHandler({ intermediates: "$", final: "p" }, swallow), // DECRQM
    p.registerCsiHandler({ prefix: "?", intermediates: "$", final: "p" }, swallow), // DECRQM, private
    p.registerCsiHandler({ prefix: ">", final: "q" }, swallow), // XTVERSION
    p.registerCsiHandler({ final: "x" }, swallow), // DECREQTPARM
    p.registerDcsHandler({ intermediates: "$", final: "q" }, swallow), // DECRQSS
    p.registerDcsHandler({ intermediates: "+", final: "q" }, swallow), // XTGETTCAP
    // A mode set that names only mouse and focus modes is not taken; one
    // that names others too is, and its reports are dropped (reports.ts).
    p.registerCsiHandler({ prefix: "?", final: "h" }, (params) => params.length > 0 && params.every((m) => typeof m === "number" && mouseModes.has(m))),
    ...[4, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19].map((n) => p.registerOscHandler(n, (data) => data.split(";").includes("?"))),
  ];
  return () => off.forEach((d) => d.dispose());
}
