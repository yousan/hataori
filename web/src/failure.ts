// failureReason is what the page shows for a session that failed: the
// state event's reason, and its detail when that is text for a person, as
// the chat front end posts it (internal/chat, ReadableDetail): lines with
// no other control character. A detail with escape codes or carriage
// returns is terminal output, such as the screen of a start that timed
// out; the events list shows it.

// maxDetail bounds the detail shown with the reason.
const maxDetail = 2000;

export function readableDetail(detail: unknown): string | undefined {
  if (typeof detail !== "string") return undefined;
  const d = detail.trim();
  // The same bound as the server's, which counts bytes.
  if (d === "" || new TextEncoder().encode(d).length > maxDetail) return undefined;
  for (const ch of d) {
    const c = ch.codePointAt(0) ?? 0;
    if (c !== 0x0a && (c < 0x20 || c === 0x7f || (c >= 0x80 && c < 0xa0))) return undefined;
  }
  return d;
}

// startFailure is the catalog's sentence for why a session failed as it
// started: by the state event's reason, and by whether a start-up dialog
// named files (trust).
export type StartFailure = "trust" | "blocked" | "gone" | "start" | "resume" | "takeIn" | "ended" | "other";

export function startFailure(reason: string | undefined, trust: readonly string[] | undefined): StartFailure {
  const r = reason ?? "";
  if (r === "blocked at start") return trust && trust.length > 0 ? "trust" : "blocked";
  if (r === "cannot resume" || r === "resumed another session") return "gone";
  if (r === "crashed") return "ended";
  if (r.startsWith("start: ")) return "start";
  if (r.startsWith("resume: ")) return "resume";
  if (r.startsWith("take in: ")) return "takeIn";
  return "other";
}

export function failureReason(data: Record<string, unknown> | undefined): string | undefined {
  const r = data?.reason;
  if (typeof r !== "string" || r === "") return undefined;
  const d = readableDetail(data?.detail);
  if (!d) return r;
  return d.includes("\n") ? `${r}.\n${d}` : `${r}: ${d}`;
}
