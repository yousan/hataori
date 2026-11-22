// The calls that give a thread input from its tab (design, section 7.2;
// internal/dashboard, act.go): send, answer, stop. On the web listener the
// server answers 428 until the session has made the check acting needs;
// the page then makes it (attach.ts, actCheck) and makes the call again,
// once.

import { actCheck, type CheckFailure } from "./attach";
import { authorized } from "./session";

export const sendPath = "/api/send";
export const answerPath = "/api/answer";
export const stopPath = "/api/stop";

// ActResult is how a call went: done, or refused with the server's words,
// or the check it needed failed.
export type ActResult = { ok: true } | { ok: false; status: number; reason: string; check?: CheckFailure };

async function post(path: string, body: unknown): Promise<Response> {
  return fetch(path, authorized({ method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }));
}

// act makes the call, with the check first when the server asks for it.
export async function act(path: string, body: unknown): Promise<ActResult> {
  try {
    let r = await post(path, body);
    if (r.status === 428) {
      const failed = await actCheck();
      if (failed) return { ok: false, status: 428, reason: failed.detail ?? "", check: failed.failure };
      r = await post(path, body);
    }
    if (r.ok) return { ok: true };
    return { ok: false, status: r.status, reason: (await r.text()).trim() };
  } catch {
    return { ok: false, status: 0, reason: "" };
  }
}

// newKey is a key of the page's own for one message, so that sending it
// again after an error gives the agent the message once.
export function newKey(): string {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}
